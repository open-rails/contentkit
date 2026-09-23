// Package image derives WebP variants, public slots and zip downloads with
// libvips (CGO). A Processor runs one media.ProcessJob: it fills an item's
// missing or stale variants in one manifest edit and re-encodes its public
// slots. Originals are read, never served.
package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// SpecChooser returns the variants a file gets, by name: a host's per-file
// choice such as taller manhwa pages or a blurred teaser. The default is the
// kind's Specs.
type SpecChooser func(kind media.Kind, f media.File) map[string]media.Spec

// Config configures a Processor.
type Config struct {
	Store     media.Store
	Kinds     *media.Registry
	Manifests *media.Manifests
	Specs     SpecChooser
	Hooks     media.Hooks // Failed
	Workers   int         // sources encoded at once; default 2
	MaxPixels int         // largest source decoded; default 100 MP
	TempDir   string      // zip scratch; default os.TempDir()
}

// Processor derives images. It is safe for concurrent use and idempotent:
// a variant of an unchanged source and spec is never recomputed.
type Processor struct {
	c Config
}

func New(c Config) (*Processor, error) {
	if c.Store == nil || c.Kinds == nil || c.Manifests == nil {
		return nil, errors.New("media/image: Processor needs a Store, Kinds and Manifests")
	}
	if c.Specs == nil {
		c.Specs = func(k media.Kind, _ media.File) map[string]media.Spec { return k.Specs }
	}
	if c.Workers <= 0 {
		c.Workers = 2
	}
	if c.MaxPixels <= 0 {
		c.MaxPixels = 100_000_000
	}
	if err := start(); err != nil {
		return nil, fmt.Errorf("media/image: libvips: %w", err)
	}
	return &Processor{c: c}, nil
}

// Process runs one job: a Slot job re-encodes that slot; otherwise the
// ref's manifest (when the ref addresses one) and every uploaded slot are
// brought up to date.
func (p *Processor) Process(ctx context.Context, job media.ProcessJob) error {
	item, err := p.c.Kinds.Item(job.Ref)
	if err != nil {
		return err
	}
	if job.Slot != "" {
		return p.slot(ctx, item, job.Slot)
	}
	var errs []error
	if _, err := item.ManifestKey(); err == nil {
		errs = append(errs, p.manifest(ctx, item))
	}
	for slot := range item.Kind().Slots {
		errs = append(errs, p.slot(ctx, item, slot))
	}
	return errors.Join(errs...)
}

// derived holds a source's new variants by name.
type derived struct {
	variants map[string]media.Variant
	w, h     int
}

