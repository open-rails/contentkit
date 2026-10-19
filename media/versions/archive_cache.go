package versions

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"

	"github.com/open-rails/contentkit/media"
)

// archiveRevision covers every input that changes ZIP bytes. The recipe fixes
// Store compression, zero timestamps and positional naming. Source-format or
// bounded-download changes select different blobs; inclusion and order select
// different entries. It deliberately excludes the attachment's external title.
func archiveRevision(files []media.File) string {
	type entry struct{ Blob, Name, Type string }
	entries := make([]entry, len(files))
	for i, f := range files {
		entries[i] = entry{f.Blob, media.PositionalName(i+1, len(files), path.Ext(f.Path)), f.Type}
	}
	body, _ := json.Marshal(struct {
		Recipe string
		Files  []entry
	}{"reader-zip-store-1", entries})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (a *Archive) writeCached(w io.Writer) (int64, error) {
	n, cacheErr, err := streamCachedArchive(a.ctx, a.s.m.Manifests.Store(), a.cache, a.keys, a.names, w)
	if cacheErr != nil {
		// Cache writes are optional: a fully streamed valid ZIP stays successful
		// when storage cannot retain it. The next request simply builds again.
		a.s.o.Logger.WarnContext(a.ctx, "versions: archive cache write failed", "version", a.v.Version, "error", cacheErr)
	}
	return n, err
}

const archiveSpoolMaxBytes = 512 << 20

// One cache builder across all embedded engines keeps temporary cache storage
// at most 512 MiB per process (including hosts whose /tmp is a tmpfs). Busy and
// oversized downloads still stream; caching must never gate user transfers.
var archiveSpoolBudget = make(chan struct{}, 1)

// streamCachedArchive has no shared producer lifetime. Concurrent misses may
// stream independently, publishing the same deterministic bytes with an atomic
// create-only PUT. A failed/cancelled caller neither publishes partial output
// nor cancels another caller's transfer. temp/ is never opened by media tokens
// or gateway URLs, and the ordinary sweep expires these entries by TempTTL.
func streamCachedArchive(ctx context.Context, store media.Store, cache string, keys, names []string, dst io.Writer) (int64, error, error) {
	return streamArchive(ctx, store, cache, keys, names, dst, archiveSpoolBudget, archiveSpoolMaxBytes)
}

func streamArchive(ctx context.Context, store media.Store, cache string, keys, names []string, dst io.Writer, budget chan struct{}, maxBytes int64) (written int64, cacheErr, err error) {
	out := &archiveWriter{ctx: ctx, Writer: dst}
	if rc, _, err := store.Get(ctx, cache, media.GetOptions{}); err == nil {
		defer rc.Close()
		_, err = io.Copy(out, rc)
		return out.n, nil, err
	} else if !errors.Is(err, media.ErrNotFound) {
		return 0, nil, err
	}
	var spool archiveSpool
	writer := io.Writer(out)
	select {
	case budget <- struct{}{}:
		defer func() { <-budget }()
		spool.file, spool.err = os.CreateTemp("", "contentkit-download-*.zip")
		if spool.file != nil {
			// On POSIX the descriptor remains usable, but a forced process exit
			// also frees its disk space. Platforms that disallow unlinking an
			// open file retain the normal deferred removal below.
			_ = os.Remove(spool.file.Name())
		}
		spool.max = maxBytes
		defer spool.drop()
		writer = io.MultiWriter(&spool, out)
	default: // another request owns the disk budget; stream without a cache
	}
	zw := zip.NewWriter(writer)
	for i, key := range keys {
		if err := ctx.Err(); err != nil {
			return out.n, nil, err
		}
		rc, _, err := store.Get(ctx, key, media.GetOptions{})
		if err != nil {
			return out.n, nil, err
		}
		entry, err := zw.CreateHeader(&zip.FileHeader{Name: names[i], Method: zip.Store})
		if err == nil {
			_, err = io.Copy(&archiveWriter{ctx: ctx, Writer: entry}, rc)
		}
		closeErr := rc.Close()
		if err != nil || closeErr != nil {
			return out.n, nil, errors.Join(err, closeErr)
		}
	}
	if err := zw.Close(); err != nil {
		return out.n, nil, err
	}
	if err := ctx.Err(); err != nil {
		return out.n, nil, err
	}
	if spool.file == nil {
		return out.n, spool.err, nil
	}
	if _, err := spool.file.Seek(0, io.SeekStart); err != nil {
		return out.n, err, nil
	}
	_, err = store.Put(ctx, cache, spool.file, spool.size, media.PutOptions{ContentType: "application/zip", CacheControl: "private, no-store", IfNoneMatch: "*"})
	if errors.Is(err, media.ErrPreconditionFailed) { // another complete writer won
		err = nil
	}
	return out.n, err, nil
}

// archiveSpool stops writing and immediately removes its file on overflow or
// disk failure. Its optional cache must not make the actual ZIP transfer fail.
type archiveSpool struct {
	file      *os.File
	size, max int64
	err       error
}

func (s *archiveSpool) Write(p []byte) (int, error) {
	if s.file != nil {
		if int64(len(p)) > s.max-s.size {
			s.drop()
		} else {
			n, err := s.file.Write(p)
			s.size += int64(n)
			if err != nil || n != len(p) {
				s.err = errors.Join(err, io.ErrShortWrite)
				s.drop()
			}
		}
	}
	return len(p), nil
}

func (s *archiveSpool) drop() {
	if s.file != nil {
		s.file.Close()
		os.Remove(s.file.Name())
		s.file = nil
	}
}

type archiveWriter struct {
	ctx context.Context
	io.Writer
	n int64
}

func (w *archiveWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.Writer.Write(p)
	w.n += int64(n)
	return n, err
}
