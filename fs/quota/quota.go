// Package quota caches the drive quota reported by the Graph API so that
// statfs() calls on the mount never have to wait on the network.
//
// File managers (Nautilus in particular) call statfs on their UI thread every
// time they show a location, often several times in a row. Fetching
// /me/drive for every call takes 1-4 seconds and freezes the file manager.
// Quota changes slowly and nothing depends on it being exact, so callers are
// served the last known value and the cache is refreshed in the background.
package quota

import (
	"sync"
	"time"

	"github.com/jstaf/onedriver/fs/graph"
)

// Fetcher retrieves the current drive information from the server.
type Fetcher func() (graph.Drive, error)

// DefaultTTL is how long a fetched quota is considered fresh.
const DefaultTTL = 5 * time.Minute

// Cache holds the most recently fetched drive information.
type Cache struct {
	fetch Fetcher
	ttl   time.Duration
	// minimum time between background refresh attempts, so a failing
	// (e.g. offline) refresh is not retried on every statfs call
	retry time.Duration
	now   func() time.Time

	mu          sync.Mutex
	drive       graph.Drive
	valid       bool
	fetched     time.Time
	lastAttempt time.Time
	refreshing  bool
}

// New creates a quota cache that uses fetch to retrieve drive information and
// considers a fetched value fresh for ttl.
func New(fetch Fetcher, ttl time.Duration) *Cache {
	retry := ttl / 6
	if retry > 30*time.Second {
		retry = 30 * time.Second
	}
	if retry < time.Second {
		retry = time.Second
	}
	return &Cache{
		fetch: fetch,
		ttl:   ttl,
		retry: retry,
		now:   time.Now,
	}
}

// Seed loads a previously saved value (for example from the on-disk cache at
// startup) so statfs can be answered immediately. The seeded value is treated
// as stale, so it is replaced by the next refresh. Does nothing if a value has
// already been fetched.
func (c *Cache) Seed(drive graph.Drive) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid {
		return
	}
	c.drive = drive
	c.valid = true
	c.fetched = time.Time{}
}

// Get returns the cached drive information without blocking on the network,
// except when nothing has ever been fetched successfully, in which case it
// fetches synchronously. If the cached value is stale, it is still returned
// and a refresh is started in the background.
func (c *Cache) Get() (graph.Drive, error) {
	c.mu.Lock()
	if !c.valid {
		c.mu.Unlock()
		if err := c.Refresh(); err != nil {
			return graph.Drive{}, err
		}
		c.mu.Lock()
	}
	drive := c.drive
	if c.staleLocked() && c.startRefreshLocked() {
		go c.backgroundRefresh()
	}
	c.mu.Unlock()
	return drive, nil
}

// Refresh fetches drive information now. On failure the previously cached
// value (if any) is kept.
func (c *Cache) Refresh() error {
	c.mu.Lock()
	c.lastAttempt = c.now()
	c.mu.Unlock()

	drive, err := c.fetch()

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		return err
	}
	c.drive = drive
	c.valid = true
	c.fetched = c.now()
	return nil
}

// RefreshIfStale fetches drive information if the cached value is missing or
// older than the TTL. Intended to be called from a background goroutine (such
// as the delta loop) so the value is usually fresh before anyone asks for it.
func (c *Cache) RefreshIfStale() error {
	c.mu.Lock()
	stale := !c.valid || c.now().Sub(c.fetched) >= c.ttl
	c.mu.Unlock()
	if !stale {
		return nil
	}
	return c.Refresh()
}

func (c *Cache) staleLocked() bool {
	return c.now().Sub(c.fetched) >= c.ttl
}

// startRefreshLocked reports whether the caller should start a background
// refresh, and marks one as in progress if so.
func (c *Cache) startRefreshLocked() bool {
	if c.refreshing || c.now().Sub(c.lastAttempt) < c.retry {
		return false
	}
	c.refreshing = true
	return true
}

func (c *Cache) backgroundRefresh() {
	c.Refresh()
	c.mu.Lock()
	c.refreshing = false
	c.mu.Unlock()
}
