// Package master makes uploads' masters: it runs a kind's recipe
// (media.Kind.Master) of registered hooks over each image and video
// upload's original and records the result, a lossless object of its own,
// or the original itself when no hook changed it. The identity recipe needs
// no run: the manifest writer records the original as its master.
package master

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/open-rails/contentkit/contentref"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// Config configures a Runner.
type Config struct {
	Manifests *mediastore.Manifests
	// Hooks are the worker's hooks by Name.
	Hooks   map[string]media.MasterHook
	TempDir string // scratch for originals and results; default os.TempDir()
	// WebP re-encodes an 8-bit image as lossless WebP (the image
	// processor's): a hook's PNG within WebP's size is stored as WebP.
	WebP func([]byte) ([]byte, error)
}

// Runner makes masters. It is idempotent: a current master is never made
// again.
type Runner struct {
	c   Config
	reg *mediastore.Registry
}

// New refuses a registry whose recipes name a hook that is not registered,
// or at another version (Check).
func New(c Config) (*Runner, error) {
	if c.Manifests == nil {
		return nil, errors.New("media/master: Config needs Manifests")
	}
	reg := c.Manifests.Registry()
	if err := Check(reg, c.Hooks); err != nil {
		return nil, err
	}
	return &Runner{c: c, reg: reg}, nil
}

// Check refuses a recipe of reg naming a hook hooks lacks, or at another
// version.
func Check(reg *mediastore.Registry, hooks map[string]media.MasterHook) error {
	for _, k := range reg.Config().Kinds {
		if k.Master == nil {
			continue
		}
		for _, s := range k.Master.Steps {
			h, ok := hooks[s.Hook]
			switch {
			case !ok:
				return fmt.Errorf("media/master: kind %q: master hook %q is not registered", k.Name, s.Hook)
			case h.Version() != s.Version:
				return fmt.Errorf("media/master: kind %q: master hook %q is version %q, its recipe names %q", k.Name, s.Hook, h.Version(), s.Version)
			}
		}
	}
	return nil
}

