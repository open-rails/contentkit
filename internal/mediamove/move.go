// Package mediamove moves a host's media from a bucket of the pre-v4 model
// (journal, allocations, `sha256-{hex}-{uuid}` names, release copies) into a
// new bucket that never had versioning: for each item it reads the old
// manifest, copies the live originals and renditions server-side under their
// hash names (no re-encoding), writes the v4 manifest and verifies every
// referenced object's size. It reads the old bucket only. Re-running it
// skips items already moved.
package mediamove

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"

	"github.com/open-rails/contentkit/contentref"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
)

// Options configure a move.
type Options struct {
	// Source is the old bucket; Dest the new one, on the same endpoint, whose
	// key can read Source. Manifests writes Dest's manifests.
	Source    *mediaS3.Store
	Dest      *mediaS3.Store
	Manifests *mediastore.Manifests
	// Contents maps version ids to their content ids: a version's item
	// folder names only the version. Read from the catalog.
	Contents func(ctx context.Context, versions []string) (map[string]string, error)
	Kinds    []string // default every kind of the registry
	Logger   *slog.Logger
}

// Report is what a move did.
type Report struct {
	Items   int   // items moved
	Already int   // items moved by an earlier run
	Erased  int   // erased items (tombstones), left behind
	Files   int   // objects referenced by the new manifests
	Bytes   int64 // their bytes
	Dropped []string
}

