// Package image is the media worker's image producer (libvips, CGO): the
// Image presets' WebP files, the public presets' images, editor views, and
// the kinds' default public images. A Processor runs one media.ProcessJob:
// it redoes stale or pending outputs, writes each under the hash of its
// bytes, and records them in one manifest edit per source (or per group of
// sources). An output whose fingerprint matches is never redone, so a
// re-derive writes nothing.
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
	"slices"
	"strings"
	"time"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/open-rails/helpers/deps"
	"golang.org/x/sync/errgroup"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// Recipe versions the image producer: a change re-renders every image output.
const Recipe = "webp-1"

// SourceRecipe versions source-format outputs, which copy an unchanged
// upload without its metadata.
const SourceRecipe = "source-2"

// privateFP is the fingerprint of an Image preset's output of src at s.
func privateFP(src media.File, s media.Image) string {
	if s.Format == media.ImageFormatSource {
		return mediastore.SpecFP(src, s, SourceRecipe)
	}
	return mediastore.SpecFP(src, s, Recipe)
}

// Config configures a Processor.
type Config struct {
	Store     media.Store
	Manifests *mediastore.Manifests
	Workers   int // uploads decoded at once; default 2
	// MaxPixels bounds a decoded source, all frames of an animation
	// together (default 100 MP); MaxFrames and MaxAnimationSeconds bound
	// animations (defaults 1000 and 60).
	MaxPixels           int
	MaxFrames           int
	MaxAnimationSeconds float64
	TempDir             string // zip scratch; default os.TempDir()
	// Each rendered source is recorded and reported on its own (GroupSize 1,
	// the default). Above 1, one manifest edit records GroupSize of them, or
	// those finished within GroupWait (default 5s) of the group's first,
	// including on retried jobs.
	GroupSize int
	GroupWait time.Duration
}