// Process makes ref's pending masters. A hook's failure keeps that upload's
// previous master and renditions; the others proceed.
func (r *Runner) Process(ctx context.Context, ref contentref.ContentRef) error {
	item, err := r.reg.Item(ref)
	if err != nil {
		return err
	}
	k := item.Kind()
	if k.Master.Identity() {
		return nil
	}
	m, _, err := r.c.Manifests.Get(ctx, ref)
	if errors.Is(err, mediastore.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	var errs []error
	for _, f := range m.Files {
		if !k.MasterPending(f) {
			continue
		}
		master, err := r.make(ctx, item, f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_, err = r.c.Manifests.EditExisting(ctx, ref, func(cur *mediastore.Manifest) error {
			g, ok := cur.Get(f.Path)
			if !ok || g.Blob != f.Blob || k.Recipe(g).FP() != master.Recipe {
				return mediastore.ErrUnchanged // replaced meanwhile: its next pass
			}
			cur.SetMaster(f.Path, master)
			return nil
		})
		if err != nil && !errors.Is(err, mediastore.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// FailPending fails the uploads whose first master an exhausted job could
// not make. One that has a master keeps it: a release never changes because
// a recipe's hook fails.
func (r *Runner) FailPending(ctx context.Context, ref contentref.ContentRef, cause error) error {
	item, err := r.reg.Item(ref)
	if err != nil || item.Kind().Master.Identity() {
		return err
	}
	_, err = r.c.Manifests.EditExisting(ctx, ref, func(cur *mediastore.Manifest) error {
		changed := false
		for _, f := range slices.Clone(cur.Files) {
			if item.Kind().MasterPending(f) && f.Master == nil {
				cur.SetFailed(f.Path, cause)
				changed = true
			}
		}
		if !changed {
			return mediastore.ErrUnchanged
		}
		return nil
	})
	if errors.Is(err, mediastore.ErrNotFound) {
		return nil
	}
	return err
}

// make runs f's recipe over its original and stores what changed.
func (r *Runner) make(ctx context.Context, item mediastore.Item, f media.File) (media.Master, error) {
	recipe := item.Kind().Recipe(f)
	dir, err := os.MkdirTemp(r.c.TempDir, "contentkit-master-")
	if err != nil {
		return media.Master{}, err
	}
	defer os.RemoveAll(dir)
	key, err := item.Blob(f.Blob)
	if err != nil {
		return media.Master{}, err
	}
	in := filepath.Join(dir, "original")
	if err := r.download(ctx, key, in); err != nil {
		return media.Master{}, err
	}
	cur, typ, changed := in, f.Type, false
	for i, step := range recipe.Steps {
		out := filepath.Join(dir, fmt.Sprintf("step-%d", i))
		res, err := r.c.Hooks[step.Hook].Process(ctx, media.MasterInput{Kind: item.Kind().Name, File: f.Path, Path: cur, Type: typ, Out: out, Params: step.Params})
		if err != nil {
			return media.Master{}, fmt.Errorf("media/master: %s of %s: %w", step.Hook, f.Path, err)
		}
		if !res.Unchanged {
			cur, typ, changed = out, res.Type, true
		}
	}
	master := media.Master{Blob: f.Blob, Recipe: recipe.FP(), Steps: slices.Clone(recipe.Steps)}
	if !changed {
		return master, nil // the original, with no object of its own
	}
	if cur, typ, err = r.webp(cur, typ, dir); err != nil {
		return media.Master{}, fmt.Errorf("media/master: %s: %w", f.Path, err)
	}
	dims, err := lossless(cur, typ, f.Type)
	if err != nil {
		return media.Master{}, fmt.Errorf("media/master: %s: %w", f.Path, err)
	}
	sum, size, err := digest(cur)
	if err != nil {
		return media.Master{}, err
	}
	blob := layout.BlobRef(layout.AreaInternal, sum)
	dst, err := item.Blob(blob)
	if err != nil {
		return media.Master{}, err
	}
	body, err := os.Open(cur)
	if err != nil {
		return media.Master{}, err
	}
	defer body.Close()
	if _, err := r.c.Manifests.Store().Put(ctx, dst, body, size, media.PutOptions{ContentType: typ, ChecksumSHA256: sum}); err != nil {
		return media.Master{}, err
	}
	_, of, _ := layout.SplitBlobRef(f.Blob)
	master.Blob, master.Of, master.Type, master.Size, master.W, master.H = blob, of, typ, size, dims.W, dims.H
	return master, nil
}

// webp stores an 8-bit PNG WebP can hold as lossless WebP.
func (r *Runner) webp(path, typ, dir string) (string, string, error) {
	if typ != media.MasterPNG || r.c.WebP == nil {
		return path, typ, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	d, ok := pngSize(b)
	if !ok || max(d.W, d.H) > media.MaxWebPSide || len(b) > 24 && b[24] != 8 {
		return path, typ, nil // too large for WebP, or 16-bit: kept as PNG
	}
	out, err := r.c.WebP(b)
	if err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, "master.webp")
	return path, media.MasterWebP, os.WriteFile(path, out, 0o600)
}

func (r *Runner) download(ctx context.Context, key, path string) error {
	rc, _, err := r.c.Manifests.Store().Get(ctx, key, media.GetOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, rc)
	return errors.Join(err, out.Close())
}

func digest(path string) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return h.Sum(nil), n, err
}

// lossless checks a hook's result for an upload of type original: an image
// as lossless WebP, or PNG when a side exceeds media.MaxWebPSide (or it is
// 16-bit); a video as FFV1 in Matroska. It returns an image's pixel size.
func lossless(path, typ, original string) (media.Dims, error) {
	f, err := os.Open(path)
	if err != nil {
		return media.Dims{}, err
	}
	defer f.Close()
	head := make([]byte, 1<<20)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return media.Dims{}, err
	}
	head = head[:n]
	image := len(original) > 6 && original[:6] == "image/"
	switch {
	case image && typ == media.MasterWebP:
		if d, ok := webpLossless(head); ok {
			return d, nil
		}
	case image && typ == media.MasterPNG:
		if d, ok := pngSize(head); ok && (max(d.W, d.H) > media.MaxWebPSide || head[24] != 8) {
			return d, nil
		}
	case !image && typ == media.MasterMatroska:
		if bytes.HasPrefix(head, []byte{0x1a, 0x45, 0xdf, 0xa3}) && ffv1(head) {
			return media.Dims{}, nil
		}
	}
	return media.Dims{}, fmt.Errorf("%w (got %s for %s)", media.ErrMasterFormat, typ, original)
}

// ffv1 finds FFV1 in a Matroska header: codec V_FFV1, or the Video for
// Windows mapping (fourcc FFV1) older FFmpeg muxers write.
func ffv1(head []byte) bool {
	return bytes.Contains(head, []byte("V_FFV1")) || bytes.Contains(head, []byte("V_MS/VFW/FOURCC")) && bytes.Contains(head, []byte("FFV1"))
}

// webpLossless is a lossless WebP's size: a VP8L image, no lossy chunk.
func webpLossless(b []byte) (media.Dims, bool) {
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return media.Dims{}, false
	}
	var d media.Dims
	for p := 12; p+8 <= len(b); {
		id, size := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:p+8]))
		body := b[p+8:]
		switch id {
		case "VP8 ", "ANMF":
			return media.Dims{}, false
		case "VP8L":
			if len(body) < 5 || body[0] != 0x2f {
				return media.Dims{}, false
			}
			v := binary.LittleEndian.Uint32(body[1:5])
			d = media.Dims{W: int(v&0x3fff) + 1, H: int(v>>14&0x3fff) + 1}
		}
		p += 8 + size + size&1
	}
	return d, d.W > 0
}

// pngSize is a PNG's size from its IHDR.
func pngSize(b []byte) (media.Dims, bool) {
	if len(b) < 29 || !bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) || string(b[12:16]) != "IHDR" {
		return media.Dims{}, false
	}
	return media.Dims{W: int(binary.BigEndian.Uint32(b[16:20])), H: int(binary.BigEndian.Uint32(b[20:24]))}, true
}
