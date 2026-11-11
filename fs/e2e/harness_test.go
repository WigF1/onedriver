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
	"sort"
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

// fakeGraph is a minimal in-memory Graph API: files in the root folder and
// in one level of subfolders. Paths look like "a.txt" or "Pictures/a.jpg";
// item IDs are "id-" plus the path with "/" replaced by "~".
type fakeGraph struct {
	mu       sync.Mutex
	files    map[string][]byte // path -> content
	folders  map[string]bool   // folder path (one level, no "/")
	ranges   map[string]int    // path -> head range requests
	fulls    map[string]int    // path -> full downloads
	uploads  map[string][]byte // path -> last uploaded content
	requests map[string]int    // request path -> count
	throttle map[string][]int  // path prefix -> Retry-After seconds to answer 429 with, in order
	deltas   []graph.DriveItem // returned by the next delta poll
	rejected map[string]bool   // access tokens answered with HTTP 401
	modTime  time.Time

	pageSize     int  // children are returned in pages of this size if > 0
	corruptPages int  // this many listing pages after the first are invalid JSON
	down         bool // drop every connection, as if the network were gone

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
		folders:  make(map[string]bool),
		ranges:   make(map[string]int),
		fulls:    make(map[string]int),
		uploads:  make(map[string][]byte),
		requests: make(map[string]int),
		throttle: make(map[string][]int),
		rejected: make(map[string]bool),
		modTime:  time.Now().Add(-time.Hour).UTC().Truncate(time.Second),
	}
}

func idOf(path string) string { return "id-" + strings.ReplaceAll(path, "/", "~") }

func pathOf(id string) string {
	return strings.ReplaceAll(strings.TrimPrefix(id, "id-"), "~", "/")
}

// add creates a file; a path "Folder/name" also creates the folder.
func (g *fakeGraph) add(path string, content []byte) {
	g.mu.Lock()
	g.files[path] = content
	if i := strings.Index(path, "/"); i > 0 {
		g.folders[path[:i]] = true
	}
	g.mu.Unlock()
}

func (g *fakeGraph) set(f func(g *fakeGraph)) {
	g.mu.Lock()
	f(g)
	g.mu.Unlock()
}

