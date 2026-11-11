package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// atomicSave does what KeePassXC, Qt's QSaveFile, LibreOffice and many
// editors do: write the new content to a temp file next to the original,
// close it, then rename it over the original.
func atomicSave(t *testing.T, dir, name string, data []byte, beforeRename time.Duration) {
	t.Helper()
	tmp := filepath.Join(dir, name+".QXyMZe")
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(beforeRename)
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		t.Fatalf("rename over %s failed: %v", name, err)
	}
}

// waitForServerContent waits until the server has exactly want at path.
func waitForServerContent(t *testing.T, g *fakeGraph, path string, want []byte) {
	t.Helper()
	eventually(t, 20*time.Second, "server content of "+path, func() bool {
		got, ok := g.content(path)
		return ok && bytes.Equal(got, want)
	})
}

func noTempFiles(t *testing.T, g *fakeGraph) {
	t.Helper()
	for _, p := range g.paths() {
		if strings.Contains(p, ".QXyMZe") {
			t.Fatalf("temp file left on the server: %v", g.paths())
		}
	}
}

func testAtomicSave(t *testing.T, size int, beforeRename time.Duration) {
	g := newFakeGraph()
	g.add("db.kdbx", testData(size, 1))
	mnt := mount(t, g, options{}).dir
	if _, err := os.ReadFile(filepath.Join(mnt, "db.kdbx")); err != nil {
		t.Fatal(err)
	}
	g.set(func(g *fakeGraph) { g.uploadDelay = 500 * time.Millisecond })

	saved := testData(size, 2)
	atomicSave(t, mnt, "db.kdbx", saved, beforeRename)

	waitForServerContent(t, g, "db.kdbx", saved)
	time.Sleep(3 * time.Second) // give a second (duplicate) upload time to show up
	noTempFiles(t, g)
	if n := g.uploadsOf("db.kdbx.QXyMZe"); n != 1 {
		t.Fatalf("temp file uploaded %d times, want 1", n)
	}
	if got, _ := os.ReadFile(filepath.Join(mnt, "db.kdbx")); !bytes.Equal(got, saved) {
		t.Fatal("local content after save is wrong")
	}
}

// #492: the rename arrives while the upload queued by the save is running.
func TestAtomicSaveSmallFileWhileUploading(t *testing.T) {
	testAtomicSave(t, 100*1024, 2500*time.Millisecond)
}

// #492: over 4 MiB the two uploads used chunked sessions and the save failed.
func TestAtomicSaveLargeFileWhileUploading(t *testing.T) {
	testAtomicSave(t, 6*1024*1024, 2500*time.Millisecond)
}

// The rename arrives before the queued upload has started.
func TestAtomicSaveRenameBeforeUploadStarts(t *testing.T) {
	testAtomicSave(t, 100*1024, 0)
}

// Saving twice in a row (both atomically) ends with the second version and
// no leftovers.
func TestRepeatedAtomicSaves(t *testing.T) {
	g := newFakeGraph()
	g.add("notes.md", []byte("v0"))
	mnt := mount(t, g, options{}).dir
	os.ReadFile(filepath.Join(mnt, "notes.md"))
	atomicSave(t, mnt, "notes.md", []byte("v1"), 0)
	atomicSave(t, mnt, "notes.md", []byte("v2"), 0)
	waitForServerContent(t, g, "notes.md", []byte("v2"))
	time.Sleep(3 * time.Second)
	noTempFiles(t, g)
	if got, _ := g.content("notes.md"); string(got) != "v2" {
		t.Fatalf("server ended with %q, want v2", got)
	}
}

// Editing a file and renaming it before its upload ran must not lose the
// edit: the move used to cancel the pending upload.
func TestEditThenRenameKeepsTheEdit(t *testing.T) {
	g := newFakeGraph()
	g.add("draft.txt", []byte("old"))
	mnt := mount(t, g, options{}).dir
	os.ReadFile(filepath.Join(mnt, "draft.txt"))

	if err := os.WriteFile(filepath.Join(mnt, "draft.txt"), []byte("edited"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(mnt, "draft.txt"), filepath.Join(mnt, "final.txt")); err != nil {
		t.Fatal(err)
	}
	waitForServerContent(t, g, "final.txt", []byte("edited"))
}

// A pending upload of a file that is replaced by a rename is dropped, and
// the replacement's content wins.
func TestReplacedFilesPendingUploadIsDropped(t *testing.T) {
	g := newFakeGraph()
	g.add("report.odt", []byte("original"))
	mnt := mount(t, g, options{}).dir
	path := filepath.Join(mnt, "report.odt")
	os.ReadFile(path)

	os.WriteFile(path, []byte("in-place edit"), 0644) // queues an upload of report.odt
	atomicSave(t, mnt, "report.odt", []byte("atomic save"), 0)
	waitForServerContent(t, g, "report.odt", []byte("atomic save"))
	time.Sleep(4 * time.Second) // the dropped upload must not overwrite it later
	if got, _ := g.content("report.odt"); string(got) != "atomic save" {
		t.Fatalf("server content %q, want the atomic save", got)
	}
	noTempFiles(t, g)
}

// If the server no longer knows a file's ID (deleted there without us seeing
// it), a local save creates it again by name instead of failing.
func TestUploadRecreatesItemTheServerForgot(t *testing.T) {
	g := newFakeGraph()
	g.add("todo.txt", []byte("old"))
	mnt := mount(t, g, options{}).dir
	path := filepath.Join(mnt, "todo.txt")
	os.ReadFile(path)

	g.remove("todo.txt")
	if err := os.WriteFile(path, []byte("new list"), 0644); err != nil {
		t.Fatal(err)
	}
	waitForServerContent(t, g, "todo.txt", []byte("new list"))
}
