package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/jstaf/onedriver/fs/graph"
)

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// #466: a file created remotely in a folder whose listing was never loaded
// must not turn that folder's listing into just that one file.
func TestNewRemoteFileInUnlistedFolderKeepsListingComplete(t *testing.T) {
	g := newFakeGraph()
	g.add("Pictures/a.jpg", []byte("a"))
	g.add("Pictures/b.jpg", []byte("b"))
	g.add("Pictures/c.jpg", []byte("c"))
	m := mount(t, g, options{deltaInterval: 100 * time.Millisecond})
	names(t, m.dir) // lists the root: Pictures is known, its contents are not

	g.add("Pictures/new.jpg", []byte("new"))
	g.set(func(g *fakeGraph) { g.deltas = append(g.deltas, g.item("Pictures/new.jpg")) })
	eventually(t, 5*time.Second, "delta to be applied", func() bool {
		return g.requestCount("/me/drive/root/delta") > 2
	})

	got := names(t, filepath.Join(m.dir, "Pictures"))
	want := []string{"a.jpg", "b.jpg", "c.jpg", "new.jpg"}
	if len(got) != len(want) {
		t.Fatalf("Pictures lists %v, want %v", got, want)
	}
}

// #466: a listing page that fails to decode must not be cached as the
// complete listing.
func TestBadListingPageIsNotCachedAsComplete(t *testing.T) {
	g := newFakeGraph()
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		g.add("Docs/"+n+".txt", []byte(n))
	}
	m := mount(t, g, options{})
	names(t, m.dir)
	g.set(func(g *fakeGraph) { g.pageSize, g.corruptPages = 3, 1 })

	if got, err := os.ReadDir(filepath.Join(m.dir, "Docs")); err == nil {
		t.Fatalf("listing with a bad page succeeded with %d entries, want an error", len(got))
	}
	if got := names(t, filepath.Join(m.dir, "Docs")); len(got) != 7 {
		t.Fatalf("Docs lists %d entries after a bad page, want 7: %v", len(got), got)
	}
}

// #201: writing to a file updates its modification time.
func TestWriteUpdatesModTime(t *testing.T) {
	g := newFakeGraph()
	g.add("notes.txt", []byte("old"))
	mnt := mount(t, g, options{}).dir
	path := filepath.Join(mnt, "notes.txt")

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(before.ModTime()) < 30*time.Minute {
		t.Fatalf("fixture mtime unexpectedly recent: %v", before.ModTime())
	}
	start := time.Now().Add(-2 * time.Second)
	if err := os.WriteFile(path, []byte("new content"), 0644); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if after.ModTime().Before(start) {
		t.Fatalf("mtime not updated by write: %v", after.ModTime())
	}
}

// #356: offline, a cached file can be opened read-write (as LibreOffice and
// other editors do just to view a document) and read; writes are refused.
func TestCachedFileOpensReadWriteOffline(t *testing.T) {
	g := newFakeGraph()
	g.add("doc.odt", []byte("document body"))
	m := mount(t, g, options{deltaInterval: 100 * time.Millisecond})
	path := filepath.Join(m.dir, "doc.odt")
	if _, err := os.ReadFile(path); err != nil { // cache it
		t.Fatal(err)
	}

	g.set(func(g *fakeGraph) { g.down = true })
	eventually(t, 5*time.Second, "filesystem to go offline", m.fs.IsOffline)

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("read-write open of cached file failed offline: %v", err)
	}
	defer f.Close()
	buf := make([]byte, 64)
	n, _ := f.Read(buf)
	if string(buf[:n]) != "document body" {
		t.Fatalf("read %q offline", buf[:n])
	}
	if _, err := f.WriteAt([]byte("x"), 0); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("offline write returned %v, want EROFS", err)
	}
	if _, err := os.OpenFile(path, os.O_RDWR|os.O_TRUNC, 0); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("offline truncating open returned %v, want EROFS", err)
	}
}

// Deletion deltas for children of folders that were never listed don't
// break anything (parent of the deleted item isn't loaded).
func TestDeltaForUnlistedFolderChildIsHarmless(t *testing.T) {
	g := newFakeGraph()
	g.add("Music/song.mp3", []byte("la"))
	m := mount(t, g, options{deltaInterval: 100 * time.Millisecond})
	g.queueDelta(graph.DriveItem{ID: idOf("Music/song.mp3"), Name: "song.mp3",
		Deleted: &graph.Deleted{State: "deleted"}, Parent: &graph.DriveItemParent{ID: idOf("Music")}})
	g.remove("Music/song.mp3")
	eventually(t, 5*time.Second, "delta poll", func() bool {
		return g.requestCount("/me/drive/root/delta") > 2
	})
	if got := names(t, filepath.Join(m.dir, "Music")); len(got) != 0 {
		t.Fatalf("Music lists %v, want empty", got)
	}
}
