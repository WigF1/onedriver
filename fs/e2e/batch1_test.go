package e2e

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jstaf/onedriver/fs/graph"
)

func readNames(t *testing.T, f *os.File, n int) []string {
	t.Helper()
	names, err := f.Readdirnames(n)
	if err != nil && n <= 0 {
		t.Fatal(err)
	}
	return names
}

// #494: listings come back in a stable, sorted order.
func TestDirectoryListingIsSortedAndStable(t *testing.T) {
	g := newFakeGraph()
	for i := 0; i < 50; i++ {
		g.add(fmt.Sprintf("file-%02d.txt", (i*37)%50), []byte("x"))
	}
	mnt := mount(t, g, options{}).dir

	var first []string
	for i := 0; i < 5; i++ {
		f, err := os.Open(mnt)
		if err != nil {
			t.Fatal(err)
		}
		names := readNames(t, f, -1)
		f.Close()
		if !sort.StringsAreSorted(names) {
			t.Fatalf("listing not sorted: %v", names[:5])
		}
		if first == nil {
			first = names
		} else if strings.Join(first, "/") != strings.Join(names, "/") {
			t.Fatal("listing order changed between reads")
		}
	}
	if len(first) != 51 { // 50 files + trash folder
		t.Fatalf("got %d entries, want 51", len(first))
	}
}

// #494: a second reader opening and closing the same directory must not
// disturb a reader that is part-way through it (the old per-directory
// listing was thrown away on the other reader's close and rebuilt in a new
// random order, giving duplicate and missing entries).
func TestConcurrentDirectoryReadersDontInterfere(t *testing.T) {
	g := newFakeGraph()
	const n = 800
	for i := 0; i < n; i++ {
		g.add(fmt.Sprintf("entry-with-a-longish-name-%04d.dat", i), []byte("x"))
	}
	mnt := mount(t, g, options{}).dir

	a, err := os.Open(mnt)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	got := readNames(t, a, 5) // first getdents fills only part of the listing

	for i := 0; i < 3; i++ {
		b, _ := os.Open(mnt)
		readNames(t, b, -1)
		b.Close()
	}

	got = append(got, readNames(t, a, -1)...)
	seen := make(map[string]bool)
	for _, name := range got {
		if seen[name] {
			t.Fatalf("duplicate entry %q", name)
		}
		seen[name] = true
	}
	if len(seen) != n+1 {
		t.Fatalf("got %d distinct entries, want %d", len(seen), n+1)
	}
}

// Graph throttling: 429 responses are retried after Retry-After.
func TestThrottledRequestsAreRetried(t *testing.T) {
	g := newFakeGraph()
	g.add("small.txt", []byte("throttled content"))
	mnt := mount(t, g, options{}).dir

	g.throttleNext("/me/drive/items/id-small.txt", 1, 0)
	start := time.Now()
	got, err := os.ReadFile(filepath.Join(mnt, "small.txt"))
	if err != nil {
		t.Fatalf("read failed while throttled: %v", err)
	}
	if string(got) != "throttled content" {
		t.Fatalf("got %q", got)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("Retry-After not honoured: finished in %v", elapsed)
	}
}

func TestThrottledUploadResendsBody(t *testing.T) {
	g := newFakeGraph()
	mnt := mount(t, g, options{}).dir

	g.throttleNext("/me/drive/items/"+rootID+":/", 0)
	if err := os.WriteFile(filepath.Join(mnt, "new.txt"), []byte("body survives retry"), 0644); err != nil {
		t.Fatal(err)
	}
	eventually(t, 15*time.Second, "upload of new.txt", func() bool {
		data, ok := g.uploaded("new.txt")
		return ok && string(data) == "body survives retry"
	})
}

func TestThrottledForReportsBackoff(t *testing.T) {
	g := newFakeGraph()
	g.add("a.txt", []byte("a"))
	mnt := mount(t, g, options{}).dir
	g.throttleNext("/me/drive/items/id-a.txt", 3)
	done := make(chan struct{})
	go func() {
		os.ReadFile(filepath.Join(mnt, "a.txt"))
		close(done)
	}()
	eventually(t, 2*time.Second, "throttle to be reported", func() bool {
		return graph.ThrottledFor() > time.Second
	})
	<-done
}

// #470: an unchanged filesystem doesn't rewrite its metadata every poll.
func TestSerializeOnlyWritesChanges(t *testing.T) {
	g := newFakeGraph()
	for i := 0; i < 20; i++ {
		g.add(fmt.Sprintf("f%d", i), []byte("x"))
	}
	m := mount(t, g, options{})
	os.ReadDir(m.dir)

	if n := m.fs.SerializeAll(); n == 0 {
		t.Fatal("first serialization wrote nothing")
	}
	if n := m.fs.SerializeAll(); n != 0 {
		t.Fatalf("idle serialization rewrote %d items, want 0", n)
	}
	os.WriteFile(filepath.Join(m.dir, "f3"), []byte("changed"), 0644)
	if n := m.fs.SerializeAll(); n == 0 {
		t.Fatal("change was not serialized")
	}
}

// #425: empty files are uploaded.
func TestEmptyFileIsUploaded(t *testing.T) {
	g := newFakeGraph()
	mnt := mount(t, g, options{}).dir

	f, err := os.Create(filepath.Join(mnt, "empty.txt"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	eventually(t, 15*time.Second, "upload of empty.txt", func() bool {
		data, ok := g.uploaded("empty.txt")
		return ok && len(data) == 0
	})
}

// #473: a file deleted on the server without us seeing the delta is
// removed locally when it can't be downloaded.
func TestFileDeletedOnServerIsRemovedOnOpen(t *testing.T) {
	g := newFakeGraph()
	g.add("gone.txt", []byte("soon deleted"))
	g.add("kept.txt", []byte("kept"))
	mnt := mount(t, g, options{}).dir
	path := filepath.Join(mnt, "gone.txt")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}

	g.remove("gone.txt")
	// the read fails (ENOENT, or EBADF for kernel readahead requests that
	// arrive after the item has been removed)
	if _, err := os.ReadFile(path); err == nil {
		t.Fatal("read of a file deleted on the server succeeded")
	}
	eventually(t, 5*time.Second, "gone.txt to disappear from listing", func() bool {
		entries, _ := os.ReadDir(mnt)
		for _, e := range entries {
			if e.Name() == "gone.txt" {
				return false
			}
		}
		return true
	})
}

// #473: deletion deltas without a parent reference are still applied.
func TestDeletionDeltaWithoutParentIsApplied(t *testing.T) {
	g := newFakeGraph()
	g.add("victim.txt", []byte("x"))
	mnt := mount(t, g, options{deltaInterval: 200 * time.Millisecond}).dir
	if _, err := os.Stat(filepath.Join(mnt, "victim.txt")); err != nil {
		t.Fatal(err)
	}

	g.remove("victim.txt")
	g.queueDelta(graph.DriveItem{
		ID:      "id-victim.txt",
		Name:    "victim.txt",
		Deleted: &graph.Deleted{State: "deleted"},
		Parent:  &graph.DriveItemParent{},
	})
	eventually(t, 5*time.Second, "victim.txt to be removed by delta", func() bool {
		_, err := os.Stat(filepath.Join(mnt, "victim.txt"))
		return errors.Is(err, fs.ErrNotExist)
	})
}