// remove deletes a file on the "server" without sending a delta.
func (g *fakeGraph) remove(path string) {
	g.mu.Lock()
	delete(g.files, path)
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

func (g *fakeGraph) counts(path string) (ranges, fulls int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ranges[path], g.fulls[path]
}

func (g *fakeGraph) requestCount(path string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests[path]
}

func (g *fakeGraph) uploaded(path string) ([]byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	data, ok := g.uploads[path]
	return data, ok
}

// item returns the DriveItem for a path ("" is the root). Caller holds g.mu.
func (g *fakeGraph) item(path string) graph.DriveItem {
	mt := g.modTime
	if path == "" {
		return graph.DriveItem{ID: rootID, Name: "root", ModTime: &mt,
			Parent: &graph.DriveItemParent{DriveID: "drive", DriveType: graph.DriveTypePersonal},
			Folder: &graph.Folder{ChildCount: uint32(len(g.files))}}
	}
	parentID, name := rootID, path
	if i := strings.LastIndex(path, "/"); i > 0 {
		parentID, name = idOf(path[:i]), path[i+1:]
	}
	parent := &graph.DriveItemParent{ID: parentID, DriveType: graph.DriveTypePersonal}
	if path == trashName() || g.folders[path] {
		return graph.DriveItem{ID: idOf(path), Name: name, ModTime: &mt,
			Parent: parent, Folder: &graph.Folder{}}
	}
	data := g.files[path]
	return graph.DriveItem{
		ID: idOf(path), Name: name, Size: uint64(len(data)), ModTime: &mt,
		Parent: parent, ETag: "etag-" + path + "-" + strconv.Itoa(len(data)),
		File: &graph.File{Hashes: graph.Hashes{QuickXorHash: graph.QuickXORHash(&data)}},
	}
}

// children lists a folder ("" is the root), sorted. Caller holds g.mu.
func (g *fakeGraph) children(folder string) []graph.DriveItem {
	var paths []string
	if folder == "" {
		paths = append(paths, trashName())
		for f := range g.folders {
			paths = append(paths, f)
		}
	}
	for p := range g.files {
		dir := ""
		if i := strings.LastIndex(p, "/"); i > 0 {
			dir = p[:i]
		}
		if dir == folder {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	items := make([]graph.DriveItem, 0, len(paths))
	for _, p := range paths {
		items = append(items, g.item(p))
	}
	return items
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
	if g.down {
		// close the connection without an HTTP response: looks like no network
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
	}
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

	const items = "/me/drive/items/"
	switch {
	case p == "/me/drive":
		writeJSON(w, graph.Drive{ID: "drive", DriveType: graph.DriveTypePersonal,
			Quota: graph.DriveQuota{Total: 1 << 40, Remaining: 1 << 39}})
	case p == "/me/drive/root" || p == items+rootID:
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
	case strings.HasPrefix(p, items) && strings.HasSuffix(p, "/children"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, items), "/children")
		folder := ""
		if id != rootID {
			folder = pathOf(id)
			if !g.folders[folder] && folder != trashName() {
				notFound(w)
				return
			}
		}
		g.serveChildren(w, r, p, g.children(folder))
	case r.Method == "PUT" && strings.HasPrefix(p, items) && strings.Contains(p, ":/") &&
		strings.HasSuffix(p, ":/content"):
		// small upload of a new file: /me/drive/items/{parent}:/{name}:/content
		rest := strings.TrimSuffix(strings.TrimPrefix(p, items), ":/content")
		parts := strings.SplitN(rest, ":/", 2)
		path := parts[1]
		if parts[0] != rootID {
			path = pathOf(parts[0]) + "/" + parts[1]
		}
		g.storeUpload(w, r, path)
	case r.Method == "PUT" && strings.HasPrefix(p, items+"id-") && strings.HasSuffix(p, "/content"):
		g.storeUpload(w, r, pathOf(strings.TrimSuffix(strings.TrimPrefix(p, items), "/content")))
	case strings.HasPrefix(p, items+"id-") && strings.HasSuffix(p, "/content"):
		path := pathOf(strings.TrimSuffix(strings.TrimPrefix(p, items), "/content"))
		data, ok := g.files[path]
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
				g.ranges[path]++
			} else if start == 0 {
				g.fulls[path]++ // first chunk of a multipart full download
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data[start : end+1])
			return
		}
		g.fulls[path]++
		w.Write(data)
	case strings.HasPrefix(p, items+"id-"):
		path := pathOf(strings.TrimPrefix(p, items))
		if _, ok := g.files[path]; !ok && path != trashName() && !g.folders[path] {
			notFound(w)
			return
		}
		writeJSON(w, g.item(path))
	default:
		http.Error(w, `{"error":{"code":"notImplemented","message":"`+p+`"}}`, 501)
	}
}

// serveChildren writes one page of a listing, with an @odata.nextLink when
// paging is enabled. Caller holds g.mu.
func (g *fakeGraph) serveChildren(w http.ResponseWriter, r *http.Request, p string, all []graph.DriveItem) {
	if g.pageSize <= 0 {
		writeJSON(w, map[string]interface{}{"value": all})
		return
	}
	skip, _ := strconv.Atoi(r.URL.Query().Get("$skiptoken"))
	if skip > 0 && g.corruptPages > 0 {
		g.corruptPages--
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"value": [ {"id": "trunc`))
		return
	}
	end := skip + g.pageSize
	if end > len(all) {
		end = len(all)
	}
	page := map[string]interface{}{"value": all[skip:end]}
	if end < len(all) {
		page["@odata.nextLink"] = graph.GraphURL + p + "?$skiptoken=" + strconv.Itoa(end)
	}
	writeJSON(w, page)
}

func (g *fakeGraph) storeUpload(w http.ResponseWriter, r *http.Request, path string) {
	data, _ := io.ReadAll(r.Body)
	g.files[path] = data
	g.uploads[path] = data
	writeJSON(w, g.item(path))
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
