package fs

import (
	"io"
	"os"
	"strings"

	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/jstaf/onedriver/fs/graph"
	"github.com/jstaf/onedriver/fs/partial"
	"github.com/rs/zerolog"
)

// contentHash returns the server's QuickXorHash for an inode. Caller must hold
// the inode lock.
func contentHash(inode *Inode) string {
	if inode.DriveItem.File == nil {
		return ""
	}
	return inode.DriveItem.File.Hashes.QuickXorHash
}

// cacheIsValid reports whether fd holds exactly the content the server says
// the inode has. A cache file whose checksum was already verified and which
// hasn't changed since (same size and mtime) is trusted without re-hashing.
// Caller must hold the inode write lock.
func (f *Filesystem) cacheIsValid(inode *Inode, fd *os.File) (bool, error) {
	hash := contentHash(inode)
	st, err := fd.Stat()
	if err != nil {
		return false, err
	}
	v := inode.verified
	if hash != "" && strings.EqualFold(v.hash, hash) &&
		v.size == st.Size() && v.mtime.Equal(st.ModTime()) {
		return true, nil
	}
	if !inode.VerifyChecksum(graph.QuickXORHashStream(fd)) {
		return false, nil
	}
	inode.verified = verifiedContent{hash: hash, size: st.Size(), mtime: st.ModTime()}
	return true, nil
}

// downloadLocked downloads an inode's full content into fd, replacing what's
// there only if the download's checksum matches. Caller must hold the inode
// write lock.
func (f *Filesystem) downloadLocked(inode *Inode, fd *os.File, ctx zerolog.Logger) fuse.Status {
	id := inode.DriveItem.ID

	// write to tempfile first to ensure our download is good
	tempID := "temp-" + id
	temp, err := f.content.Open(tempID)
	if err != nil {
		ctx.Error().Err(err).Msg("Failed to create tempfile for download.")
		return fuse.EIO
	}
	defer f.content.Delete(tempID)

	// replace content only on a match
	size, err := graph.GetItemContentStream(id, f.auth, temp)
	if graph.IsNotFound(err) {
		// deleted on the server and we missed the delta: forget it once the
		// caller has released the inode lock (DeleteID takes it)
		ctx.Warn().Msg("Item no longer exists on the server, removing it locally.")
		go f.forgetDeleted(id)
		return fuse.ENOENT
	}
	if err != nil || !inode.VerifyChecksum(graph.QuickXORHashStream(temp)) {
		ctx.Error().Err(err).Msg("Failed to fetch remote content.")
		return fuse.EREMOTEIO
	}
	temp.Seek(0, 0) // being explicit, even though already done in hashstream func
	fd.Seek(0, 0)
	fd.Truncate(0)
	if _, err := io.Copy(fd, temp); err != nil {
		ctx.Error().Err(err).Msg("Failed to copy downloaded content into cache.")
		return fuse.EIO
	}
	inode.DriveItem.Size = size
	if st, err := fd.Stat(); err == nil {
		inode.verified = verifiedContent{
			hash:  contentHash(inode),
			size:  st.Size(),
			mtime: st.ModTime(),
		}
	}
	inode.needsDownload = false
	inode.head.Reset()
	return fuse.OK
}

// ensureContent completes a download that Open deferred, so the cache file
// can be used for reads and writes. Returns immediately if nothing is pending.
func (f *Filesystem) ensureContent(inode *Inode, ctx zerolog.Logger) fuse.Status {
	inode.Lock()
	defer inode.Unlock()
	if !inode.needsDownload {
		return fuse.OK
	}
	fd, err := f.content.Open(inode.DriveItem.ID)
	if err != nil {
		ctx.Error().Err(err).Msg("Could not open cache file.")
		return fuse.EIO
	}
	// another download may have filled the cache in the meantime
	if valid, err := f.cacheIsValid(inode, fd); err == nil && valid {
		inode.needsDownload = false
		inode.head.Reset()
		return fuse.OK
	}
	ctx.Info().Msg("Downloading content deferred from Open().")
	return f.downloadLocked(inode, fd, ctx)
}

// readPending serves a read of a file whose download was deferred. Reads near
// the start of the file are answered with a range request; anything else
// downloads the whole file first. ok is false when the caller should go on
// to read from the (now complete) cache file.
func (f *Filesystem) readPending(inode *Inode, in *fuse.ReadIn, ctx zerolog.Logger) (data []byte, ok bool, status fuse.Status) {
	inode.RLock()
	pending := inode.needsDownload
	id := inode.DriveItem.ID
	size := inode.DriveItem.Size
	hash := contentHash(inode)
	inode.RUnlock()
	if !pending {
		return nil, false, fuse.OK
	}

	if partial.Plan(size, in.Offset, int(in.Size)) == partial.Head {
		data, err := inode.head.Read(hash, size, in.Offset, int(in.Size),
			func(end uint64) ([]byte, error) {
				ctx.Info().Uint64("bytes", end+1).
					Msg("Serving start of file with a range request instead of a full download.")
				return graph.GetItemContentRange(id, f.auth, 0, end)
			})
		if err == nil {
			return data, true, fuse.OK
		}
		ctx.Warn().Err(err).Msg("Range request failed, falling back to full download.")
	}
	return nil, false, f.ensureContent(inode, ctx)
}

// forgetDeleted removes an item that no longer exists on the server from the
// local metadata and content caches.
func (f *Filesystem) forgetDeleted(id string) {
	f.DeleteID(id)
	f.content.Delete(id)
}
