// Package e2e mounts onedriver against a fake Graph API server and exercises
// it through the kernel, so FUSE behaviour can be tested without a Microsoft
// account. Requires /dev/fuse and fusermount3; tests are skipped otherwise.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// fakeGraph is a minimal in-memory Graph API serving one folder of files.
type fakeGraph struct {
	mu      sync.Mutex
	files   map[string][]byte // name -> content
	ranges  map[string]int    // name -> range requests (excluding full multipart chunks)
	fulls   map[string]int    // name -> full downloads
	modTime time.Time
}

const rootID = "ROOT"

func newFakeGraph() *fakeGraph {
	return &fakeGraph{
		files:   make(map[string][]byte),
		ranges:  make(map[string]int),
		fulls:   make(map[string]int),
		modTime: time.Now().Add(-time.Hour).UTC().Truncate(time.Second),
	}
}

func (g *fakeGraph) add(name string, content []byte) {
	g.mu.Lock()
	g.files[name] = content
	g.mu.Unlock()
}

func (g *fakeGraph) counts(name string) (ranges, fulls int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ranges[name], g.fulls[name]
}

func (g *fakeGraph) item(name string) graph.DriveItem {
	mt := g.modTime
	parent := &graph.DriveItemParent{ID: rootID, DriveType: graph.DriveTypePersonal}
	if name == "" {
		return graph.DriveItem{ID: rootID, Name: "root", ModTime: &mt,
			Folder: &graph.Folder{ChildCount: uint32(len(g.files))}}
	}
	if name == trashName() {
		return graph.DriveItem{ID: "id-" + name, Name: name, ModTime: &mt,
			Parent: parent, Folder: &graph.Folder{}}
	}
	data := g.files[name]
	return graph.DriveItem{
		ID: "id-" + name, Name: name, Size: uint64(len(data)), ModTime: &mt,
		Parent: parent, ETag: "etag-" + name,
		File: &graph.File{Hashes: graph.Hashes{QuickXorHash: graph.QuickXORHash(&data)}},
	}
}

func trashName() string { return fmt.Sprintf(".Trash-%d", os.Getuid()) }

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (g *fakeGraph) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v1.0")
	switch {
	case p == "/me/drive":
		writeJSON(w, graph.Drive{ID: "drive", DriveType: graph.DriveTypePersonal,
			Quota: graph.DriveQuota{Total: 1 << 40, Remaining: 1 << 39}})
	case p == "/me/drive/root" || p == "/me/drive/items/"+rootID:
		writeJSON(w, g.item(""))
	case strings.HasPrefix(p, "/me/drive/root/delta"):
		writeJSON(w, map[string]interface{}{
			"@odata.deltaLink": graph.GraphURL + "/me/drive/root/delta?token=next",
			"value":            []interface{}{},
		})
	case p == "/me/drive/items/"+rootID+"/children":
		children := []graph.DriveItem{g.item(trashName())}
		for name := range g.files {
			children = append(children, g.item(name))
		}
		writeJSON(w, map[string]interface{}{"value": children})
	case strings.HasPrefix(p, "/me/drive/items/id-") && strings.HasSuffix(p, "/content"):
		name := strings.TrimSuffix(strings.TrimPrefix(p, "/me/drive/items/id-"), "/content")
		data, ok := g.files[name]
		if !ok {
			http.NotFound(w, r)
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
			http.NotFound(w, r)
			return
		}
		writeJSON(w, g.item(name))
	default:
		http.Error(w, `{"error":{"code":"notImplemented","message":"`+p+`"}}`, 501)
	}
}

// mount starts a fake Graph server and mounts onedriver against it.
func mount(t *testing.T, g *fakeGraph) string {
	t.Helper()
	if _, err := exec.LookPath("fusermount3"); err != nil {
		t.Skip("fusermount3 not available")
	}
	if f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0); err != nil {
		t.Skip("/dev/fuse not accessible")
	} else {
		f.Close()
	}

	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	oldURL := graph.GraphURL
	graph.GraphURL = srv.URL + "/v1.0"
	t.Cleanup(func() { graph.GraphURL = oldURL })

	dir := t.TempDir()
	mnt := filepath.Join(dir, "mount")
	os.Mkdir(mnt, 0755)
	auth := &graph.Auth{AccessToken: "fake", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	filesystem := fs.NewFilesystem(auth, filepath.Join(dir, "cache"))
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
	return mnt
}

func testData(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed
	}
	return b
}

func readAt(t *testing.T, path string, off int64, n int) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, off)
	if err != nil && got != n {
		t.Fatalf("read %s at %d: %v", path, off, err)
	}
	return buf[:got]
}

func TestSniffReadUsesRangeRequest(t *testing.T) {
	g := newFakeGraph()
	big := testData(5*partial.HeadSize, 1)
	g.add("big.bin", big)
	mnt := mount(t, g)

	start := time.Now()
	got := readAt(t, filepath.Join(mnt, "big.bin"), 0, 4096)
	if !bytes.Equal(got, big[:4096]) {
		t.Fatal("sniff read returned wrong data")
	}
	ranges, fulls := g.counts("big.bin")
	if ranges != 1 || fulls != 0 {
		t.Fatalf("got %d range / %d full downloads, want 1 / 0", ranges, fulls)
	}
	t.Logf("sniff read took %v", time.Since(start))

	// a second sniff in a new open is served without another request
	readAt(t, filepath.Join(mnt, "big.bin"), 100, 4096)
	if r, f := g.counts("big.bin"); r != 1 || f != 0 {
		t.Fatalf("second sniff made requests: %d range / %d full", r, f)
	}
}

