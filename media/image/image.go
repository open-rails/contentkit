// Package image is the media worker's image producer (libvips, CGO): the
// Image presets' private WebP files, the Zip presets, the public presets'
// owned generations, editor views, and the kinds' default public images. A
// Processor runs one media.ProcessJob: it redoes stale or pending outputs,
// records them in one manifest edit per pass, and keeps public/ in step.
package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// Recipe versions the image producer: a change re-renders every image output.
const Recipe = "webp-1"

// Config configures a Processor.
type Config struct {
	Store     media.Store
	Manifests *media.Manifests
	Workers   int // uploads decoded at once; default 2
	// MaxPixels bounds a decoded source, all frames of an animation
	// together (default 100 MP); MaxFrames and MaxAnimationSeconds bound
	// animations (defaults 1000 and 60).
	MaxPixels           int
	MaxFrames           int
	MaxAnimationSeconds float64
	TempDir             string // zip scratch; default os.TempDir()
}

// Processor runs image jobs. It is safe for concurrent use and idempotent:
// an output whose fingerprint matches is never redone (unless forced).
type Processor struct {
	c   Config
	reg *media.Registry
}

func New(c Config) (*Processor, error) {
	if c.Store == nil || c.Manifests == nil {
		return nil, errors.New("media/image: Processor needs a Store and Manifests")
	}
	if c.Workers <= 0 {
		c.Workers = 2
	}
	if c.MaxPixels <= 0 {
		c.MaxPixels = 100_000_000
	}
	if c.MaxFrames <= 0 {
		c.MaxFrames = 1000
	}
	if c.MaxAnimationSeconds <= 0 {
		c.MaxAnimationSeconds = 60
	}
	if err := start(); err != nil {
		return nil, fmt.Errorf("media/image: libvips: %w", err)
	}
	return &Processor{c: c, reg: c.Manifests.Registry()}, nil
}

// publicSpec is what a public preset's fingerprint covers.
type publicSpec struct {
	To     string      `json:"to"`
	Widths []int       `json:"widths"`
	Image  media.Image `json:"image"`
}

func publicFP(src media.File, p *media.Public) string {
	return media.SpecFP(src, publicSpec{p.To, p.Widths, p.Image}, Recipe)
}

