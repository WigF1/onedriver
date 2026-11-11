// Package e2e mounts onedriver against a fake Graph API server and exercises
// it through the kernel, so FUSE behaviour can be tested without a Microsoft
// account. Requires /dev/fuse and fusermount3; tests are skipped otherwise.
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/jstaf/onedriver/fs"
	"github.com/jstaf/onedriver/fs/graph"
	"github.com/jstaf/onedriver/fs/partial"
)

const rootID = "ROOT"

// fakeGraph is a minimal in-memory Graph API serving one folder of files.
type fakeGraph struct {
	mu       sync.Mutex
	files    map[string][]byte // name -> content
	ranges   map[string]int    // name -> head range requests
	fulls    map[string]int    // name -> full downloads
	uploads  map[string][]byte // name -> last uploaded content
	requests map[string]int    // request path -> count
	throttle map[string][]int  // path prefix -> Retry-After seconds to answer 429 with, in order
	deltas   []graph.DriveItem // returned by the next delta poll
	rejected map[string]bool   // access tokens answered with HTTP 401
	modTime  time.Time

	// token endpoint behaviour: "" (issue new tokens), "500" or "invalid_grant"
	tokenMode  string
	tokenDelay time.Duration
	tokenPosts int
}

func (g *fakeGraph) setTokenMode(mode string, delay time.Duration) {
	g.mu.Lock()
	g.tokenMode, g.tokenDelay = mode, delay
	g.mu.Unlock()
}

func (g *fakeGraph) reject(token string) {
	g.mu.Lock()
	g.rejected[token] = true
	g.mu.Unlock()
}

func (g *fakeGraph) posts() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.tokenPosts
}

func newFakeGraph() *fakeGraph {
	return &fakeGraph{
		files:    make(map[string][]byte),
		ranges:   make(map[string]int),
		fulls:    make(map[string]int),
		uploads:  make(map[string][]byte),
		requests: make(map[string]int),
		throttle: make(map[string][]int),
		rejected: make(map[string]bool),
		modTime:  time.Now().Add(-time.Hour).UTC().Truncate(time.Second),
	}
}

func (g *fakeGraph) add(name string, content []byte) {
	g.mu.Lock()
	g.files[name] = content
	g.mu.Unlock()
}

// remove deletes a file on the "server" without sending a delta.
func (g *fakeGraph) remove(name string) {
	g.mu.Lock()
	delete(g.files, name)
	g.mu.Unlock()
}

// queueDelta makes the next delta poll return item.
func (g *fakeGraph) queueDelta(item graph.DriveItem) {
	g.mu.Lock()
	g.deltas = append(g.deltas, item)
	g.mu.Unlock()
}

// throttleNext makes the next requests whose path starts with prefix fail
// with HTTP 429 and the given Retry-After values (in seconds).
func (g *fakeGraph) throttleNext(prefix string, retryAfter ...int) {
	g.mu.Lock()
	g.throttle[prefix] = append(g.throttle[prefix], retryAfter...)
	g.mu.Unlock()
}

func (g *fakeGraph) counts(name string) (ranges, fulls int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ranges[name], g.fulls[name]
}

func (g *fakeGraph) requestCount(path string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests[path]
}

func (g *fakeGraph) uploaded(name string) ([]byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	data, ok := g.uploads[name]
	return data, ok
}

func (g *fakeGraph) item(name string) graph.DriveItem {
	mt := g.modTime
	parent := &graph.DriveItemParent{ID: rootID, DriveType: graph.DriveTypePersonal}
	if name == "" {
		return graph.DriveItem{ID: rootID, Name: "root", ModTime: &mt,
			Parent: &graph.DriveItemParent{DriveID: "drive", DriveType: graph.DriveTypePersonal},
			Folder: &graph.Folder{ChildCount: uint32(len(g.files))}}
	}
	if name == trashName() {
		return graph.DriveItem{ID: "id-" + name, Name: name, ModTime: &mt,
			Parent: parent, Folder: &graph.Folder{}}
	}
	data := g.files[name]
	return graph.DriveItem{
		ID: "id-" + name, Name: name, Size: uint64(len(data)), ModTime: &mt,
		Parent: parent, ETag: "etag-" + name + "-" + strconv.Itoa(len(data)),
		File: &graph.File{Hashes: graph.Hashes{QuickXorHash: graph.QuickXORHash(&data)}},
	}
}

