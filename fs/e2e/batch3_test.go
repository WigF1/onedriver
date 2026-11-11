package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// #472: when the server stops answering, only the first operation waits for
// the network timeout; the following ones answer from the cache (or fail)
// right away instead of each hanging the application, and everything works
// again once the network is back.
func TestNetworkOutageFailsFast(t *testing.T) {
	g := newFakeGraph()
	g.add("cached.txt", []byte("cached"))
	g.add("uncached.txt", []byte("not downloaded"))
	g.add("A/a.txt", []byte("a"))
	g.add("B/b.txt", []byte("b"))
	g.add("C/c.txt", []byte("c"))
	m := mount(t, g, options{deltaInterval: 500 * time.Millisecond})
	if _, err := os.ReadFile(filepath.Join(m.dir, "cached.txt")); err != nil {
		t.Fatal(err)
	}

	g.set(func(g *fakeGraph) { g.hang = true })
	start := time.Now()
	os.ReadDir(filepath.Join(m.dir, "A")) // first request runs into the timeout
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("first operation took %v", d)
	}

	// everything else answers immediately
	for name, op := range map[string]func() error{
		"list unlisted folder": func() error { _, err := os.ReadDir(filepath.Join(m.dir, "B")); return err },
		"open uncached file":   func() error { _, err := os.ReadFile(filepath.Join(m.dir, "uncached.txt")); return err },
		"read cached file": func() error {
			got, err := os.ReadFile(filepath.Join(m.dir, "cached.txt"))
			if err == nil && string(got) != "cached" {
				t.Errorf("cached file content %q", got)
			}
			return err
		},
		"stat": func() error { _, err := os.Stat(filepath.Join(m.dir, "C")); return err },
	} {
		start := time.Now()
		err := op()
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s took %v during the outage (err %v)", name, d, err)
		}
		if name == "read cached file" && err != nil {
			t.Errorf("cached file unreadable during outage: %v", err)
		}
	}

	g.set(func(g *fakeGraph) { g.hang = false })
	eventually(t, 15*time.Second, "folder B to list again after the outage", func() bool {
		entries, err := os.ReadDir(filepath.Join(m.dir, "B"))
		return err == nil && len(entries) == 1
	})
	got, err := os.ReadFile(filepath.Join(m.dir, "uncached.txt"))
	if err != nil || string(got) != "not downloaded" {
		t.Fatalf("uncached file after outage: %q, %v", got, err)
	}
}

// #343: an edit made on another device (or in OneDrive web) is picked up
// even when the server's modification time isn't newer than the local one
// (clock differences, or a local write that bumped the local mtime).
func TestRemoteEditIsAppliedRegardlessOfClocks(t *testing.T) {
	g := newFakeGraph()
	g.add("report.docx", []byte("version one"))
	m := mount(t, g, options{deltaInterval: 200 * time.Millisecond})
	path := filepath.Join(m.dir, "report.docx")
	if got, _ := os.ReadFile(path); string(got) != "version one" {
		t.Fatalf("initial content %q", got)
	}

	// edited elsewhere; the fake keeps the old server mtime
	g.add("report.docx", []byte("version two, edited in Word online"))
	g.set(func(g *fakeGraph) { g.deltas = append(g.deltas, g.item("report.docx")) })
	eventually(t, 10*time.Second, "remote edit to show locally", func() bool {
		got, _ := os.ReadFile(path)
		return string(got) == "version two, edited in Word online"
	})
}

// Same, after the file was last written locally (local mtime = now).
func TestRemoteEditAfterLocalWriteIsApplied(t *testing.T) {
	g := newFakeGraph()
	g.add("notes.txt", []byte("v0"))
	m := mount(t, g, options{deltaInterval: 200 * time.Millisecond})
	path := filepath.Join(m.dir, "notes.txt")
	os.WriteFile(path, []byte("v1 local"), 0644)
	waitForServerContent(t, g, "notes.txt", []byte("v1 local"))
	time.Sleep(2500 * time.Millisecond) // upload completion processed

	g.add("notes.txt", []byte("v2 from the phone"))
	g.set(func(g *fakeGraph) { g.deltas = append(g.deltas, g.item("notes.txt")) })
	eventually(t, 10*time.Second, "remote edit to show locally", func() bool {
		got, _ := os.ReadFile(path)
		return string(got) == "v2 from the phone"
	})
}

// A remote change arriving while local changes are still being uploaded
// doesn't throw the local changes away.
func TestRemoteChangeDuringLocalUploadKeepsLocalChanges(t *testing.T) {
	g := newFakeGraph()
	g.add("budget.ods", []byte("original"))
	m := mount(t, g, options{deltaInterval: 200 * time.Millisecond})
	path := filepath.Join(m.dir, "budget.ods")
	os.ReadFile(path)
	g.set(func(g *fakeGraph) { g.uploadDelay = 3 * time.Second })

	os.WriteFile(path, []byte("my local edit"), 0644)
	g.add("budget.ods", []byte("someone else's edit"))
	g.set(func(g *fakeGraph) { g.deltas = append(g.deltas, g.item("budget.ods")) })

	waitForServerContent(t, g, "budget.ods", []byte("my local edit"))
	time.Sleep(time.Second)
	if got, _ := os.ReadFile(path); string(got) != "my local edit" {
		t.Fatalf("local file shows %q, want the local edit", got)
	}
}

// #390: names that differ by a Turkish dotted İ are different files, and a
// folder named with İ can be used. Case-insensitive lookups still work.
func TestTurkishDottedIFilenames(t *testing.T) {
	g := newFakeGraph()
	g.add("THIS_IS_TEST.txt", []byte("test1"))
	g.add("THİS_İS_TEST.txt", []byte("test2"))
	g.add("İstanbul/plan.txt", []byte("gezi"))
	mnt := mount(t, g, options{}).dir

	got := names(t, mnt)
	want := map[string]bool{"THIS_IS_TEST.txt": true, "THİS_İS_TEST.txt": true, "İstanbul": true}
	for _, n := range got {
		delete(want, n)
	}
	if len(want) != 0 {
		t.Fatalf("listing %v is missing %v", got, want)
	}

	for name, content := range map[string]string{
		"THIS_IS_TEST.txt":  "test1",
		"THİS_İS_TEST.txt":  "test2",
		"this_is_test.txt":  "test1", // case-insensitive, like OneDrive
		"THİS_İS_TEST.TXT":  "test2",
		"İstanbul/plan.txt": "gezi",
	} {
		data, err := os.ReadFile(filepath.Join(mnt, name))
		if err != nil || string(data) != content {
			t.Errorf("%s: got %q, %v; want %q", name, data, err, content)
		}
	}

	// a folder created locally with İ is usable
	dir := filepath.Join(mnt, "İzmir")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatalf("mkdir İzmir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("stat of new İ folder: %v", err)
	}
}
