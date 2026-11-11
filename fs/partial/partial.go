// Package partial serves reads from the start of a file that has not been
// downloaded yet, using HTTP range requests instead of a full download.
//
// File managers open every file in a folder whose type they can't tell from
// its name, and read the first few KB to sniff the content type. Downloading
// the whole file for that (onedriver's previous behaviour) can take minutes
// for large files and blocks the folder from being shown. Reads within the
// first HeadSize bytes are instead answered from a single range request, and
// anything else falls back to the normal, checksum-verified full download.
//
// Data fetched here is never written to the content cache, which only ever
// holds complete files whose checksum matched.
package partial

import (
	"fmt"
	"sync"
)

// HeadSize is how much of the start of a file can be served without a full
// download. Content-type sniffing reads a few KB; this leaves room for tools
// that read a little more (file(1), archive headers, kernel readahead).
const HeadSize = 1024 * 1024

// Action is how a read of a not-yet-downloaded file should be served.
type Action int

const (
	// Full means the file must be downloaded completely first.
	Full Action = iota
	// Head means the read can be answered from the start of the file.
	Head
)

func (a Action) String() string {
	if a == Head {
		return "head"
	}
	return "full"
}

// Plan decides how to serve a read of length bytes at offset from a file of
// the given size that is not in the cache. Files no bigger than HeadSize are
// always downloaded in full, since that costs a single request anyway and
// leaves the file cached.
func Plan(size, offset uint64, length int) Action {
	if size <= HeadSize || length < 0 {
		return Full
	}
	if offset+uint64(length) <= HeadSize {
		return Head
	}
	return Full
}

// Fetcher retrieves bytes [0, end] (inclusive) of an item.
type Fetcher func(end uint64) ([]byte, error)

// Buffer holds the first HeadSize bytes of one item, tied to the item's
// content hash so a changed file is never served from an old buffer.
type Buffer struct {
	mu   sync.Mutex
	hash string
	data []byte
}

// Read returns up to length bytes at offset from the start of the item with
// the given content hash and size, fetching the head with fetch if it isn't
// buffered for that hash. Concurrent callers share a single fetch.
func (b *Buffer) Read(hash string, size, offset uint64, length int, fetch Fetcher) ([]byte, error) {
	if hash == "" {
		return nil, fmt.Errorf("item has no content hash")
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	want := uint64(HeadSize)
	if size < want {
		want = size
	}
	if b.hash != hash || uint64(len(b.data)) != want {
		b.hash, b.data = "", nil
		if want > 0 {
			data, err := fetch(want - 1)
			if err != nil {
				return nil, err
			}
			if uint64(len(data)) != want {
				return nil, fmt.Errorf("range request returned %d bytes, wanted %d",
					len(data), want)
			}
			b.data = data
		}
		b.hash = hash
	}

	if offset >= uint64(len(b.data)) {
		return []byte{}, nil
	}
	end := offset + uint64(length)
	if end > uint64(len(b.data)) {
		end = uint64(len(b.data))
	}
	// copy so callers can't alias the shared buffer
	out := make([]byte, end-offset)
	copy(out, b.data[offset:end])
	return out, nil
}

// Reset drops any buffered data, for example once the full file is cached.
func (b *Buffer) Reset() {
	b.mu.Lock()
	b.hash, b.data = "", nil
	b.mu.Unlock()
}