func trashName() string { return fmt.Sprintf(".Trash-%d", os.Getuid()) }

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(`{"error":{"code":"itemNotFound","message":"Item not found"}}`))
}

func (g *fakeGraph) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v1.0")
	g.requests[p]++
	if g.rejected[strings.TrimPrefix(r.Header.Get("Authorization"), "bearer ")] {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":"InvalidAuthenticationToken","message":"expired"}}`))
		return
	}

	for prefix, waits := range g.throttle {
		if strings.HasPrefix(p, prefix) && len(waits) > 0 {
			g.throttle[prefix] = waits[1:]
			w.Header().Set("Retry-After", strconv.Itoa(waits[0]))
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"code":"activityLimitReached","message":"throttled"}}`))
			return
		}
	}

	switch {
	case p == "/me/drive":
		writeJSON(w, graph.Drive{ID: "drive", DriveType: graph.DriveTypePersonal,
			Quota: graph.DriveQuota{Total: 1 << 40, Remaining: 1 << 39}})
	case p == "/me/drive/root" || p == "/me/drive/items/"+rootID:
		writeJSON(w, g.item(""))
	case strings.HasPrefix(p, "/me/drive/root/delta"):
		deltas := g.deltas
		g.deltas = nil
		if deltas == nil {
			deltas = []graph.DriveItem{}
		}
		writeJSON(w, map[string]interface{}{
			"@odata.deltaLink": graph.GraphURL + "/me/drive/root/delta?token=next",
			"value":            deltas,
		})
	case p == "/me/drive/items/"+rootID+"/children":
		children := []graph.DriveItem{g.item(trashName())}
		for name := range g.files {
			children = append(children, g.item(name))
		}
		writeJSON(w, map[string]interface{}{"value": children})
	case r.Method == "PUT" && strings.HasPrefix(p, "/me/drive/items/"+rootID+":/"):
		// small upload of a new file: /me/drive/items/{parent}:/{name}:/content
		name := strings.TrimSuffix(strings.TrimPrefix(p, "/me/drive/items/"+rootID+":/"), ":/content")
		g.storeUpload(w, r, name)
	case r.Method == "PUT" && strings.HasPrefix(p, "/me/drive/items/id-") && strings.HasSuffix(p, "/content"):
		name := strings.TrimSuffix(strings.TrimPrefix(p, "/me/drive/items/id-"), "/content")
		g.storeUpload(w, r, name)
	case strings.HasPrefix(p, "/me/drive/items/id-") && strings.HasSuffix(p, "/content"):
		name := strings.TrimSuffix(strings.TrimPrefix(p, "/me/drive/items/id-"), "/content")
		data, ok := g.files[name]
		if !ok {
			notFound(w)
			return
		}
		if rng := r.Header.Get("Range"); rng != "" {
			var start, end int
			fmt.Sscanf(rng, "bytes=%d-%d", &start, &end)
			if end >= len(data) {
				end = len(data) - 1
			}
			if start == 0 && end < len(data)-1 && end+1 == partial.HeadSize {
				g.ranges[name]++
			} else if start == 0 {
				g.fulls[name]++ // first chunk of a multipart full download
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data[start : end+1])
			return
		}
		g.fulls[name]++
		w.Write(data)
	case strings.HasPrefix(p, "/me/drive/items/id-"):
		name := strings.TrimPrefix(p, "/me/drive/items/id-")
		if _, ok := g.files[name]; !ok && name != trashName() {
			notFound(w)
			return
		}
		writeJSON(w, g.item(name))
	default:
		http.Error(w, `{"error":{"code":"notImplemented","message":"`+p+`"}}`, 501)
	}
}

func (g *fakeGraph) storeUpload(w http.ResponseWriter, r *http.Request, name string) {
	data, _ := io.ReadAll(r.Body)
	g.files[name] = data
	g.uploads[name] = data
	writeJSON(w, g.item(name))
}

// fakeRouter is the single Graph endpoint for the whole test binary, so
// graph.GraphURL is set once and never changes while background loops of
// earlier mounts are still running. Each mount gets its own access token,
// which selects its fakeGraph; requests with a retired token get HTTP 503
// (never 401, which would start an interactive login).
type fakeRouter struct {
	mu     sync.Mutex
	next   int
	graphs map[string]*fakeGraph
}

var router = &fakeRouter{graphs: make(map[string]*fakeGraph)}

func (r *fakeRouter) register(g *fakeGraph) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	token := fmt.Sprintf("fake-token-%d", r.next)
	r.graphs[token] = g
	return token
}

func (r *fakeRouter) unregister(token string) {
	r.mu.Lock()
	delete(r.graphs, token)
	r.mu.Unlock()
}

// tokenURL is the fake OAuth token endpoint for the fakeGraph that the
// access token belongs to.
func (r *fakeRouter) tokenURL(token string) string {
	return graph.GraphURL[:len(graph.GraphURL)-len("/v1.0")] + "/oauth/" + token + "/token"
}

// handleToken serves the OAuth token endpoint: refresh_token grants get a new
// access + refresh token pair registered to the same fakeGraph.
func (r *fakeRouter) handleToken(w http.ResponseWriter, req *http.Request) {
	key := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/oauth/"), "/token")
	r.mu.Lock()
	g := r.graphs[key]
	r.mu.Unlock()
	if g == nil {
		http.Error(w, "unknown", http.StatusServiceUnavailable)
		return
	}
	req.ParseForm()
	g.mu.Lock()
	g.tokenPosts++
	mode, delay := g.tokenMode, g.tokenDelay
	g.mu.Unlock()
	time.Sleep(delay)
	switch mode {
	case "500":
		http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
		return
	case "invalid_grant":
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"AADSTS70000: grant expired"}`))
		return
	}
	access := r.register(g)
	writeJSON(w, map[string]interface{}{
		"access_token":  access,
		"refresh_token": "refresh-for-" + access,
		"expires_in":    3600,
	})
}

