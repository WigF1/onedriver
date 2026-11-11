package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jstaf/onedriver/fs/partial"
)

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
	mnt := mount(t, g, options{}).dir

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
	mnt := mount(t, g, options{}).dir
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
	mnt := mount(t, g, options{}).dir

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
	mnt := mount(t, g, options{}).dir

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
	mnt := mount(t, g, options{}).dir

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
	mnt := mount(t, g, options{}).dir

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
	mnt := mount(t, g, options{}).dir
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
	mnt := mount(t, g, options{}).dir
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
	mnt := mount(t, g, options{}).dir
	for i := 0; i < 20; i++ {
		name := "file" + strconv.Itoa(i) + ".dat"
		readAt(t, filepath.Join(mnt, name), 0, 4096)
		if _, f := g.counts(name); f != 0 {
			t.Fatalf("%s was downloaded in full", name)
		}
	}
}