// manifest runs passes until the manifest needs no more work: a commit that
// lands while a pass runs is absorbed by this job, not queued again.
func (p *Processor) manifest(ctx context.Context, item media.Item) error {
	man, _, err := p.c.Manifests.Get(ctx, item.Ref())
	if errors.Is(err, media.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	failed := map[string]bool{} // sources that cannot be derived; reported once
	for range 8 {
		if len(p.todo(item.Kind(), man, failed)) == 0 && !p.zipStale(item.Kind(), man) {
			return nil
		}
		if man, err = p.pass(ctx, item, man, failed); err != nil {
			return err
		}
	}
	return fmt.Errorf("media/image: manifest of %s kept changing", item.Ref())
}

// todo maps each source to the variants it lacks or has under another spec.
func (p *Processor) todo(kind media.Kind, man *media.Manifest, failed map[string]bool) map[string]map[string]media.Spec {
	todo := map[string]map[string]media.Spec{}
	for _, f := range man.Files {
		if !isImage(f) || failed[f.Source()] {
			continue
		}
		for name, s := range p.c.Specs(kind, f) {
			if v, ok := f.Variants[name]; !ok || v.Spec != s.Hash() {
				if todo[f.Source()] == nil {
					todo[f.Source()] = map[string]media.Spec{}
				}
				todo[f.Source()][name] = s
			}
		}
	}
	return todo
}

// zipStale reports a zip whose inputs are complete but differ from the recorded
// one, or a recorded zip whose inputs are incomplete.
func (p *Processor) zipStale(kind media.Kind, man *media.Manifest) bool {
	if kind.Zip == "" {
		return false
	}
	_, inputs, ok := zipInputs(man, kind.Zip)
	d, has := man.Downloads["zip"]
	return ok && d.Inputs != inputs || !ok && has
}

// pass derives what man lacks, builds the zip when its inputs are complete,
// and records both in one manifest edit, returning the edited manifest.
func (p *Processor) pass(ctx context.Context, item media.Item, man *media.Manifest, failed map[string]bool) (*media.Manifest, error) {
	ref, kind := item.Ref(), item.Kind()
	todo := p.todo(kind, man, failed)

	var (
		mu      sync.Mutex
		results = map[string]derived{}
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.c.Workers)
	for source, specs := range todo {
		g.Go(func() error {
			d, err := p.derive(gctx, item, source, specs)
			if err != nil {
				if !isPermanent(err) {
					return err
				}
				p.failed(ctx, ref, fileOf(man, source), err)
				mu.Lock()
				failed[source] = true
				mu.Unlock()
				return nil
			}
			mu.Lock()
			results[source] = d
			mu.Unlock()
			return nil
		})
	}
	deriveErr := g.Wait()

	record := func(m *media.Manifest) {
		for i := range m.Files {
			f := &m.Files[i]
			if !isImage(*f) {
				continue
			}
			specs := p.c.Specs(kind, *f)
			for name := range f.Variants {
				if _, ok := specs[name]; !ok {
					delete(f.Variants, name)
				}
			}
			d, ok := results[f.Source()] // a replaced source keeps nothing derived from the old one
			if !ok {
				continue
			}
			for name, s := range specs {
				if v, ok := d.variants[name]; ok && v.Spec == s.Hash() {
					if f.Variants == nil {
						f.Variants = map[string]media.Variant{}
					}
					f.Variants[name] = v
				}
			}
			if f.Meta == nil {
				f.Meta = map[string]any{}
			}
			f.Meta["w"], f.Meta["h"] = d.w, d.h
		}
	}

	var zip *media.Download
	if kind.Zip != "" && deriveErr == nil {
		projected := clone(man)
		record(projected)
		if entries, inputs, ok := zipInputs(projected, kind.Zip); ok && projected.Downloads["zip"].Inputs != inputs {
			var err error
			if zip, err = p.buildZip(ctx, item, entries, inputs); err != nil {
				return nil, err
			}
		}
	}

	edited, err := p.c.Manifests.Edit(ctx, ref, func(m *media.Manifest) error {
		record(m)
		if kind.Zip == "" {
			return nil
		}
		_, inputs, ok := zipInputs(m, kind.Zip)
		switch {
		case ok && zip != nil && zip.Inputs == inputs:
			if m.Downloads == nil {
				m.Downloads = map[string]media.Download{}
			}
			m.Downloads["zip"] = *zip
		case !ok:
			delete(m.Downloads, "zip") // stale: some file lacks the zip variant
		}
		return nil
	})
	if err = errors.Join(deriveErr, err); err != nil {
		return nil, err
	}
	return edited, nil
}

// derive encodes one source into each spec and stores the blobs.
func (p *Processor) derive(ctx context.Context, item media.Item, source string, specs map[string]media.Spec) (derived, error) {
	key, err := item.Original(source)
	if err != nil {
		return derived{}, err
	}
	src, _, err := p.read(ctx, key)
	if errors.Is(err, media.ErrNotFound) {
		return derived{}, permanentError{err}
	} else if err != nil {
		return derived{}, err
	}
	d := derived{variants: make(map[string]media.Variant, len(specs))}
	if d.w, d.h, err = probe(src, p.c.MaxPixels); err != nil {
		return derived{}, err
	}
	for name, s := range specs {
		out, err := encode(src, s)
		if err != nil {
			return derived{}, err
		}
		blob, err := p.putBlob(ctx, item, bytes.NewReader(out), int64(len(out)), sha(out), "image/webp")
		if err != nil {
			return derived{}, err
		}
		d.variants[name] = media.Variant{Blob: blob, Spec: s.Hash(), Type: "image/webp", Size: int64(len(out))}
	}
	return d, nil
}

// putBlob stores an immutable, content-addressed blob unless it exists.
func (p *Processor) putBlob(ctx context.Context, item media.Item, body io.Reader, size int64, sum []byte, contentType string) (string, error) {
	name := media.SHA256Name(sum)
	key, err := item.Blob(name)
	if err != nil {
		return "", err
	}
	if obj, err := p.c.Store.Head(ctx, key); err == nil && obj.Size == size {
		return name, nil
	} else if err != nil && !errors.Is(err, media.ErrNotFound) {
		return "", err
	}
	opts := media.PutOptions{ContentType: contentType, CacheControl: "max-age=31536000, immutable", ChecksumSHA256: sum}
	if p.c.Store.Capabilities().ConditionalPut {
		opts.IfNoneMatch = "*"
	}
	if _, err := p.c.Store.Put(ctx, key, body, size, opts); err != nil && !errors.Is(err, media.ErrPreconditionFailed) {
		return "", err
	}
	return name, nil
}

func (p *Processor) read(ctx context.Context, key string) ([]byte, media.Object, error) {
	rc, obj, err := p.c.Store.Get(ctx, key, media.GetOptions{})
	if err != nil {
		return nil, media.Object{}, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return b, obj, err
}

func (p *Processor) failed(ctx context.Context, ref contentref.ContentRef, file string, err error) {
	if p.c.Hooks.Failed != nil {
		p.c.Hooks.Failed(ctx, ref, file, err)
	}
}

func isImage(f media.File) bool { return strings.HasPrefix(f.Type, "image/") }

func fileOf(m *media.Manifest, source string) string {
	for _, f := range m.Files {
		if f.Source() == source {
			return f.Name
		}
	}
	return source
}

func clone(m *media.Manifest) *media.Manifest {
	b, _ := json.Marshal(m)
	var out media.Manifest
	_ = json.Unmarshal(b, &out)
	return &out
}

func sha(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}