func (r *fakeRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if strings.HasPrefix(req.URL.Path, "/oauth/") {
		r.handleToken(w, req)
		return
	}
	token := strings.TrimPrefix(req.Header.Get("Authorization"), "bearer ")
	r.mu.Lock()
	g := r.graphs[token]
	r.mu.Unlock()
	if g == nil {
		http.Error(w, `{"error":{"code":"gone","message":"test finished"}}`, http.StatusServiceUnavailable)
		return
	}
	g.ServeHTTP(w, req)
}

// startRouter points the graph package at the fake router. Called once from
// TestMain.
func startRouter() func() {
	srv := httptest.NewServer(router)
	graph.GraphURL = srv.URL + "/v1.0"
	return srv.Close
}

type options struct {
	deltaInterval time.Duration // start the delta loop if non-zero
}

type mounted struct {
	dir       string
	fs        *fs.Filesystem
	auth      *graph.Auth
	tokenFile string
	token     string // the initial access token
}

// mount starts a fake Graph server and mounts onedriver against it.
func mount(t *testing.T, g *fakeGraph, opts options) mounted {
	t.Helper()
	if _, err := exec.LookPath("fusermount3"); err != nil {
		t.Skip("fusermount3 not available")
	}
	if f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0); err != nil {
		t.Skip("/dev/fuse not accessible")
	} else {
		f.Close()
	}

	token := router.register(g)
	t.Cleanup(func() { router.unregister(token) })

	dir := t.TempDir()
	mnt := filepath.Join(dir, "mount")
	os.Mkdir(mnt, 0755)
	auth := &graph.Auth{
		AuthConfig: graph.AuthConfig{
			ClientID: "test", RedirectURL: "https://example.invalid/redirect",
			TokenURL: router.tokenURL(token), CodeURL: "https://example.invalid/code",
		},
		AccessToken:  token,
		RefreshToken: "refresh-for-" + token,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}
	tokenFile := filepath.Join(dir, "auth_tokens.json")
	if err := auth.ToFile(tokenFile); err != nil {
		t.Fatal(err)
	}
	filesystem := fs.NewFilesystem(auth, filepath.Join(dir, "cache"))
	if opts.deltaInterval > 0 {
		go filesystem.DeltaLoop(opts.deltaInterval)
	}
	server, err := fuse.NewServer(filesystem, mnt, &fuse.MountOptions{
		Name: "onedriver", FsName: "onedriver-test", DisableXAttrs: true, MaxBackground: 64,
	})
	if err != nil {
		t.Fatalf("mount failed: %v", err)
	}
	go server.Serve()
	if err := server.WaitMount(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Unmount(); err != nil {
			exec.Command("fusermount3", "-uz", mnt).Run()
		}
	})
	return mounted{dir: mnt, fs: filesystem, auth: auth, tokenFile: tokenFile, token: token}
}

// eventually polls cond until it returns true or the timeout passes.
func eventually(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out: " + msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func testData(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed
	}
	return b
}