// Move moves every item of opts.Kinds.
func Move(ctx context.Context, opts Options) (Report, error) {
	var r Report
	if opts.Source == nil || opts.Dest == nil || opts.Manifests == nil || opts.Contents == nil {
		return r, errors.New("mediamove: Source, Dest, Manifests and Contents are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := mediastore.RequireWritable(ctx, opts.Dest); err != nil {
		return r, fmt.Errorf("mediamove: destination: %w", err)
	}
	reg := opts.Manifests.Registry()
	kinds := opts.Kinds
	if len(kinds) == 0 {
		for _, k := range reg.Kinds() {
			kinds = append(kinds, k.Name)
		}
	}
	for _, name := range kinds {
		k, err := reg.Kind(name)
		if err != nil {
			return r, err
		}
		root := k.DefaultsItem().Prefix()
		root = strings.TrimSuffix(root, layout.DefaultID+"/")
		for folder, err := range opts.Source.Folders(ctx, root) {
			if err != nil {
				return r, err
			}
			key, ok := layout.ParseFolder(folder)
			if !ok || key.ID == layout.DefaultID {
				continue
			}
			if err := moveItem(ctx, opts, k, folder, key.ID, &r); err != nil {
				return r, fmt.Errorf("mediamove: %s: %w", folder, err)
			}
		}
	}
	return r, nil
}

// oldManifest is the part of a v3 manifest a move reads.
type oldManifest struct {
	V          int            `json:"v"`
	Deleted    bool           `json:"deleted"`
	Meta       map[string]any `json:"meta"`
	Files      []oldFile      `json:"files"`
	Deliveries []struct {
		Ready, Adopted, Withdrawn bool
		Files                     []struct {
			Source string `json:"source"`
			Blob   string `json:"blob"`
		} `json:"files"`
	} `json:"deliveries"`
}

type oldFile struct {
	media.File
	Public []struct {
		Preset     string       `json:"preset"`
		FP         string       `json:"fp"`
		Generation string       `json:"generation"`
		Blobs      []string     `json:"blobs"`
		Names      []string     `json:"names"`
		State      int          `json:"state"`
		Dims       []media.Dims `json:"dims"`
	} `json:"public"`
}

// oldBlob parses a v3 blob reference: "sha256-{hex}[-{uuid}]" in private/,
// or "{area}/sha256-{hex}[-{uuid}]" (originals, internal, public).
func oldBlob(ref string) (area, name, digest string, ok bool) {
	area, name = layout.AreaPrivate, ref
	if a, n, qualified := strings.Cut(ref, "/"); qualified {
		area, name = a, n
	}
	h, found := strings.CutPrefix(name, "sha256-")
	if !found || len(h) < 64 || !layout.ValidHash(h[:64]) {
		return "", "", "", false
	}
	return area, name, h[:64], true
}

// newArea is where an old area's files live now: originals are internal.
func newArea(old string) string {
	if old == "originals" {
		return layout.AreaInternal
	}
	return old
}

func moveItem(ctx context.Context, opts Options, k *mediastore.RegisteredKind, folder, id string, r *Report) error {
	body, _, err := opts.Source.Get(ctx, folder+layout.ManifestName, media.GetOptions{})
	if errors.Is(err, mediastore.ErrNotFound) {
		return nil // never committed: nothing to move
	} else if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(body, media.MaxManifestBytes+1))
	body.Close()
	if err != nil {
		return err
	}
	old, err := decodeOld(raw)
	if err != nil {
		return err
	}
	if old.Deleted {
		r.Erased++
		return nil
	}
	ref, err := itemRef(ctx, opts, k, id)
	if err != nil {
		return err
	}
	item, err := opts.Manifests.Registry().Item(ref)
	if err != nil {
		return err
	}
	if item.Prefix() != folder {
		return fmt.Errorf("item folder %s is not the registry's %s", folder, item.Prefix())
	}
	if cur, _, err := opts.Manifests.GetAny(ctx, ref); err == nil && cur.Rev > 0 {
		r.Already++
		return nil
	} else if err != nil && !errors.Is(err, mediastore.ErrNotFound) {
		return err
	}
	// A released file was served from its delivery copy: keep it released.
	served := map[string]string{}
	for _, d := range old.Deliveries {
		if !d.Ready || d.Withdrawn {
			continue
		}
		for _, f := range d.Files {
			if area, _, _, ok := oldBlob(f.Blob); ok && layout.Served(area) {
				served[f.Source] = f.Blob
			}
		}
	}
	copies := map[string]string{} // new key -> old key
	place := func(ref string) (string, bool) {
		if s, ok := served[ref]; ok {
			ref = s
		}
		area, name, digest, ok := oldBlob(ref)
		if !ok {
			return "", false
		}
		blob := newArea(area) + "/" + digest
		copies[folder+blob] = folder + area + "/" + name
		return blob, true
	}
	var files []media.File
	for _, of := range old.Files {
		f := of.File
		switch {
		case f.Staged != "":
			r.Dropped = append(r.Dropped, folder+f.Path+" (still staged)")
			continue
		case !f.IsUpload() && k.Area(f.Preset) != layout.AreaPublic && !hasPrivate(k, f.Preset):
			r.Dropped = append(r.Dropped, folder+f.Path+" (preset "+f.Preset+" removed)")
			continue
		}
		f.Editor = nil // re-rendered on demand
		if f.Blob != "" && !f.Gone {
			blob, ok := place(f.Blob)
			if !ok {
				return fmt.Errorf("file %s: unreadable blob %q", f.Path, f.Blob)
			}
			f.Blob = blob
		}
		if f.Track != nil && f.Track.Index != "" {
			track := *f.Track
			blob, ok := place(track.Index)
			if !ok {
				return fmt.Errorf("file %s: unreadable track index %q", f.Path, track.Index)
			}
			track.Index = blob
			f.Track = &track
		}
		if f.Admission != nil {
			f.Admission = &media.SourceAdmission{Generation: f.Admission.Generation, Target: f.Admission.Target}
		}
		files = append(files, f)
		// A public preset's ready generation becomes the upload's outputs,
		// named by the hash of the bytes the gateway served.
		for _, pub := range of.Public {
			if pub.State != 1 || !hasPublic(k, pub.Preset) {
				continue
			}
			for i, name := range pub.Names {
				var src string
				if i < len(pub.Blobs) {
					if s, ok := served[pub.Blobs[i]]; ok {
						src = s
					} else {
						src = pub.Blobs[i]
					}
				}
				var digest string
				var oldKey string
				if area, n, d, ok := oldBlob(src); ok {
					digest, oldKey = d, folder+area+"/"+n
				} else {
					physical := name
					if pub.Generation != "" {
						ext := path.Ext(name)
						physical = strings.TrimSuffix(name, ext) + "-" + pub.Generation + ext
					}
					oldKey = folder + layout.AreaPublic + "/" + physical
					if digest, err = hashObject(ctx, opts.Source, oldKey); err != nil {
						return err
					}
				}
				blob := layout.AreaPublic + "/" + digest
				copies[folder+blob] = oldKey
				out := media.File{Path: name, Blob: blob, Type: "image/webp", From: f.Path, Preset: pub.Preset, FP: pub.FP}
				if i < len(pub.Dims) {
					out.W, out.H = pub.Dims[i].W, pub.Dims[i].H
				}
				files = append(files, out)
			}
		}
	}
	// Copy, then size every copy from its source.
	sizes := map[string]int64{}
	for dst, src := range copies {
		head, err := opts.Source.Head(ctx, src)
		if err != nil {
			return fmt.Errorf("source %s: %w", src, err)
		}
		if existing, err := opts.Dest.Head(ctx, dst); err == nil && existing.Size == head.Size {
			sizes[dst] = head.Size
			continue
		}
		if _, err := opts.Dest.CopyFrom(ctx, opts.Source.Bucket(), head, dst); err != nil {
			return err
		}
		sizes[dst] = head.Size
	}
	for i := range files {
		if files[i].Blob != "" && files[i].Size == 0 {
			files[i].Size = sizes[folder+files[i].Blob]
		}
	}
	_, err = opts.Manifests.EditItem(ctx, item, mediastore.EditOptions{Op: "move-v3"}, func(m *mediastore.Manifest) error {
		if m.Rev > 0 {
			return mediastore.ErrUnchanged
		}
		m.Meta = old.Meta
		m.Files = files
		return nil
	})
	if err != nil {
		return err
	}
	// Verify: every object the new manifest names is there, at its size.
	for dst, size := range sizes {
		obj, err := opts.Dest.Head(ctx, dst)
		if err != nil {
			return fmt.Errorf("verify %s: %w", dst, err)
		}
		if obj.Size != size {
			return fmt.Errorf("verify %s: %d bytes, source %d", dst, obj.Size, size)
		}
		r.Files++
		r.Bytes += size
	}
	r.Items++
	return nil
}

