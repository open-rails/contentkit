package image

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"io"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/open-rails/contentkit/media"
)

// staleZips are the Zip presets whose output does not pack their current
// inputs, once no upload feeding their prefix is pending.
func (p *Processor) staleZips(item media.Item, m *media.Manifest, job media.ProcessJob) []*media.Private {
	k := item.Kind()
	var out []*media.Private
	for i := range k.Private {
		z := &k.Private[i]
		if z.Zip == "" || job.Preset != "" && z.Name != job.Preset || !k.ZipStale(m, z) && !job.Force {
			continue
		}
		pending := false
		for _, f := range m.Files {
			for _, pr := range k.PrivateFor(f.Path) {
				pending = pending || f.IsUpload() && !f.Unattached && strings.HasPrefix(k.OutputPath(pr, f.Path), z.Zip) && slices.Contains(f.Pending, pr.Name)
			}
		}
		if !pending {
			out = append(out, z)
		}
	}
	return out
}

// buildZip streams z's inputs, in manifest order, into a stored
// (uncompressed) zip in a temp file and uploads it at its content address.
// ok is false without inputs.
func (p *Processor) buildZip(ctx context.Context, item media.Item, m *media.Manifest, z *media.Private) (media.File, bool, error) {
	inputs := item.Kind().ZipInputs(m, z)
	if len(inputs) == 0 {
		return media.File{}, false, nil
	}
	f, err := os.CreateTemp(p.c.TempDir, "contentkit-zip-*")
	if err != nil {
		return media.File{}, false, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	zw := zip.NewWriter(io.MultiWriter(f, h))
	for _, in := range inputs {
		key, err := item.Blob(in.Blob)
		if err != nil {
			return media.File{}, false, err
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: path.Base(in.Path), Method: zip.Store})
		if err != nil {
			return media.File{}, false, err
		}
		rc, _, err := p.c.Store.Get(ctx, key, media.GetOptions{})
		if err != nil {
			return media.File{}, false, err
		}
		_, err = io.Copy(w, rc)
		rc.Close()
		if err != nil {
			return media.File{}, false, err
		}
	}
	if err := zw.Close(); err != nil {
		return media.File{}, false, err
	}
	size, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return media.File{}, false, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return media.File{}, false, err
	}
	blob, err := p.putBlob(ctx, item, f, size, h.Sum(nil), "application/zip")
	if err != nil {
		return media.File{}, false, err
	}
	return media.File{Path: z.To, Blob: blob, Type: "application/zip", Size: size, FP: media.ZipFP(inputs)}, true, nil
}