// Process brings ref's image outputs up to date. Passes repeat until the
// manifest needs no more work, so a commit landing during a pass is absorbed.
func (p *Processor) Process(ctx context.Context, job media.ProcessJob) error {
	item, err := p.reg.Item(job.Ref)
	if err != nil {
		return err
	}
	forceZips := job.Force
	for range 8 {
		m, _, err := p.c.Manifests.Get(ctx, job.Ref)
		if errors.Is(err, media.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		if err := p.syncPublic(ctx, item); err != nil {
			return err
		}
		todo, err := p.todo(ctx, item, m, job)
		if err != nil {
			return err
		}
		zipJob := job
		zipJob.Force = forceZips
		zips := p.staleZips(item, m, zipJob)
		if m.Full {
			// No record of a private output or zip fits. Public files still
			// render when their existing reservation can be replaced without growth.
			zips = nil
			todo = slices.DeleteFunc(todo, func(w work) bool { return len(w.public) == 0 })
			for i := range todo {
				todo[i].private, todo[i].measure, todo[i].analyze = nil, false, false
			}
		}
		if len(todo) == 0 && len(zips) == 0 {
			if job.Editor {
				return p.editorViews(ctx, item, m)
			}
			return nil
		}
		// ZIP inputs must be the published renditions, not the outputs from
		// the snapshot taken before rendering. A concurrent takedown can
		// retire that snapshot's allocations while the new ones are written.
		if len(todo) > 0 {
			zips = nil
		} else {
			forceZips = false
		}
		if len(todo) > 0 {
			// Each source commits and reports when it is ready. Keep the existing
			// source concurrency; no whole-gallery completion barrier hides progress.
			g, workCtx := errgroup.WithContext(ctx)
			g.SetLimit(p.c.Workers)
			for _, source := range todo {
				g.Go(func() error {
					if err := p.pass(workCtx, item, m, []work{source}, nil); err != nil {
						return err
					}
					return media.CompleteProcessing(workCtx, p.c.Manifests, item.Ref(), nil)
				})
			}
			err = g.Wait()
		} else {
			err = p.pass(ctx, item, m, nil, zips)
		}
		if err != nil {
			if errors.Is(err, media.ErrManifestTooLarge) {
				return p.c.Manifests.SetFull(ctx, item.Ref(), err)
			}
			return err
		}

		if m.Full {
			// One pass: a public render that failed has no record to stop
			// the next (none fits), so it is not tried again in this job.
			if job.Editor {
				return p.editorViews(ctx, item, m)
			}
			return nil
		}
		job.Force = false // forced once
	}
	return fmt.Errorf("media/image: manifest of %s kept changing", job.Ref)
}

// FailPending records an exhausted job's remaining sources, preserving completed work.
func (p *Processor) FailPending(ctx context.Context, job media.ProcessJob, cause error) error {
	item, err := p.reg.Item(job.Ref)
	if err != nil {
		return err
	}
	job.Force = false
	_, err = p.c.Manifests.EditExisting(ctx, job.Ref, func(cur *media.Manifest) error {
		todo, err := p.todo(ctx, item, cur, job)
		if err != nil {
			return err
		}
		for _, w := range todo {
			cur.SetFailed(w.src.Path, cause)
		}
		if job.Editor {
			for _, f := range cur.Files {
				if f.Fail() == nil && p.reg.EditorFingerprint(f) != "" && p.reg.EditorView(f) == "" {
					cur.SetFailed(f.Path, cause)
				}
			}
		}
		return nil
	})
	if errors.Is(err, media.ErrNotFound) {
		return nil
	}
	return err
}

// work is one upload's image outputs to render.
type work struct {
	src     media.File
	private []*media.Private
	public  []*media.Public
	measure bool // record its size (W×H) only
	analyze bool // record original-page evidence, independently of renditions
}

// done is a rendered upload.
type done struct {
	work
	outputs      map[string]media.File // by private preset
	publications []media.Publication
	written      []string // public keys written
	dims         media.Dims
	analysis     *media.PageAnalysis
	err          error // permanent
}

// todo lists the uploads with stale, pending or forced image outputs.
func (p *Processor) todo(ctx context.Context, item media.Item, m *media.Manifest, job media.ProcessJob) ([]work, error) {
	k := item.Kind()
	analysisCapacity := k.PageAnalysisCapacity(m)
	var out []work
	for _, f := range m.Files {
		if !f.IsUpload() || f.Blob == "" || f.Gone || f.Fail() != nil {
			continue
		}
		w := work{src: f}
		for _, pr := range k.PrivateFor(f.Path) {
			if pr.Image == nil || job.Preset != "" && pr.Name != job.Preset {
				continue
			}
			outs := m.Outputs(f.Path, pr.Name)
			if job.Force || slices.Contains(f.Pending, pr.Name) || len(outs) != 1 || outs[0].Path != k.OutputPath(pr, f.Path) ||
				outs[0].FP != media.SpecFP(f, spec(pr, f), Recipe) {
				w.private = append(w.private, pr)
			}
		}
		if !f.Unattached {
			for _, pu := range k.PublicFor(f.Path) {
				if job.Preset != "" && pu.Name != job.Preset {
					continue
				}
				stale := job.Force || slices.Contains(f.Pending, pu.Name)
				if !stale {
					var err error
					if stale, err = p.publicStale(ctx, item, m, f, pu); err != nil {
						return nil, err
					}
				}
				if stale {
					w.public = append(w.public, pu)
				}
			}
		}
		w.measure = strings.HasPrefix(f.Type, "image/") && f.W == 0
		w.analyze = analysisCapacity > 0 && k.IsPageUpload(f) && strings.HasPrefix(f.Type, "image/") &&
			!f.HasPageAnalysis(media.PageAnalysisRecipe) && media.PageAnalysisFits(f.Meta)
		if w.analyze {
			analysisCapacity--
		}
		if len(w.private) > 0 || len(w.public) > 0 || w.measure || w.analyze {
			out = append(out, w)
		}
	}
	return out, nil
}

// spec is pr's image for upload f: Choose's, else the preset's.
func spec(pr *media.Private, f media.File) media.Image {
	if pr.Choose != nil {
		if im := pr.Choose(f); im != nil {
			return *im
		}
	}
	return *pr.Image
}

// publicStale reports a public preset whose names are missing or carry
// another fingerprint.
func (p *Processor) publicStale(ctx context.Context, item media.Item, m *media.Manifest, f media.File, pu *media.Public) (bool, error) {
	fp := publicFP(f, pu)
	if len(item.Kind().PublicNames(m, pu, f.Path)) == 0 {
		return false, nil
	}
	pub, ok := item.Kind().Publication(m, f, pu)
	if !ok || !pub.Ready() || pub.FP != fp {
		return true, nil
	}
	for i := range pub.Names {
		key, err := item.PublicationKey(pub, i)
		if err != nil {
			return false, err
		}
		obj, err := p.c.Store.Head(ctx, key)
		if errors.Is(err, media.ErrNotFound) {
			return true, nil
		} else if err != nil {
			return false, err
		}
		if obj.Metadata["fp"] != fp {
			return true, nil
		}
	}
	return false, nil
}

// pass renders todo and the stale zips and records them in one manifest
// edit. An upload whose blob or edit changed meanwhile keeps nothing this
// pass made for it; the next pass redoes it.
//
// Public writes use manifest-owned generations. Publication verifies ownership
// again; a delayed write from a retired generation cannot overwrite this one.
func (p *Processor) pass(ctx context.Context, item media.Item, m *media.Manifest, todo []work, zips []*media.Private) (err error) {
	var (
		mu      sync.Mutex
		results []done
		written []string // every public key this pass may have written
	)
	defer func() {
		if err != nil && len(written) > 0 {
			err = errors.Join(err, p.dropIfGone(ctx, item, written))
		}
	}()
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.c.Workers)
	for _, w := range todo {
		g.Go(func() error {
			d, err := p.render(gctx, item, m, w)
			mu.Lock()
			defer mu.Unlock()
			written = append(written, d.written...)
			if err != nil {
				return err
			}
			results = append(results, d)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	zipped := map[string]media.File{}
	for _, z := range zips {
		f, ok, err := p.buildZip(ctx, item, m, z)
		if err != nil {
			return err
		}
		if ok {
			zipped[z.Name] = f
		}
	}
	var orphaned []string
	k := item.Kind()
	_, err = p.c.Manifests.EditExisting(ctx, item.Ref(), func(cur *media.Manifest) error {
		orphaned = orphaned[:0]
		// A blob this edit newly references may have been swept or taken
		// down before it took the folder lock (one shared with an upload
		// removed meanwhile, too): there reports it still in place. What is
		// missing is not recorded, and the next pass makes it again; the
		// rest of the pass is kept.
		present := map[string]bool{}
		for _, b := range cur.Blobs() {
			present[b] = true
		}
		there := func(blob string) (bool, error) {
			if ok, seen := present[blob]; seen {
				return ok, nil
			}
			key, _ := item.Blob(blob)
			_, err := p.c.Store.Head(ctx, key)
			if err != nil && !errors.Is(err, media.ErrNotFound) {
				return false, fmt.Errorf("media/image: output %s: %w", key, err)
			}
			present[blob] = err == nil
			return err == nil, nil
		}
		var gone []string // outputs of uploads removed or replaced meanwhile, and of zips that bundled them
		analysisCapacity := k.PageAnalysisCapacity(cur)
		for _, d := range results {
			i := cur.Find(d.src.Path)
			if i < 0 || cur.Files[i].Blob != d.src.Blob || cur.Files[i].Edit.Hash() != d.src.Edit.Hash() {
				orphaned = append(orphaned, d.written...)
				for _, f := range d.outputs {
					gone = append(gone, f.Blob)
				}
				continue
			}
			if d.analysis != nil && analysisCapacity > 0 {
				meta, err := media.WithPageAnalysis(cur.Files[i].Meta, *d.analysis)
				if err == nil {
					cur.Files[i].Meta = meta
					analysisCapacity--
				} else {
					// A concurrent metadata edit may have used the reserved space.
					// Never discard an administrator's choices to fit evidence.
					slog.WarnContext(ctx, "page analysis metadata unavailable", "ref", item.Ref(), "path", d.src.Path, "error", err)
				}
			}
			if d.err != nil && cur.Full {
				// A failure record does not fit a Full manifest: drop the
				// public names instead, so the pass is not repeated.
				for _, pu := range d.public {
					cur.ClearPending(d.src.Path, pu.Name)
				}
				continue
			}
			if d.err != nil {
				cur.SetFailed(d.src.Path, d.err)
				continue
			}
			missing := false
			for _, pr := range d.private {
				ok, err := there(d.outputs[pr.Name].Blob)
				if err != nil {
					return err
				}
				missing = missing || !ok
			}
			if missing {
				continue // still pending: the next pass renders it again
			}
			if d.dims.W > 0 && !cur.Full { // a Full manifest only shrinks
				cur.Files[i].W, cur.Files[i].H = d.dims.W, d.dims.H
			}
			for _, pr := range d.private {
				if err := cur.SetOutputs(d.src.Path, pr.Name, []media.File{d.outputs[pr.Name]}); err != nil {
					return err
				}
			}
			for _, pub := range d.publications {
				f, _ := cur.Get(d.src.Path)
				owned, ok := f.Publication(pub.Preset)
				if !ok || owned.Generation != pub.Generation {
					for i := range pub.Names {
						key, err := item.PublicationKey(pub, i)
						if err != nil {
							return err
						}
						orphaned = append(orphaned, key)
					}
					continue
				}
				present := true
				for i := range pub.Names {
					key, _ := item.PublicationKey(pub, i)
					if _, err := p.c.Store.Head(ctx, key); errors.Is(err, media.ErrNotFound) {
						present = false
					} else if err != nil {
						return err
					}
				}
				if present {
					pub.State = media.PublicationReady
					cur.SetPublication(d.src.Path, pub)
					cur.ClearPending(d.src.Path, pub.Preset)
				}
			}
		}
		for _, z := range zips {
			if f, ok := zipped[z.Name]; ok && f.FP == media.ZipFP(k.ZipInputs(cur, z)) {
				if ok, err := there(f.Blob); err != nil {
					return err
				} else if ok {
					if err := cur.SetOutputs(z.Zip, z.Name, []media.File{f}); err != nil {
						return err
					}
				}
			} else {
				if ok {
					gone = append(gone, f.Blob)
				}
				if len(k.ZipInputs(cur, z)) == 0 {
					if err := cur.SetOutputs(z.Zip, z.Name, nil); err != nil {
						return err
					}
				}
			}
		}
		// What was rendered for a source that is gone does not outlive it.
		return p.c.Manifests.DeleteUnreferenced(ctx, item, cur, gone)
	})
	switch {
	case errors.Is(err, media.ErrNotFound):
		err, orphaned = nil, written
	case errors.Is(err, media.ErrManifestTooLarge):
		// The outputs do not fit: mark the item Full, which stops processing
		// (the rendered blobs go by age).
		if err := p.c.Manifests.SetFull(ctx, item.Ref(), err); err != nil {
			return errors.Join(err, p.dropIfGone(ctx, item, written))
		}
		return p.dropIfGone(ctx, item, written)
	case err != nil:
		return err
	}
	for _, d := range results {
		if d.err != nil {
			p.failed(ctx, item, d.src.Path, d.err)
		}
	}
	if len(orphaned) > 0 {
		if err := p.c.Manifests.DropUnreferenced(ctx, item.Ref(), media.Unreferenced{Public: orphaned}); err != nil {
			return err
		}
	}
	return nil
}

// dropIfGone removes failed-pass outputs only after the item was deleted.
func (p *Processor) dropIfGone(ctx context.Context, item media.Item, keys []string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	_, err := p.c.Manifests.EditExisting(ctx, item.Ref(), func(*media.Manifest) error { return nil })
	if errors.Is(err, media.ErrNotFound) {
		return p.drop(ctx, keys)
	}
	return err
}

// drop deletes public keys.
func (p *Processor) drop(ctx context.Context, keys []string) error {
	for _, key := range keys {
		if err := p.c.Store.Delete(ctx, key); err != nil && !errors.Is(err, media.ErrNotFound) {
			return err
		}
	}
	return nil
}

// render decodes one upload once and encodes its private and public outputs.
func (p *Processor) render(ctx context.Context, item media.Item, m *media.Manifest, w work) (done, error) {
	d := done{work: w, outputs: map[string]media.File{}}
	if !strings.HasPrefix(w.src.Type, "image/") {
		d.err = &media.ImageError{Code: media.CodeImageUnreadable, Message: w.src.Type + " is not an image",
			Details: media.ErrorDetails{Type: w.src.Type}}
		return d, nil
	}
	key, err := item.Blob(w.src.Blob)
	if err != nil {
		return d, err
	}
	src, err := p.read(ctx, key, w.src, item.Kind())
	if err != nil {
		if isPermanent(err) {
			if w.analyze {
				digest, _ := layout.BlobDigest(w.src.Blob)
				d.analysis = &media.PageAnalysis{Recipe: media.PageAnalysisRecipe, SourceSHA256: hex.EncodeToString(digest),
					Status: "failed", Blank: "unknown", Confidence: "unknown", Flags: []string{"source_unavailable"}}
			}
			d.err = err
			return d, nil
		}
		return d, err
	}
	if w.analyze {
		a, err := p.AnalyzePage(ctx, src, w.src.Type)
		if err != nil {
			return d, err
		}
		d.analysis = &a
	}
	k := item.Kind()
	for _, pu := range w.public {
		names := k.PublicNames(m, pu, w.src.Path)
		if len(names) == 0 {
			continue // past a preview's First by now: nothing to render
		}
		outs, dims, err := encodePublic(src, w.src.Type, pu, names, w.src.Edit, p.rules(pu.Image.Animation))
		if d.dims = dims; err != nil {
			return p.permanent(d, err)
		}
		var prepared []string
		if k.Versions {
			sums := make([][]byte, len(names))
			for i, name := range names {
				sums[i] = sha(outs[name].webp)
			}
			prepared, err = p.c.Manifests.NewBlobs(ctx, item.Ref(), layout.AreaInternal, sums)
			if err != nil {
				return d, err
			}
		}
		var publication media.Publication
		_, err = p.c.Manifests.EditExisting(ctx, item.Ref(), func(cur *media.Manifest) error {
			publication = media.Publication{}
			f, ok := cur.Get(w.src.Path)
			if !ok || f.Unattached {
				return nil
			}
			if f.Blob != w.src.Blob || f.Edit.Hash() != w.src.Edit.Hash() || !slices.Equal(k.PublicNames(cur, pu, w.src.Path), names) {
				return nil // changed, or moved to another preview position
			}
			fp := publicFP(w.src, pu)
			if old, ok := k.Publication(cur, f, pu); ok && !old.Ready() && old.FP == fp {
				publication = old // interrupted PUTs retry the active reservation
				return nil
			}
			stable := pu.Stable
			publication = media.Publication{Preset: pu.Name, Source: f.Key(), FP: fp, Stable: &stable,
				Generation: uuid.NewString(), Names: names, Blobs: prepared, Dims: make([]media.Dims, len(names))}
			for i, name := range names {
				publication.Dims[i] = outs[name].dims
			}
			cur.SetPublication(f.Path, publication)
			if !cur.Full {
				cur.AddPending(f.Path, pu.Name)
			}
			return nil
		})
		if err != nil && !errors.Is(err, media.ErrNotFound) {
			return d, err
		}
		if publication.Generation == "" {
			continue
		}
		for i := range publication.Names {
			out := outs[names[i]]
			pk, err := item.PublicationKey(publication, i)
			if err != nil {
				return d, err
			}
			d.written = append(d.written, pk) // an interrupted PUT may still land
			_, err = p.c.Store.Put(ctx, pk, bytes.NewReader(out.webp), int64(len(out.webp)), media.PutOptions{
				ContentType: "image/webp", ChecksumSHA256: sha(out.webp), IfNoneMatch: "*",
				Metadata: map[string]string{"from": url.PathEscape(w.src.Path), "fp": publication.FP}})
			if errors.Is(err, media.ErrPreconditionFailed) {
				// Only this active reservation can write here. Verify its
				// bytes before accepting an earlier attempt as completed.
				obj, headErr := p.c.Store.Head(ctx, pk)
				if headErr != nil {
					return d, headErr
				}
				if obj.Size != int64(len(out.webp)) || obj.Metadata["fp"] != publication.FP ||
					len(obj.ChecksumSHA256) > 0 && !bytes.Equal(obj.ChecksumSHA256, sha(out.webp)) {
					return d, fmt.Errorf("media/image: reserved public file %s differs from its output", pk)
				}
				if len(obj.ChecksumSHA256) == 0 {
					rc, _, err := p.c.Store.Get(ctx, pk, media.GetOptions{})
					if err != nil {
						return d, err
					}
					h := sha256.New()
					n, readErr := io.Copy(h, io.LimitReader(rc, int64(len(out.webp))+1))
					rc.Close()
					if readErr != nil {
						return d, readErr
					}
					if n != int64(len(out.webp)) || !bytes.Equal(h.Sum(nil), sha(out.webp)) {
						return d, fmt.Errorf("media/image: reserved public file %s differs from its output", pk)
					}
				}
			} else if err != nil {
				return d, err
			}
		}
		d.publications = append(d.publications, publication)
	}
	for _, pr := range w.private {
		s := spec(pr, w.src)
		if _, err := probe(src, w.src.Type, p.rules(s.Animation)); err != nil {
			return p.permanent(d, err)
		}
		out, dims, err := encode(src, w.src.Type, s, w.src.Edit)
		if err != nil {
			return p.permanent(d, err)
		}
		typ := outputType(s, w.src.Type)
		blob := w.src.Blob
		area := k.Area("", pr.Name)
		sourceArea, _, _ := layout.SplitBlobRef(blob)
		if s.Format != media.ImageFormatSource || !bytes.Equal(out, src) || sourceArea != area {
			blob, err = p.putBlob(ctx, item, area, bytes.NewReader(out), int64(len(out)), sha(out), typ)
			if err != nil {
				return d, err
			}
		}
		d.outputs[pr.Name] = media.File{Path: k.OutputPath(pr, w.src.Path), Blob: blob, Type: typ, Size: int64(len(out)),
			W: dims.W, H: dims.H, FP: media.SpecFP(w.src, s, Recipe)}
	}
	if d.dims.W == 0 {
		s, err := probe(src, w.src.Type, p.rules(media.AnimationAllow))
		if err != nil {
			return p.permanent(d, err)
		}
		d.dims = s.dims()
	}
	return d, nil
}

func (p *Processor) permanent(d done, err error) (done, error) {
	if isPermanent(err) {
		d.err = err
		return d, nil
	}
	return d, err
}

// syncPublic deletes the public names no attached upload's
// preset expects: a removed upload's, or all of a hidden item's.
func (p *Processor) syncPublic(ctx context.Context, item media.Item) error {
	_, err := p.c.Manifests.SyncPublic(ctx, item.Ref())
	return err
}

// editorViews renders the missing editor views of the item's image uploads
// (Config.Editor over the whole oriented source), recorded on that source.
func (p *Processor) editorViews(ctx context.Context, item media.Item, m *media.Manifest) error {
	want := map[string]media.File{}
	for _, f := range m.Files {
		if p.reg.EditorFingerprint(f) != "" && f.Fail() == nil {
			want[f.Path] = f
		}
	}
	if len(want) == 0 {
		return nil
	}
	for name, f := range want {
		if blob := p.reg.EditorView(f); blob != "" {
			key, err := item.Blob(blob)
			if err != nil {
				return err
			}
			if _, err := p.c.Store.Head(ctx, key); err == nil {
				delete(want, name)
			} else if !errors.Is(err, media.ErrNotFound) {
				return err
			}
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.c.Workers)
	for _, f := range want {
		g.Go(func() error {
			key, _ := item.Blob(f.Blob)
			src, err := p.read(gctx, key, f, item.Kind())
			if err != nil {
				return nil // missing or unreadable: its upload records why
			}
			if _, err := probe(src, f.Type, p.rules(media.AnimationAllow)); err != nil {
				return nil
			}
			out, _, err := encode(src, f.Type, p.reg.Config().Editor, nil)
			if err != nil {
				return nil
			}
			blob, err := p.putBlob(gctx, item, layout.AreaPrivate, bytes.NewReader(out), int64(len(out)), sha(out), "image/webp")
			if err != nil {
				return err
			}
			_, err = p.c.Manifests.EditExisting(gctx, item.Ref(), func(cur *media.Manifest) error {
				i := cur.Find(f.Path)
				if i < 0 || cur.Files[i].Blob != f.Blob || cur.Files[i].Gone {
					return p.c.Manifests.DeleteUnreferenced(gctx, item, cur, []string{blob})
				}
				key, _ := item.Blob(blob)
				if _, err := p.c.Store.Head(gctx, key); err != nil {
					return err
				}
				cur.Files[i].Editor = &media.EditorImage{Blob: blob, FP: p.reg.EditorFingerprint(f)}
				return nil
			})
			if errors.Is(err, media.ErrNotFound) {
				return p.c.Manifests.DropIfDeleted(gctx, item.Ref(), []string{blob})
			}
			return err
		})
	}
	return g.Wait()
}

func (p *Processor) rules(animation media.Animation) rules {
	return rules{maxPixels: p.c.MaxPixels, maxFrames: p.c.MaxFrames, maxSeconds: p.c.MaxAnimationSeconds, animation: animation}
}

// putBlob records ownership and writes one immutable allocation in area.
func (p *Processor) putBlob(ctx context.Context, item media.Item, area string, body io.Reader, size int64, sum []byte, contentType string) (string, error) {
	names, err := p.c.Manifests.NewBlobs(ctx, item.Ref(), area, [][]byte{sum})
	if err != nil {
		return "", err
	}
	name := names[0]
	key, err := item.Blob(name)
	if err != nil {
		return "", err
	}
	opts := media.PutOptions{ContentType: contentType, ChecksumSHA256: sum}
	caps, err := p.c.Store.Capabilities(ctx)
	if err != nil {
		return "", err
	}
	if caps.ConditionalPut {
		opts.IfNoneMatch = "*"
	}
	if _, err := p.c.Store.Put(ctx, key, body, size, opts); err != nil && !errors.Is(err, media.ErrPreconditionFailed) {
		return "", err
	}
	return name, nil
}

// read reads upload f's blob: one stored larger than f may be (its Upload's
// MaxBytes, or a grabbed frame's recorded size) is refused unread. Blobs are
// hashed when placed or produced, so the bytes are not hashed again.
func (p *Processor) read(ctx context.Context, key string, f media.File, k *media.Kind) ([]byte, error) {
	u, _ := k.UploadOf(f.Path)
	limit := u.MaxBytes
	if f.Frame != nil && f.Frame.Of != "" {
		limit = max(limit, f.Size)
	}
	rc, obj, err := p.c.Store.Get(ctx, key, media.GetOptions{})
	if errors.Is(err, media.ErrNotFound) {
		return nil, permanentError{err}
	} else if err != nil {
		return nil, err
	}
	defer rc.Close()
	tooLarge := func(size int64) error {
		return permanentError{&media.ImageError{Code: media.CodeTooLarge,
			Message: fmt.Sprintf("%s files at %s may be at most %d bytes; this one is %d", f.Type, u.Path, limit, size),
			Details: media.ErrorDetails{Type: f.Type, Size: size, MaxBytes: limit}}}
	}
	if obj.Size > limit {
		return nil, tooLarge(obj.Size)
	}
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, tooLarge(int64(len(b)))
	}
	return b, nil
}

func (p *Processor) failed(ctx context.Context, item media.Item, path string, err error) {
	if h := p.reg.Config().Hooks.Failed; h != nil {
		h(ctx, item.Ref(), path, err)
	}
}

func sha(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}
