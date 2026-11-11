// Package e2e mounts onedriver against a fake Graph API server and exercises
// it through the kernel, so FUSE behaviour can be tested without a Microsoft
// account. Requires /dev/fuse and fusermount3; tests are skipped otherwise.
package e2e

import (
	"encoding/hex"
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
// item IDs are "id-" plus the hex-encoded path (see idOf).
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

	byID        map[string]string // item ID -> path, for items whose ID isn't idOf(path)
	uploadCount map[string]int    // path -> completed content uploads
	sessions    map[string]*fakeUploadSession
	nextID      int
	uploadDelay time.Duration // delay before content uploads are processed

	pageSize     int  // children are returned in pages of this size if > 0
	corruptPages int  // this many listing pages after the first are invalid JSON
	down         bool // drop every connection, as if the network were gone
	hang         bool // accept requests but never answer (TCP-level outage)

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
		files:       make(map[string][]byte),
		folders:     make(map[string]bool),
		ranges:      make(map[string]int),
		fulls:       make(map[string]int),
		uploads:     make(map[string][]byte),
		requests:    make(map[string]int),
		throttle:    make(map[string][]int),
		rejected:    make(map[string]bool),
		byID:        make(map[string]string),
		uploadCount: make(map[string]int),
		sessions:    make(map[string]*fakeUploadSession),
		modTime:     time.Now().Add(-time.Hour).UTC().Truncate(time.Second),
	}
}

// idOf returns the default item ID of a path, URL-safe like real OneDrive
// IDs (names can contain "#", "?", spaces...).
func idOf(path string) string { return "id-" + hex.EncodeToString([]byte(path)) }

func pathOf(id string) string {
	b, err := hex.DecodeString(strings.TrimPrefix(id, "id-"))
	if err != nil {
		return ""
	}
	return string(b)
}

type fakeUploadSession struct {
	path string
	data []byte
}

// idFor returns the item ID of a path: a reassigned ID (after a rename) if
// there is one, else idOf(path). Caller holds g.mu.
func (g *fakeGraph) idFor(path string) string {
	for id, p := range g.byID {
		if p == path {
			return id
		}
	}
	return idOf(path)
}

// pathFor returns the path of an item ID, "" if the ID is unknown (e.g. the
// item was replaced). Caller holds g.mu.
func (g *fakeGraph) pathFor(id string) string {
	if p, ok := g.byID[id]; ok {
		return p
	}
	p := pathOf(id)
	if g.idFor(p) != id {
		return "" // the path now belongs to another item
	}
	return p
}

// rename moves the item with id to newPath, replacing what was there and
// keeping the item's ID. Caller holds g.mu.
func (g *fakeGraph) rename(id, newPath string) bool {
	old := g.pathFor(id)
	data, ok := g.files[old]
	if old == "" || !ok {
		return false
	}
	if other := g.idFor(newPath); other != id {
		delete(g.byID, other)
	}
	delete(g.files, old)
	g.files[newPath] = data
	g.byID[id] = newPath
	return true
}

func (g *fakeGraph) uploadsOf(path string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.uploadCount[path]
}

