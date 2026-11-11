package fs

import (
	"encoding/json"
	"time"

	"github.com/jstaf/onedriver/fs/graph"
	"github.com/rs/zerolog/log"
	bolt "go.etcd.io/bbolt"
)

// quotaKey is where the last fetched drive quota is stored in bucketDelta.
var quotaKey = []byte("quota")

// prefetchDelay is the pause between directory listings fetched from the
// server while prefetching, so the crawl doesn't hog the connection or
// trigger Graph API throttling.
const prefetchDelay = 100 * time.Millisecond

// fetchDrive fetches drive information from the server and saves it to the
// database, so the next startup can answer statfs before going online.
func (f *Filesystem) fetchDrive() (graph.Drive, error) {
	drive, err := graph.GetDrive(f.auth)
	if err != nil {
		return drive, err
	}
	if data, err := json.Marshal(drive); err == nil {
		f.db.Batch(func(tx *bolt.Tx) error {
			return tx.Bucket(bucketDelta).Put(quotaKey, data)
		})
	}
	return drive, nil
}

// loadSavedDrive seeds the quota cache from the database, if a previous
// session saved one.
func (f *Filesystem) loadSavedDrive() {
	f.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketDelta).Get(quotaKey)
		if data == nil {
			return nil
		}
		var drive graph.Drive
		if err := json.Unmarshal(data, &drive); err == nil {
			f.quota.Seed(drive)
		}
		return nil
	})
}

// PrefetchDirectories walks the whole directory tree and fetches the listing
// of every folder that isn't cached yet. File contents are not downloaded.
// Listings are kept up to date by the delta loop afterwards and persist in the
// database, so opening a folder (or a file manager counting the items in each
// subfolder) is served from the cache instead of waiting on the network.
// Already-cached folders cost nothing, so this is cheap to run repeatedly.
func (f *Filesystem) PrefetchDirectories() {
	start := time.Now()
	queue := []string{f.root}
	visited, fetched := 0, 0
	for len(queue) > 0 {
		if f.IsOffline() {
			log.Debug().Msg("Offline, stopping directory prefetch.")
			return
		}
		id := queue[0]
		queue = queue[1:]

		inode := f.GetID(id)
		if inode == nil || !inode.IsDir() {
			continue
		}
		inode.RLock()
		cached := inode.children != nil
		inode.RUnlock()
		if !cached {
			// leave the API alone while it is throttling us; interactive
			// requests from the file manager need the headroom more
			if wait := graph.ThrottledFor(); wait > 0 {
				log.Info().Dur("wait", wait).Msg("Directory prefetch paused while throttled.")
				time.Sleep(wait)
			}
		}

		children, err := f.GetChildrenID(id, f.auth)
		if err != nil {
			log.Warn().Err(err).Str("id", id).Msg("Could not prefetch directory.")
			continue
		}
		visited++
		if !cached {
			fetched++
			time.Sleep(prefetchDelay)
		}
		for _, child := range children {
			if child.IsDir() {
				queue = append(queue, child.ID())
			}
		}
	}
	if fetched > 0 {
		f.SerializeAll()
	}
	log.Info().
		Int("directories", visited).
		Int("fetched", fetched).
		Dur("elapsed", time.Since(start)).
		Msg("Directory prefetch complete.")
}

// BackgroundRefresh keeps the quota cache and directory listings warm: it
// prefetches the directory tree once at startup and then re-checks both at
// the given interval (picking up folders created on other devices).
func (f *Filesystem) BackgroundRefresh(interval time.Duration) {
	for {
		if !f.IsOffline() {
			if err := f.quota.RefreshIfStale(); err != nil {
				log.Debug().Err(err).Msg("Could not refresh drive quota.")
			}
			f.PrefetchDirectories()
		}
		time.Sleep(interval)
	}
}
