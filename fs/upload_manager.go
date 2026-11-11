package fs

import (
	"encoding/json"
	"time"

	"github.com/jstaf/onedriver/fs/graph"
	"github.com/rs/zerolog/log"
	bolt "go.etcd.io/bbolt"
)

const maxUploadsInFlight = 5

var bucketUploads = []byte("uploads")

// UploadManager is used to manage and retry uploads.
type UploadManager struct {
	queue         chan *UploadSession
	deletionQueue chan string
	waitQueue     chan uploadWait
	waiters       map[string][]chan uploadResult // only touched by uploadLoop
	sessions      map[string]*UploadSession
	inFlight      uint8 // number of sessions in flight
	auth          *graph.Auth
	fs            *Filesystem
	db            *bolt.DB
}

// NewUploadManager creates a new queue/thread for uploads
func NewUploadManager(duration time.Duration, db *bolt.DB, fs *Filesystem, auth *graph.Auth) *UploadManager {
	manager := UploadManager{
		queue:         make(chan *UploadSession),
		deletionQueue: make(chan string, 1000), // FIXME - why does this chan need to be buffered now???
		waitQueue:     make(chan uploadWait),
		waiters:       make(map[string][]chan uploadResult),
		sessions:      make(map[string]*UploadSession),
		auth:          auth,
		db:            db,
		fs:            fs,
	}
	db.View(func(tx *bolt.Tx) error {
		// Add any incomplete sessions from disk - any sessions here were never
		// finished. The most likely cause of this is that the user shut off
		// their computer or closed the program after starting the upload.
		b := tx.Bucket(bucketUploads)
		if b == nil {
			// bucket does not exist yet, bail out early
			return nil
		}
		return b.ForEach(func(key []byte, val []byte) error {
			session := &UploadSession{}
			err := json.Unmarshal(val, session)
			if err != nil {
				log.Error().Err(err).Msg("Failure restoring upload sessions from disk.")
				return err
			}
			if session.getState() != uploadNotStarted {
				manager.inFlight++
			}
			session.cancel(auth) // uploads are currently non-resumable
			manager.sessions[session.ID] = session
			return nil
		})
	})
	go manager.uploadLoop(duration)
	return &manager
}

// uploadLoop manages the deduplication and tracking of uploads
func (u *UploadManager) uploadLoop(duration time.Duration) {
	ticker := time.NewTicker(duration)
	for {
		select {
		case session := <-u.queue: // new sessions
			// deduplicate sessions for the same item
			if old, exists := u.sessions[session.ID]; exists {
				old.cancel(u.auth)
			}
			contents, _ := json.Marshal(session)
			u.db.Batch(func(tx *bolt.Tx) error {
				// persist to disk in case the user shuts off their computer or
				// kills onedriver prematurely
				b, _ := tx.CreateBucketIfNotExists(bucketUploads)
				return b.Put([]byte(session.ID), contents)
			})
			u.sessions[session.ID] = session

		case cancelID := <-u.deletionQueue: // remove uploads for deleted items
			session, exists := u.sessions[cancelID]
			if exists && session.createsItem() {
				switch session.getState() {
				case uploadStarted:
					// The item is being created on the server right now and
					// can't be stopped: it was deleted locally before it had
					// a server ID, so nothing deletes it there. Let the upload
					// finish and delete the result (LibreOffice lock files
					// left behind on OneDrive).
					session.Lock()
					session.discard = true
					session.Unlock()
					continue
				case uploadComplete:
					// finished, but the loop hasn't processed it yet
					u.discardUploaded(session)
				}
			}
			u.finishUpload(cancelID)

		case req := <-u.waitQueue: // someone needs the result of an upload now
			session, exists := u.sessions[req.id]
			if !exists {
				req.done <- uploadResult{found: false}
				continue
			}
			u.waiters[req.id] = append(u.waiters[req.id], req.done)
			// start it right away instead of on the next tick
			if session.getState() == uploadNotStarted &&
				u.inFlight < maxUploadsInFlight && !u.auth.Expired() {
				u.inFlight++
				go session.Upload(u.auth)
			}

		case <-ticker.C: // periodically start uploads, or remove them if done/failed
			for _, session := range u.sessions {
				switch session.getState() {
				case uploadNotStarted:
					// max active upload sessions are capped at this limit for faster
					// uploads of individual files and also to prevent possible server-
					// side throttling that can cause errors.
					if u.inFlight < maxUploadsInFlight && !u.auth.Expired() {
						u.inFlight++
						go session.Upload(u.auth)
					}

				case uploadErrored:
					session.Lock()
					discard := session.discard
					session.Unlock()
					if discard {
						// deleted locally and the upload didn't go through:
						// nothing to retry and nothing to clean up
						u.finishUpload(session.OldID)
						continue
					}
					if u.auth.Expired() {
						// not the upload's fault: keep it, without counting a
						// retry, until the user has logged in again
						session.setState(uploadNotStarted, nil)
						continue
					}
					if isItemNotFound(session.lastErr()) && u.handleNotFound(session) {
						continue
					}
					session.retries++
					if session.retries > 5 {
						log.Error().
							Str("id", session.ID).
							Str("name", session.Name).
							Err(session).
							Int("retries", session.retries).
							Msg("Upload session failed too many times, cancelling session.")
						u.finishUpload(session.ID)
					}

					log.Warn().
						Str("id", session.ID).
						Str("name", session.Name).
						Err(session).
						Msg("Upload session failed, will retry from beginning.")
					session.cancel(u.auth) // cancel large sessions
					session.setState(uploadNotStarted, nil)

				case uploadComplete:
					session.Lock()
					discard := session.discard
					session.Unlock()
					if discard {
						u.discardUploaded(session)
						u.finishUpload(session.OldID)
						continue
					}
					log.Info().
						Str("id", session.ID).
						Str("oldID", session.OldID).
						Str("name", session.Name).
						Msg("Upload completed!")

					// ID changed during upload, move to new ID
					if session.OldID != session.ID {
						err := u.fs.MoveID(session.OldID, session.ID)
						if err != nil {
							log.Error().
								Str("id", session.ID).
								Str("oldID", session.OldID).
								Str("name", session.Name).
								Err(err).
								Msg("Could not move inode to new ID!")
						}
					}

					// inode will exist at the new ID now, but we check if inode
					// is nil to see if the item has been deleted since upload start
					if inode := u.fs.GetID(session.ID); inode != nil {
						inode.Lock()
						inode.DriveItem.ETag = session.ETag
						inode.Unlock()
					}

					// the old ID is the one that was used to add it to the queue.
					// cleanup the session.
					u.notifyWaiters(session.OldID, uploadResult{found: true, newID: session.ID})
					u.finishUpload(session.OldID)
				}
			}
		}
	}
}

