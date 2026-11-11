package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// LibreOffice-style lock file: created when a document is opened, deleted on
// close. Deleted while its first upload was running, it used to be left on
// OneDrive (it had no server ID yet, so the delete never reached the server).
func TestFileDeletedDuringFirstUploadIsRemovedFromServer(t *testing.T) {
	g := newFakeGraph()
	g.add("TestCalc.xlsx", []byte("sheet"))
	mnt := mount(t, g, options{}).dir
	g.set(func(g *fakeGraph) { g.uploadDelay = 1500 * time.Millisecond })

	lock := filepath.Join(mnt, ".~lock.TestCalc.xlsx#")
	if err := os.WriteFile(lock, []byte("user,host,date"), 0644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond) // the upload has started, the server is still busy
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}

	eventually(t, 10*time.Second, "upload to reach the server", func() bool {
		return g.uploadsOf(".~lock.TestCalc.xlsx#") > 0
	})
	eventually(t, 10*time.Second, "lock file to be deleted on the server", func() bool {
		_, exists := g.content(".~lock.TestCalc.xlsx#")
		return !exists
	})
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("lock file visible locally again: %v", err)
	}
}

// Deleted before its upload started: nothing is uploaded at all.
func TestFileDeletedBeforeUploadIsNeverUploaded(t *testing.T) {
	g := newFakeGraph()
	mnt := mount(t, g, options{}).dir
	path := filepath.Join(mnt, "scratch.tmp")
	os.WriteFile(path, []byte("x"), 0644)
	os.Remove(path)
	time.Sleep(4 * time.Second)
	if n := g.uploadsOf("scratch.tmp"); n != 0 {
		t.Fatalf("deleted file uploaded %d times", n)
	}
}