func hasPrivate(k *mediastore.RegisteredKind, preset string) bool {
	for _, p := range k.Private {
		if p.Name == preset {
			return true
		}
	}
	return false
}

func hasPublic(k *mediastore.RegisteredKind, preset string) bool {
	for _, p := range k.Public {
		if p.Name == preset {
			return true
		}
	}
	return false
}

// itemRef is the ref of item id of k: a version's names its content.
func itemRef(ctx context.Context, opts Options, k *mediastore.RegisteredKind, id string) (contentref.ContentRef, error) {
	reg := opts.Manifests.Registry()
	if !k.Versions {
		return reg.Ref(k.Name, id)
	}
	contents, err := opts.Contents(ctx, []string{id})
	if err != nil {
		return contentref.ContentRef{}, err
	}
	content, ok := contents[id]
	if !ok {
		return contentref.ContentRef{}, fmt.Errorf("version %s has no content in the catalog", id)
	}
	return reg.VersionRef(k.Name, content, id)
}

// hashObject reads an object and returns its SHA-256.
func hashObject(ctx context.Context, store media.Store, key string) (string, error) {
	rc, _, err := store.Get(ctx, key, media.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read %s: %w", key, err)
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// decodeOld reads a v3 manifest (gzip JSON or plain JSON).
func decodeOld(b []byte) (*oldManifest, error) {
	r := io.Reader(bytes.NewReader(b))
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	}
	var m oldManifest
	if err := json.NewDecoder(io.LimitReader(r, media.MaxManifestBytes+1)).Decode(&m); err != nil {
		return nil, err
	}
	if m.V != 3 {
		return nil, fmt.Errorf("manifest version %d, want 3", m.V)
	}
	return &m, nil
}