func TestFullReadDownloadsAndCaches(t *testing.T) {
	g := newFakeGraph()
	big := testData(5*partial.HeadSize+123, 2)
	g.add("big.bin", big)
	mnt := mount(t, g)
	path := filepath.Join(mnt, "big.bin")

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, big) {
		t.Fatal("full read returned wrong data")
	}
	_, fulls := g.counts("big.bin")
	if fulls != 1 {
		t.Fatalf("got %d full downloads, want 1", fulls)
	}

	// reopening is served from the cache
	got, _ = os.ReadFile(path)
	if !bytes.Equal(got, big) {
		t.Fatal("cached read returned wrong data")
	}
	if _, f := g.counts("big.bin"); f != 1 {
		t.Fatalf("reopen downloaded again (%d full downloads)", f)
	}
}

func TestReadCrossingHeadBoundaryDownloadsFull(t *testing.T) {
	g := newFakeGraph()
	big := testData(3*partial.HeadSize, 3)
	g.add("big.bin", big)
	mnt := mount(t, g)

	off := int64(partial.HeadSize - 100)
	got := readAt(t, filepath.Join(mnt, "big.bin"), off, 1000)
	if !bytes.Equal(got, big[off:off+1000]) {
		t.Fatal("boundary read returned wrong data")
	}
	if _, f := g.counts("big.bin"); f != 1 {
		t.Fatalf("got %d full downloads, want 1", f)
	}
}

func TestSmallFileDownloadsFull(t *testing.T) {
	g := newFakeGraph()
	small := []byte("hello, onedriver\n")
	g.add("small.txt", small)
	mnt := mount(t, g)

	got, err := os.ReadFile(filepath.Join(mnt, "small.txt"))
	if err != nil || !bytes.Equal(got, small) {
		t.Fatalf("got %q, %v", got, err)
	}
	if r, f := g.counts("small.txt"); r != 0 || f != 1 {
		t.Fatalf("got %d range / %d full downloads, want 0 / 1", r, f)
	}
}

func TestOpenWithoutReadDownloadsNothing(t *testing.T) {
	g := newFakeGraph()
	g.add("big.bin", testData(4*partial.HeadSize, 4))
	mnt := mount(t, g)

	f, err := os.Open(filepath.Join(mnt, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if r, fu := g.counts("big.bin"); r != 0 || fu != 0 {
		t.Fatalf("open+close made %d range / %d full downloads, want none", r, fu)
	}
}

func TestReadWriteOpenDownloadsFirst(t *testing.T) {
	g := newFakeGraph()
	big := testData(3*partial.HeadSize, 5)
	g.add("big.bin", big)
	mnt := mount(t, g)

	f, err := os.OpenFile(filepath.Join(mnt, "big.bin"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, fu := g.counts("big.bin"); fu != 1 {
		t.Fatalf("read-write open made %d full downloads, want 1", fu)
	}
	buf := make([]byte, 4096)
	f.ReadAt(buf, 0)
	f.Close() // no writes, so nothing is uploaded
	if !bytes.Equal(buf, big[:4096]) {
		t.Fatal("read after read-write open returned wrong data")
	}
}

func TestSniffThenFullRead(t *testing.T) {
	g := newFakeGraph()
	big := testData(4*partial.HeadSize, 6)
	g.add("big.bin", big)
	mnt := mount(t, g)
	path := filepath.Join(mnt, "big.bin")

	readAt(t, path, 0, 4096)
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("full read after sniff wrong: %v", err)
	}
	if r, f := g.counts("big.bin"); r != 1 || f != 1 {
		t.Fatalf("got %d range / %d full downloads, want 1 / 1", r, f)
	}
}

func TestConcurrentSniffsShareOneRequest(t *testing.T) {
	g := newFakeGraph()
	big := testData(4*partial.HeadSize, 7)
	g.add("big.bin", big)
	mnt := mount(t, g)
	path := filepath.Join(mnt, "big.bin")

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64(i * 4096)
			if got := readAt(t, path, off, 2048); !bytes.Equal(got, big[off:off+2048]) {
				t.Errorf("reader %d got wrong data", i)
			}
		}(i)
	}
	wg.Wait()
	if r, f := g.counts("big.bin"); r != 1 || f != 0 {
		t.Fatalf("got %d range / %d full downloads, want 1 / 0", r, f)
	}
}

func TestManyFilesSniffQuickly(t *testing.T) {
	// what a file manager does when opening a folder: sniff every file
	g := newFakeGraph()
	for i := 0; i < 20; i++ {
		g.add("file"+strconv.Itoa(i)+".dat", testData(3*partial.HeadSize, byte(i)))
	}
	mnt := mount(t, g)
	for i := 0; i < 20; i++ {
		name := "file" + strconv.Itoa(i) + ".dat"
		readAt(t, filepath.Join(mnt, name), 0, 4096)
		if _, f := g.counts(name); f != 0 {
			t.Fatalf("%s was downloaded in full", name)
		}
	}
}
