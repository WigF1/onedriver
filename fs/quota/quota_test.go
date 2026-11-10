package quota

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jstaf/onedriver/fs/graph"
)

// fakeClock is a manually advanced clock for tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeServer counts fetches and returns a configurable result.
type fakeServer struct {
	calls     atomic.Int32
	remaining atomic.Uint64
	fail      atomic.Bool
	block     chan struct{} // if non-nil, fetches wait on it
}

func (s *fakeServer) fetch() (graph.Drive, error) {
	s.calls.Add(1)
	if s.block != nil {
		<-s.block
	}
	if s.fail.Load() {
		return graph.Drive{}, errors.New("offline")
	}
	return graph.Drive{Quota: graph.DriveQuota{Remaining: s.remaining.Load()}}, nil
}

func newTestCache(s *fakeServer, ttl time.Duration) (*Cache, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	c := New(s.fetch, ttl)
	c.now = clock.now
	return c, clock
}

// waitFor polls cond until it is true or the test times out.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFirstGetFetchesSynchronously(t *testing.T) {
	s := &fakeServer{}
	s.remaining.Store(42)
	c, _ := newTestCache(s, time.Minute)

	drive, err := c.Get()
	if err != nil {
		t.Fatal(err)
	}
	if drive.Quota.Remaining != 42 {
		t.Fatalf("got remaining %d, want 42", drive.Quota.Remaining)
	}
	if n := s.calls.Load(); n != 1 {
		t.Fatalf("got %d fetches, want 1", n)
	}
}

func TestFreshValueServedWithoutFetching(t *testing.T) {
	s := &fakeServer{}
	c, clock := newTestCache(s, time.Minute)
	c.Get()
	for i := 0; i < 100; i++ {
		clock.advance(time.Second / 2)
		c.Get()
	}
	if n := s.calls.Load(); n != 1 {
		t.Fatalf("got %d fetches within TTL, want 1", n)
	}
}

func TestStaleValueReturnedImmediatelyAndRefreshedInBackground(t *testing.T) {
	s := &fakeServer{}
	s.remaining.Store(1)
	c, clock := newTestCache(s, time.Minute)
	c.Get()

	// next fetch will block until released: Get must not wait for it
	s.block = make(chan struct{})
	s.remaining.Store(2)
	clock.advance(2 * time.Minute)

	done := make(chan graph.Drive)
	go func() {
		d, _ := c.Get()
		done <- d
	}()
	select {
	case d := <-done:
		if d.Quota.Remaining != 1 {
			t.Fatalf("got remaining %d, want stale value 1", d.Quota.Remaining)
		}
	case <-time.After(time.Second):
		t.Fatal("Get blocked on a stale refresh")
	}

	// concurrent callers while the refresh is in flight don't start more fetches
	waitFor(t, func() bool { return s.calls.Load() == 2 })
	for i := 0; i < 10; i++ {
		c.Get()
	}
	if n := s.calls.Load(); n != 2 {
		t.Fatalf("got %d fetches, want 2 (one background refresh)", n)
	}

	close(s.block)
	waitFor(t, func() bool {
		d, _ := c.Get()
		return d.Quota.Remaining == 2
	})
}

func TestFailedRefreshKeepsLastValueAndBacksOff(t *testing.T) {
	s := &fakeServer{}
	s.remaining.Store(7)
	c, clock := newTestCache(s, time.Minute)
	c.Get()

	s.fail.Store(true)
	clock.advance(2 * time.Minute)
	if err := c.Refresh(); err == nil {
		t.Fatal("expected refresh error")
	}
	d, err := c.Get()
	if err != nil || d.Quota.Remaining != 7 {
		t.Fatalf("got (%d, %v), want last good value 7", d.Quota.Remaining, err)
	}

	// immediately after a failed attempt, Get must not retry on every call
	calls := s.calls.Load()
	for i := 0; i < 50; i++ {
		c.Get()
	}
	if n := s.calls.Load(); n != calls {
		t.Fatalf("got %d extra fetches during backoff, want 0", n-calls)
	}

	// after the retry interval, one background attempt is made
	clock.advance(c.retry)
	c.Get()
	waitFor(t, func() bool { return s.calls.Load() == calls+1 })
}

func TestGetWithNothingCachedReturnsError(t *testing.T) {
	s := &fakeServer{}
	s.fail.Store(true)
	c, _ := newTestCache(s, time.Minute)
	if _, err := c.Get(); err == nil {
		t.Fatal("expected error with empty cache and failing server")
	}
}

func TestSeedServesSavedValueThenRefreshes(t *testing.T) {
	s := &fakeServer{}
	s.remaining.Store(99)
	c, _ := newTestCache(s, time.Minute)
	c.Seed(graph.Drive{Quota: graph.DriveQuota{Remaining: 5}})

	d, err := c.Get()
	if err != nil || d.Quota.Remaining != 5 {
		t.Fatalf("got (%d, %v), want seeded value 5 without blocking", d.Quota.Remaining, err)
	}
	// seeded value is stale, so a background refresh replaces it
	waitFor(t, func() bool {
		d, _ := c.Get()
		return d.Quota.Remaining == 99
	})
}

func TestSeedDoesNotOverwriteFetchedValue(t *testing.T) {
	s := &fakeServer{}
	s.remaining.Store(10)
	c, _ := newTestCache(s, time.Minute)
	c.Get()
	c.Seed(graph.Drive{Quota: graph.DriveQuota{Remaining: 1}})
	if d, _ := c.Get(); d.Quota.Remaining != 10 {
		t.Fatalf("got %d, want fetched value 10", d.Quota.Remaining)
	}
}

func TestRefreshIfStale(t *testing.T) {
	s := &fakeServer{}
	c, clock := newTestCache(s, time.Minute)
	c.RefreshIfStale() // empty -> fetch
	c.RefreshIfStale() // fresh -> no fetch
	clock.advance(time.Minute)
	c.RefreshIfStale() // stale -> fetch
	if n := s.calls.Load(); n != 2 {
		t.Fatalf("got %d fetches, want 2", n)
	}
}