// QueueUpload queues an item for upload.
func (u *UploadManager) QueueUpload(inode *Inode) error {
	data := u.fs.getInodeContent(inode)
	session, err := NewUploadSession(inode, data)
	if err == nil {
		u.queue <- session
	}
	return err
}

// CancelUpload is used to kill any pending uploads for a session
func (u *UploadManager) CancelUpload(id string) {
	u.deletionQueue <- id
}

// finishUpload is an internal method that gets called when a session is
// completed. It cancels the session if one was in progress, and then deletes
// it from both memory and disk.
// uploadWait asks the upload loop to report when the upload of id is done.
type uploadWait struct {
	id   string
	done chan uploadResult
}

// uploadResult is the outcome of WaitForUpload.
type uploadResult struct {
	found bool   // an upload for the item was queued or running
	newID string // the item's ID after a successful upload, "" if it failed
}

// WaitForUpload waits until a queued or running upload of the item with the
// given ID finishes, starting it immediately if it hasn't started yet. found
// is false if there is no such upload. On success the item has already been
// moved to its new (server) ID, which is returned.
func (u *UploadManager) WaitForUpload(id string) (newID string, found bool) {
	done := make(chan uploadResult, 1)
	u.waitQueue <- uploadWait{id: id, done: done}
	res := <-done
	return res.newID, res.found
}

// notifyWaiters reports the outcome of an upload to WaitForUpload callers.
// Only called from uploadLoop.
func (u *UploadManager) notifyWaiters(id string, res uploadResult) {
	for _, done := range u.waiters[id] {
		done <- res
	}
	delete(u.waiters, id)
}

func (u *UploadManager) finishUpload(id string) {
	// anyone still waiting gets "failed": the upload was cancelled or gave up
	u.notifyWaiters(id, uploadResult{found: true})
	if session, exists := u.sessions[id]; exists {
		session.cancel(u.auth)
	}
	u.db.Batch(func(tx *bolt.Tx) error {
		if b := tx.Bucket(bucketUploads); b != nil {
			b.Delete([]byte(id))
		}
		return nil
	})
	if u.inFlight > 0 {
		u.inFlight--
	}
	delete(u.sessions, id)
}

// handleNotFound deals with an upload that failed because the server doesn't
// know the item ID (404 itemNotFound), e.g. because an atomic save replaced
// it. Only called from uploadLoop. Returns true if the session was dealt
// with: dropped because the item no longer exists locally either (nothing is
// lost), or reset to be created again by parent ID and name. Ported from
// leifrossau/onedriver.
func (u *UploadManager) handleNotFound(session *UploadSession) bool {
	session.Lock()
	id, oldID, recreated := session.ID, session.OldID, session.recreated
	session.Unlock()
	inode := u.fs.GetID(oldID)
	if inode == nil {
		inode = u.fs.GetID(id)
	}
	if inode == nil {
		log.Debug().Str("id", id).Str("name", session.Name).
			Msg("Upload target no longer exists locally, dropping the upload.")
		u.finishUpload(oldID)
		return true
	}
	if recreated || isLocalID(id) || inode.IsDir() {
		return false
	}
	inode.RLock()
	name := inode.DriveItem.Name
	parentID := ""
	if inode.DriveItem.Parent != nil {
		parentID = inode.DriveItem.Parent.ID
	}
	inode.RUnlock()
	if name == "" || parentID == "" {
		return false
	}
	log.Warn().Str("id", id).Str("name", name).
		Msg("The server does not know this item (anymore), creating it again by name.")
	session.Lock()
	session.recreate, session.recreated = true, true
	session.Name, session.ParentID = name, parentID
	session.Unlock()
	session.cancel(u.auth)
	session.setState(uploadNotStarted, nil)
	return true
}

// discardUploaded deletes the server item that an upload created for a file
// that was deleted locally while the upload was running.
func (u *UploadManager) discardUploaded(session *UploadSession) {
	session.Lock()
	id, name := session.ID, session.Name
	session.Unlock()
	if isLocalID(id) {
		return // never reached the server
	}
	log.Info().Str("id", id).Str("name", name).
		Msg("File was deleted while it was being uploaded, deleting it on the server.")
	go func() {
		if err := graph.Remove(id, u.auth); err != nil && !graph.IsNotFound(err) {
			log.Error().Err(err).Str("id", id).Str("name", name).
				Msg("Could not delete a file that was deleted during its upload.")
		}
	}()
}
