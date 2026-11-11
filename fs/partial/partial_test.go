package partial

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestPlan(t *testing.T) {
	big := uint64(10 * HeadSize)
	cases := []struct {
		name   string
		size   uint64
		offset uint64
		length int
		want   Action
	}{
		{"small file always full", HeadSize, 0, 4096, Full},
		{"tiny file always full", 10, 0, 4096, Full},
		{"sniff read of big file", big, 0, 4096, Head},
		{"readahead within head", big, 0, 128 * 1024, Head},
		{"read ending exactly at head", big, HeadSize - 4096, 4096, Head},
		{"read crossing head boundary", big, HeadSize - 4096, 8192, Full},
		{"read past head", big, 2 * HeadSize, 4096, Full},
		{"negative length", big, 0, -1, Full},
	}
	for _, c := range cases {
		if got := Plan(c.size, c.offset, c.length); got != c.want {
			t.Errorf("%s: Plan(%d, %d, %d) = %v, want %v",
				c.name, c.size, c.offset, c.length, got, c.want)
		}
	}
}

// content returns deterministic test data of length n.
func content(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i) ^ seed
	}
	return b
}

type fakeRange struct {
	data  []byte
	calls atomic.Int32
	err   error
	short bool
}

func (f *fakeRange) fetch(end uint64) ([]byte, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	if f.short {
		end /= 2
	}
	return append([]byte(nil), f.data[:end+1]...), nil
}

func TestBufferReadsAndCaches(t *testing.T) {
	file := content(3*HeadSize, 1)
	srv := &fakeRange{data: file}
	var b Buffer

	got, err := b.Read("h1", uint64(len(file)), 100, 4096, srv.fetch)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, file[100:100+4096]) {
		t.Fatal("wrong data for first read")
	}
	got, _ = b.Read("h1", uint64(len(file)), HeadSize-10, 4096, srv.fetch)
	if !bytes.Equal(got, file[HeadSize-10:HeadSize]) {
		t.Fatal("read at end of head not clamped to buffered data")
	}
	if n := srv.calls.Load(); n != 1 {
		t.Fatalf("got %d range requests, want 1", n)
	}
}

func TestBufferDiscardsOnHashChange(t *testing.T) {
	old := content(2*HeadSize, 1)
	srv := &fakeRange{data: old}
	var b Buffer
	b.Read("old", uint64(len(old)), 0, 10, srv.fetch)

	srv.data = content(2*HeadSize, 2)
	got, err := b.Read("new", uint64(len(srv.data)), 0, 10, srv.fetch)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, srv.data[:10]) {
		t.Fatal("served stale data after the file changed")
	}
	if n := srv.calls.Load(); n != 2 {
		t.Fatalf("got %d range requests, want 2", n)
	}
}

func TestBufferErrors(t *testing.T) {
	var b Buffer
	srv := &fakeRange{data: content(2*HeadSize, 1), err: errors.New("offline")}
	if _, err := b.Read("h", 2*HeadSize, 0, 10, srv.fetch); err == nil {
		t.Fatal("expected fetch error")
	}
	srv.err = nil
	srv.short = true
	if _, err := b.Read("h", 2*HeadSize, 0, 10, srv.fetch); err == nil {
		t.Fatal("expected error for short range response")
	}
	if _, err := b.Read("", 2*HeadSize, 0, 10, srv.fetch); err == nil {
		t.Fatal("expected error for missing hash")
	}
	// a failed fetch must not leave partial state that later reads trust
	srv.short = false
	got, err := b.Read("h", 2*HeadSize, 0, 10, srv.fetch)
	if err != nil || !bytes.Equal(got, srv.data[:10]) {
		t.Fatalf("recovery read failed: %v", err)
	}
}

func TestBufferReturnsCopies(t *testing.T) {
	file := content(2*HeadSize, 1)
	srv := &fakeRange{data: file}
	var b Buffer
	got, _ := b.Read("h", uint64(len(file)), 0, 10, srv.fetch)
	got[0] ^= 0xff
	again, _ := b.Read("h", uint64(len(file)), 0, 10, srv.fetch)
	if !bytes.Equal(again, file[:10]) {
		t.Fatal("caller mutation leaked into the shared buffer")
	}
}

func TestBufferConcurrentReadsShareOneFetch(t *testing.T) {
	file := content(2*HeadSize, 3)
	srv := &fakeRange{data: file}
	var b Buffer
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := uint64(i * 1000)
			got, err := b.Read("h", uint64(len(file)), off, 512, srv.fetch)
			if err != nil || !bytes.Equal(got, file[off:off+512]) {
				t.Errorf("concurrent read %d wrong: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if n := srv.calls.Load(); n != 1 {
		t.Fatalf("got %d range requests, want 1", n)
	}
}

func TestBufferReadPastEndOfHead(t *testing.T) {
	file := content(2*HeadSize, 1)
	srv := &fakeRange{data: file}
	var b Buffer
	got, err := b.Read("h", uint64(len(file)), HeadSize+5, 10, srv.fetch)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %d bytes, %v; want empty", len(got), err)
	}
}

func TestReset(t *testing.T) {
	file := content(2*HeadSize, 1)
	srv := &fakeRange{data: file}
	var b Buffer
	b.Read("h", uint64(len(file)), 0, 10, srv.fetch)
	b.Reset()
	b.Read("h", uint64(len(file)), 0, 10, srv.fetch)
	if n := srv.calls.Load(); n != 2 {
		t.Fatalf("got %d range requests after Reset, want 2", n)
	}
}
