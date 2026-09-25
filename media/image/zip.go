package image

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"github.com/open-rails/contentkit/media"
)

type zipEntry struct{ name, blob string }

// zipInputs lists the zip's entries, one per image file in order, and hashes
// them. ok is false when there are none or a file lacks the variant.
func zipInputs(m *media.Manifest, variant string) ([]zipEntry, string, bool) {
	var blobs []string
	for _, f := range m.Files {
		if !isImage(f) || f.Unattached {
			continue
		}
		v, ok := f.Variants[variant]
		if !ok {
			return nil, "", false
		}
		blobs = append(blobs, v.Blob)
	}
	if len(blobs) == 0 {
		return nil, "", false
	}
	width := max(3, len(fmt.Sprint(len(blobs))))
	h := sha256.New()
	entries := make([]zipEntry, len(blobs))
	for i, b := range blobs {
		entries[i] = zipEntry{name: fmt.Sprintf("%0*d.webp", width, i+1), blob: b}
		fmt.Fprintf(h, "%s\x00%s\n", entries[i].name, b)
	}
	return entries, media.SHA256Name(h.Sum(nil)), true
}

// buildZip streams the entries' blobs into a stored (uncompressed) zip in a
// temp file and uploads it as a content-addressed blob. The display name is
// chosen at read time (Hooks.DownloadName).
func (p *Processor) buildZip(ctx context.Context, item media.Item, entries []zipEntry, inputs string) (*media.Download, error) {
	f, err := os.CreateTemp(p.c.TempDir, "contentkit-zip-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	zw := zip.NewWriter(io.MultiWriter(f, h))
	for _, e := range entries {
		key, err := item.Blob(e.blob)
		if err != nil {
			return nil, err
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: e.name, Method: zip.Store})
		if err != nil {
			return nil, err
		}
		rc, _, err := p.c.Store.Get(ctx, key, media.GetOptions{})
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(w, rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	size, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	blob, err := p.putBlob(ctx, item, f, size, h.Sum(nil), "application/zip")
	if err != nil {
		return nil, err
	}
	return &media.Download{Blob: blob, Type: "application/zip", Size: size, Inputs: inputs}, nil
}