// Processor runs image jobs. It is safe for concurrent use and idempotent:
// an output whose fingerprint matches is never redone (unless forced).
type Processor struct {
	c   Config
	reg *mediastore.Registry
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
	if c.GroupSize <= 0 {
		c.GroupSize = 1
	}
	if c.GroupWait <= 0 {
		c.GroupWait = 5 * time.Second
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
	return mediastore.SpecFP(src, publicSpec{p.To, p.Widths, p.Image}, Recipe)
}

// Process brings ref's image outputs up to date. Passes repeat until the
// manifest needs no more work, so a commit landing during a pass is absorbed.
func (p *Processor) Process(ctx context.Context, job mediastore.ProcessJob) error {
	item, err := p.reg.Item(job.Ref)
	if err != nil {
		return err
	}
	for range 8 {
		m, _, err := p.c.Manifests.Get(ctx, job.Ref)
		if errors.Is(err, mediastore.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		todo, err := p.todo(ctx, item, m, job)
		if err != nil {
			return err
		}
		if len(todo) == 0 {
			if job.Editor {
				return p.editorViews(ctx, item, m)
			}
			return nil
		}
		if p.c.GroupSize > 1 {
			err = p.renderInGroups(ctx, item, m, todo, job.Retry)
		} else {
			err = p.renderEach(ctx, item, m, todo)
		}
		if err != nil {
			return err
		}
		job.Force = false // forced once
	}
	return fmt.Errorf("media/image: manifest of %s kept changing", job.Ref)
}

// FailPending records an exhausted job's remaining sources, preserving completed work.
func (p *Processor) FailPending(ctx context.Context, job mediastore.ProcessJob, cause error) error {
	item, err := p.reg.Item(job.Ref)
	if err != nil {
		return err
	}
	job.Force = false
	_, err = p.c.Manifests.EditExisting(ctx, job.Ref, func(cur *mediastore.Manifest) error {
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
	if errors.Is(err, mediastore.ErrNotFound) {
		return nil
	}
	return err
}

// work is one upload's image outputs to render.
type work struct {
	src     media.File
	in      media.File // src's master, which its outputs derive from
	private []*media.Private
	public  []*media.Public
	measure bool // record its size (W×H) only
	analyze bool // record original-page evidence, independently of renditions
}

// done is a rendered upload.
type done struct {
	work
	outputs  map[string]media.File   // by private preset
	public   map[string][]media.File // by public preset, one per width
	dims     media.Dims
	analysis *media.PageAnalysis
	err      error // permanent
}

// todo lists the uploads with stale, pending or forced image outputs.
func (p *Processor) todo(ctx context.Context, item mediastore.Item, m *mediastore.Manifest, job mediastore.ProcessJob) ([]work, error) {
	k := item.Kind()
	analysisCapacity := k.PageAnalysisCapacity(m)
	var out []work
	for _, f := range m.Files {
		if !f.IsUpload() || f.Blob == "" || f.Gone || f.Fail() != nil || !k.MasterCurrent(f) {
			continue // a master its hooks have yet to make keeps its renditions
		}
		w := work{src: f, in: mediastore.Input(f)}
		for _, pr := range k.PrivateFor(f.Path) {
			if pr.Image == nil || job.Preset != "" && pr.Name != job.Preset {
				continue
			}
			outs := m.Outputs(f.Path, pr.Name)
			if job.Force || slices.Contains(f.Pending, pr.Name) || len(outs) != 1 || outs[0].Path != k.OutputPath(pr, f.Path) ||
				outs[0].FP != privateFP(w.in, pr.ImageFor(media.Dims{W: w.in.W, H: w.in.H})) {
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
					if stale, err = p.publicStale(ctx, item, f, pu); err != nil {
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
			!f.HasPageAnalysis(media.PageAnalysisRecipe) && mediastore.PageAnalysisFits(f.Meta)
		if w.analyze {
			analysisCapacity--
		}
		if len(w.private) > 0 || len(w.public) > 0 || w.measure || w.analyze {
			out = append(out, w)
		}
	}
	return out, nil
}

// publicStale reports a public preset whose outputs are missing, named
// otherwise or of another fingerprint.
func (p *Processor) publicStale(ctx context.Context, item mediastore.Item, f media.File, pu *media.Public) (bool, error) {
	fp := publicFP(mediastore.Input(f), pu)
	names := item.Kind().PublicNames(pu, f.Path)
	m, _, err := p.c.Manifests.Get(ctx, item.Ref())
	if err != nil {
		return false, err
	}
	outs := m.Outputs(f.Path, pu.Name)
	if len(outs) != len(names) {
		return true, nil
	}
	for i, o := range outs {
		if o.Path != names[i] || o.FP != fp {
			return true, nil
		}
	}
	return false, nil
}

// renderEach renders todo, Workers sources at a time, and records and reports
// each as it finishes. A rendered source is recorded even when another failed
// or the job ended meanwhile, so an exhausted job's FailPending finds only
// sources that were not rendered or could not be recorded.
func (p *Processor) renderEach(ctx context.Context, item mediastore.Item, m *mediastore.Manifest, todo []work) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.c.Workers)
	for _, w := range todo {
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}
			d, err := p.render(gctx, item, m, w)
			if err != nil {
				return err // written outputs nobody names are GC's orphans
			}
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			return p.record(ctx, item, []done{d})
		})
	}
	return g.Wait()
}

// renderInGroups renders todo, Workers sources at a time, and records the
// finished sources in groups: GroupSize of them, or those finished within
// GroupWait of the group's first, and the rest at the end. Each recorded
// group is reported, so progress still shows while a large gallery costs a
// few dozen manifest edits rather than one per page. A source that failed to
// render, or finished after a group failed, is left for the job's retry.
// On a retry, completed sources are recorded even after cancellation or another
// group's failure, just as renderEach preserves them for FailPending.
func (p *Processor) renderInGroups(ctx context.Context, item mediastore.Item, m *mediastore.Manifest, todo []work, retry bool) error {
	type rendered struct {
		done
		err error
	}
	renderCtx, stop := context.WithCancel(ctx)
	defer stop()
	g, gctx := errgroup.WithContext(renderCtx)
	g.SetLimit(p.c.Workers)
	finished := make(chan rendered, len(todo))
	var renderErr error
	go func() {
		for _, w := range todo {
			g.Go(func() error {
				if err := gctx.Err(); err != nil {
					return err
				}
				d, err := p.render(gctx, item, m, w)
				finished <- rendered{d, err}
				return err
			})
		}
		renderErr = g.Wait()
		close(finished)
	}()
	var (
		group []done
		due   <-chan time.Time
		err   error
	)
	flush := func() {
		if len(group) > 0 {
			recordCtx := ctx
			cancel := func() {}
			if retry {
				recordCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			}
			groupErr := p.record(recordCtx, item, group)
			cancel()
			err = errors.Join(err, groupErr)
		}
		group, due = nil, nil
	}
	for open := true; open; {
		select {
		case r, ok := <-finished:
			switch {
			case !ok:
				open = false
				flush()
			case r.err != nil || err != nil && !retry:
			default:
				group = append(group, r.done)
				if len(group) == 1 {
					due = time.After(p.c.GroupWait)
				}
				if len(group) == p.c.GroupSize {
					flush()
				}
			}
		case <-due:
			flush()
		}
		if err != nil {
			stop()
		}
	}
	if err == nil {
		err = renderErr
	}
	return err
}

// record saves rendered uploads in one manifest edit. An upload whose blob
// or edit changed meanwhile keeps nothing made for it; the next pass redoes
// it, and what was written for it is an orphan GC removes.
func (p *Processor) record(ctx context.Context, item mediastore.Item, results []done) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k := item.Kind()
	_, err := p.c.Manifests.EditExisting(ctx, item.Ref(), func(cur *mediastore.Manifest) error {
		analysisCapacity := k.PageAnalysisCapacity(cur)
		for _, d := range results {
			i := cur.Find(d.src.Path)
			if i < 0 || cur.Files[i].Blob != d.src.Blob || cur.Files[i].Edit.Hash() != d.src.Edit.Hash() ||
				mediastore.InputHash(cur.Files[i]) != mediastore.InputHash(d.src) {
				continue
			}
			if d.analysis != nil && analysisCapacity > 0 {
				meta, err := mediastore.WithPageAnalysis(cur.Files[i].Meta, *d.analysis)
				if err == nil {
					cur.Files[i].Meta = meta
					analysisCapacity--
				} else {
					// A concurrent metadata edit may have used the reserved space.
					// Never discard an administrator's choices to fit evidence.
					slog.WarnContext(ctx, "page analysis metadata unavailable", "ref", item.Ref(), "path", d.src.Path, "error", err)
				}
			}
			if d.err != nil {
				cur.SetFailed(d.src.Path, d.err)
				continue
			}
			if d.dims.W > 0 {
				cur.Files[i].W, cur.Files[i].H = d.dims.W, d.dims.H
			}
			for _, pr := range d.private {
				if err := cur.SetOutputs(d.src.Path, pr.Name, []media.File{d.outputs[pr.Name]}); err != nil {
					return err
				}
			}
			for _, pu := range d.work.public {
				if f, ok := cur.Get(d.src.Path); !ok || f.Unattached {
					continue
				}
				if err := cur.SetOutputs(d.src.Path, pu.Name, d.public[pu.Name]); err != nil {
					return err
				}
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, mediastore.ErrNotFound):
		return nil // deleted meanwhile: GC takes the outputs
	case err != nil && len(results) > 1 && ctx.Err() == nil && !outage(err):
		// One source that does not fit, or cannot be recorded, must not drop
		// the rest: each half is tried again, down to the source alone, which
		// is kept or refused as it would be by itself.
		half := len(results) / 2
		return errors.Join(p.record(ctx, item, results[:half]), p.record(ctx, item, results[half:]))
	case errors.Is(err, mediastore.ErrManifestTooLarge):
		// The outputs do not fit: the source fails instead, which shrinks it.
		_, failErr := p.c.Manifests.EditExisting(ctx, item.Ref(), func(cur *mediastore.Manifest) error {
			for _, d := range results {
				if f, ok := cur.Get(d.src.Path); ok && f.Blob == d.src.Blob {
					cur.SetFailed(d.src.Path, err)
				}
			}
			return nil
		})
		return failErr
	case err != nil:
		return err
	}
	for _, d := range results {
		if d.err != nil {
			p.failed(ctx, item, d.src.Path, d.err)
		}
	}
	return nil
}

// outage reports a store or database that refuses every edit alike.
func outage(err error) bool {
	return errors.Is(err, media.ErrUnavailable) || deps.PostgresUnavailable(err)
}

// render decodes one upload once and encodes its private and public outputs.
func (p *Processor) render(ctx context.Context, item mediastore.Item, m *mediastore.Manifest, w work) (done, error) {
	d := done{work: w, outputs: map[string]media.File{}}
	in := w.in
	if !strings.HasPrefix(in.Type, "image/") {
		d.err = &media.ImageError{Code: media.CodeImageUnreadable, Message: in.Type + " is not an image",
			Details: media.ErrorDetails{Type: in.Type}}
		return d, nil
	}
	key, err := item.Blob(in.Blob)
	if err != nil {
		return d, err
	}
	src, err := p.read(ctx, key, in, item.Kind())
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
		a, err := p.AnalyzePage(ctx, src, in.Type)
		if err != nil {
			return d, err
		}
		d.analysis = &a
	}
	k := item.Kind()
	u, _ := k.UploadOf(w.src.Path) // read refused an undeclared path
	d.public = map[string][]media.File{}
	for _, pu := range w.public {
		names := k.PublicNames(pu, w.src.Path)
		outs, dims, err := encodePublic(src, in.Type, pu, names, in.Edit, p.rules(u, pu.Image.Animation))
		if d.dims = dims; err != nil {
			return p.permanent(d, err)
		}
		fp := publicFP(in, pu)
		for _, name := range names {
			out := outs[name]
			blob, err := p.putBlob(ctx, item, k.Area(pu.Name), bytes.NewReader(out.webp), int64(len(out.webp)), sha(out.webp), "image/webp")
			if err != nil {
				return d, err
			}
			d.public[pu.Name] = append(d.public[pu.Name], media.File{Path: name, Blob: blob, Type: "image/webp",
				Size: int64(len(out.webp)), W: out.dims.W, H: out.dims.H, FP: fp, Input: mediastore.InputHash(in)})
		}
	}
	for _, pr := range w.private {
		// The probed size picks the shape, so an unmeasured upload renders
		// its own on the first pass; shapes keep the preset's animation.
		r := p.rules(u, pr.Image.Animation)
		info, err := probe(src, in.Type, r)
		if err != nil {
			return p.permanent(d, err)
		}
		s := pr.ImageFor(info.dims())
		out, dims, err := encode(src, in.Type, info, s, in.Edit, r)
		if err != nil {
			return p.permanent(d, err)
		}
		typ := outputType(s, in.Type)
		blob := in.Blob
		area := k.Area(pr.Name)
		if outs := m.Outputs(w.src.Path, pr.Name); len(outs) == 1 && layout.Served(layout.BlobArea(outs[0].Blob)) {
			area = layout.BlobArea(outs[0].Blob) // a released page's new rendition is served at once
		}
		sourceArea, _, _ := layout.SplitBlobRef(blob)
		if s.Format != media.ImageFormatSource || !bytes.Equal(out, src) || sourceArea != area {
			blob, err = p.putBlob(ctx, item, area, bytes.NewReader(out), int64(len(out)), sha(out), typ)
			if err != nil {
				return d, err
			}
		}
		d.outputs[pr.Name] = media.File{Path: k.OutputPath(pr, w.src.Path), Blob: blob, Type: typ, Size: int64(len(out)),
			W: dims.W, H: dims.H, FP: privateFP(in, s), Input: mediastore.InputHash(in)}
	}
	if in.Blob != w.src.Blob {
		// A master's size is not its upload's: measure the original.
		if d.dims = (media.Dims{}); w.measure {
			key, err := item.Blob(w.src.Blob)
			if err != nil {
				return d, err
			}
			orig, err := p.read(ctx, key, w.src, k)
			if err != nil {
				return p.permanent(d, err)
			}
			s, err := probe(orig, w.src.Type, p.rules(u, media.AnimationAllow))
			if err != nil {
				return p.permanent(d, err)
			}
			d.dims = s.dims()
		}
	} else if d.dims.W == 0 {
		s, err := probe(src, in.Type, p.rules(u, media.AnimationAllow))
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

// editorViews renders the missing editor views of the item's image uploads
// (Config.Editor over the whole oriented source), recorded on that source.
func (p *Processor) editorViews(ctx context.Context, item mediastore.Item, m *mediastore.Manifest) error {
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
			} else if !errors.Is(err, mediastore.ErrNotFound) {
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
			u, _ := item.Kind().UploadOf(f.Path)
			r := p.rules(u, media.AnimationAllow)
			info, err := probe(src, f.Type, r)
			if err != nil {
				return nil
			}
			out, _, err := encode(src, f.Type, info, p.reg.Config().Editor, nil, r)
			if err != nil {
				return nil
			}
			// Editor views are never served: editors read them by an exact
			// short-lived storage URL.
			blob, err := p.putBlob(gctx, item, layout.AreaInternal, bytes.NewReader(out), int64(len(out)), sha(out), "image/webp")
			if err != nil {
				return err
			}
			_, err = p.c.Manifests.EditExisting(gctx, item.Ref(), func(cur *mediastore.Manifest) error {
				i := cur.Find(f.Path)
				if i < 0 || cur.Files[i].Blob != f.Blob || cur.Files[i].Gone {
					return mediastore.ErrUnchanged
				}
				cur.Files[i].Editor = &media.EditorImage{Blob: blob, FP: p.reg.EditorFingerprint(f)}
				return nil
			})
			if errors.Is(err, mediastore.ErrNotFound) {
				return nil
			}
			return err
		})
	}
	return g.Wait()
}

// rules bound a source of upload u: its MaxPixels lowers the processor's.
func (p *Processor) rules(u media.Upload, animation media.Animation) rules {
	pixels := p.c.MaxPixels
	if u.MaxPixels > 0 {
		pixels = min(pixels, u.MaxPixels)
	}
	return rules{maxPixels: pixels, maxFrames: p.c.MaxFrames, maxSeconds: p.c.MaxAnimationSeconds, animation: animation}
}

// putBlob writes bytes under their hash in area. Identical bytes have the
// same name, so writing them again is harmless.
func (p *Processor) putBlob(ctx context.Context, item mediastore.Item, area string, body io.Reader, size int64, sum []byte, contentType string) (string, error) {
	blob := layout.BlobRef(area, sum)
	key, err := item.Blob(blob)
	if err != nil {
		return "", err
	}
	if _, err := p.c.Store.Put(ctx, key, body, size, media.PutOptions{ContentType: contentType, ChecksumSHA256: sum}); err != nil {
		return "", err
	}
	return blob, nil
}

// read reads upload f's blob: one stored larger than f may be (its Upload's
// MaxBytes, or a grabbed frame's recorded size) is refused unread. Blobs are
// hashed when placed or produced, so the bytes are not hashed again.
func (p *Processor) read(ctx context.Context, key string, f media.File, k *mediastore.RegisteredKind) ([]byte, error) {
	u, ok := k.UploadOf(f.Path)
	if !ok {
		return nil, permanentError{&media.ImageError{Code: media.CodeNotFound,
			Message: fmt.Sprintf("%s is no longer a declared upload path of kind %q", f.Path, k.Name)}}
	}
	limit := u.MaxBytes
	if f.Frame != nil && f.Frame.Of != "" || f.Master != nil && !f.Master.IsOriginal() {
		limit = max(limit, f.Size) // a grabbed frame or a master: their recorded size
	}
	rc, obj, err := p.c.Store.Get(ctx, key, media.GetOptions{})
	if errors.Is(err, mediastore.ErrNotFound) {
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

func (p *Processor) failed(ctx context.Context, item mediastore.Item, path string, err error) {
	if h := p.reg.Hooks().Failed; h != nil {
		h(ctx, item.Ref(), path, err)
	}
}

func sha(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}