func (g *fakeGraph) paths() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for p := range g.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (g *fakeGraph) content(path string) ([]byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	data, ok := g.files[path]
	return data, ok
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
		parentID, name = g.idFor(path[:i]), path[i+1:]
	}
	parent := &graph.DriveItemParent{ID: parentID, DriveType: graph.DriveTypePersonal}
	if path == trashName() || g.folders[path] {
		return graph.DriveItem{ID: g.idFor(path), Name: name, ModTime: &mt,
			Parent: parent, Folder: &graph.Folder{}}
	}
	data := g.files[path]
	return graph.DriveItem{
		ID: g.idFor(path), Name: name, Size: uint64(len(data)), ModTime: &mt,
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
	delay, hang := g.uploadDelay, g.hang
	g.mu.Unlock()
	if hang {
		<-r.Context().Done() // until the client gives up
		return
	}
	if delay > 0 && (r.Method == "PUT" || strings.HasSuffix(r.URL.Path, "/createUploadSession")) {
		time.Sleep(delay) // a slow upload, still running when the app renames
	}
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
	case r.Method == "POST" && strings.HasPrefix(p, items) && strings.HasSuffix(p, "/children"):
		// mkdir: {"name": ..., "folder": {}}
		id := strings.TrimSuffix(strings.TrimPrefix(p, items), "/children")
		var req graph.DriveItem
		json.NewDecoder(r.Body).Decode(&req)
		path := req.Name
		if id != rootID {
			path = g.pathFor(id) + "/" + req.Name
		}
		g.folders[path] = true
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, g.item(path))
	case strings.HasPrefix(p, items) && strings.HasSuffix(p, "/children"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, items), "/children")
		folder := ""
		if id != rootID {
			folder = g.pathFor(id)
			if !g.folders[folder] && folder != trashName() {
				notFound(w)
				return
			}
		}
		g.serveChildren(w, r, p, g.children(folder))
	case (r.Method == "PUT" && strings.HasSuffix(p, ":/content") ||
		r.Method == "POST" && strings.HasSuffix(p, ":/createUploadSession")) &&
		strings.HasPrefix(p, items) && strings.Contains(p, ":/"):
		// upload by name: /me/drive/items/{parent}:/{name}:/content (small)
		// or :/createUploadSession (large)
		rest := strings.TrimPrefix(p, items)
		rest = rest[:strings.LastIndex(rest, ":/")]
		parts := strings.SplitN(rest, ":/", 2)
		path := parts[1]
		if parts[0] != rootID {
			path = g.pathFor(parts[0]) + "/" + parts[1]
		}
		if r.Method == "POST" {
			g.createSession(w, r, path)
			return
		}
		g.storeUpload(w, r, path)
	case r.Method == "POST" && strings.HasPrefix(p, items) && strings.HasSuffix(p, "/createUploadSession"):
		path := g.pathFor(strings.TrimSuffix(strings.TrimPrefix(p, items), "/createUploadSession"))
		if _, ok := g.files[path]; !ok {
			notFound(w)
			return
		}
		g.createSession(w, r, path)
	case r.Method == "PUT" && strings.HasPrefix(p, items) && strings.HasSuffix(p, "/content"):
		path := g.pathFor(strings.TrimSuffix(strings.TrimPrefix(p, items), "/content"))
		if _, ok := g.files[path]; !ok {
			notFound(w) // uploading to an item that no longer exists
			return
		}
		g.storeUpload(w, r, path)
	case r.Method == "DELETE" && strings.HasPrefix(p, items):
		path := g.pathFor(strings.TrimPrefix(p, items))
		if _, ok := g.files[path]; !ok {
			notFound(w)
			return
		}
		delete(g.files, path)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == "PATCH" && strings.HasPrefix(p, items):
		// rename/move: {"name": ..., "parentReference": {"id": ...}}
		id := strings.TrimPrefix(p, items)
		var patch graph.DriveItem
		json.NewDecoder(r.Body).Decode(&patch)
		newPath := patch.Name
		if patch.Parent != nil && patch.Parent.ID != "" && patch.Parent.ID != rootID {
			newPath = g.pathFor(patch.Parent.ID) + "/" + patch.Name
		}
		if !g.rename(id, newPath) {
			notFound(w)
			return
		}
		writeJSON(w, g.item(newPath))
	case strings.HasPrefix(p, items) && strings.HasSuffix(p, "/content"):
		path := g.pathFor(strings.TrimSuffix(strings.TrimPrefix(p, items), "/content"))
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
	case strings.HasPrefix(p, items) && strings.Contains(p, ":/"):
		// GetItemChild: /me/drive/items/{parent}:/{name}
		parts := strings.SplitN(strings.TrimPrefix(p, items), ":/", 2)
		path := parts[1]
		if parts[0] != rootID {
			path = g.pathFor(parts[0]) + "/" + parts[1]
		}
		if _, ok := g.files[path]; !ok {
			notFound(w)
			return
		}
		writeJSON(w, g.item(path))
	case strings.HasPrefix(p, items):
		path := g.pathFor(strings.TrimPrefix(p, items))
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
	g.saveUpload(path, data)
	writeJSON(w, g.item(path))
}

// saveUpload stores uploaded content at path (a new item if nothing is
// there). Caller holds g.mu.
func (g *fakeGraph) saveUpload(path string, data []byte) {
	if _, exists := g.files[path]; !exists {
		if owner, used := g.byID[idOf(path)]; used && owner != path {
			// the default ID for this path belongs to an item that was
			// renamed away: like OneDrive, give the new item a fresh ID
			g.nextID++
			g.byID[fmt.Sprintf("id-new%d", g.nextID)] = path
		}
	}
	g.files[path] = data
	if i := strings.Index(path, "/"); i > 0 {
		g.folders[path[:i]] = true
	}
	g.uploads[path] = data
	g.uploadCount[path]++
}

// createSession starts a large-file upload session; chunks go to the
// returned (pre-authenticated) uploadUrl. Caller holds g.mu.
func (g *fakeGraph) createSession(w http.ResponseWriter, r *http.Request, path string) {
	g.nextID++
	sid := strconv.Itoa(g.nextID)
	g.sessions[sid] = &fakeUploadSession{path: path}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "bearer ")
	writeJSON(w, map[string]interface{}{
		"uploadUrl":          graph.GraphURL[:len(graph.GraphURL)-len("/v1.0")] + "/upload/" + token + "/" + sid,
		"expirationDateTime": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
}

// handleChunk receives one chunk of a large upload.
func (g *fakeGraph) handleChunk(w http.ResponseWriter, r *http.Request, sid string) {
	g.mu.Lock()
	delay := g.uploadDelay
	g.mu.Unlock()
	time.Sleep(delay)
	g.mu.Lock()
	defer g.mu.Unlock()
	session, ok := g.sessions[sid]
	if !ok {
		notFound(w)
		return
	}
	data, _ := io.ReadAll(r.Body)
	var start, end, total int
	fmt.Sscanf(r.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total)
	if start != len(session.data) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		w.Write([]byte(`{"error":{"code":"invalidRange","message":"bad range"}}`))
		return
	}
	session.data = append(session.data, data...)
	if len(session.data) < total {
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, map[string]interface{}{"nextExpectedRanges": []string{fmt.Sprintf("%d-", len(session.data))}})
		return
	}
	delete(g.sessions, sid)
	g.saveUpload(session.path, session.data)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, g.item(session.path))
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
	if strings.HasPrefix(req.URL.Path, "/upload/") {
		parts := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/upload/"), "/", 2)
		r.mu.Lock()
		g := r.graphs[parts[0]]
		r.mu.Unlock()
		if g == nil || len(parts) != 2 {
			http.Error(w, "gone", http.StatusServiceUnavailable)
			return
		}
		g.handleChunk(w, req, parts[1])
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
