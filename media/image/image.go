// Package image is the media worker's image producer (libvips, CGO): the
// Image presets' private WebP files, the Zip presets, the public presets'
// fixed names, editor views, and the kinds' default public images. A
// Processor runs one media.ProcessJob: it redoes stale or pending outputs,
// records them in one manifest edit per pass, and keeps public/ in step.
package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

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
	// Purge relays overwritten or deleted public keys to the host's
	// Hooks.PurgePublic (media.HostQueue.Purge); optional.
	Purge   func(ctx context.Context, keys []string) error
	Workers int // uploads decoded at once; default 2
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
		zips := p.staleZips(item, m, job)
		if m.Full {
			// No record of a private output or zip fits. Public files still
			// render: recording them only clears pending names.
			zips = nil
			todo = slices.DeleteFunc(todo, func(w work) bool { return len(w.public) == 0 })
			for i := range todo {
				todo[i].private, todo[i].measure = nil, false
			}
		}
		if len(todo) == 0 && len(zips) == 0 {
			if job.Editor {
				return p.editorViews(ctx, item, m)
			}
			return nil
		}
		if err := p.pass(ctx, item, m, todo, zips); err != nil {
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

// work is one upload's image outputs to render.
type work struct {
	src     media.File
	private []*media.Private
	public  []*media.Public
	measure bool // record its size (W×H) only
}

// done is a rendered upload.
type done struct {
	work
	outputs map[string]media.File // by private preset
	written []string              // public keys written
	dims    media.Dims
	err     error // permanent
}

// todo lists the uploads with stale, pending or forced image outputs.
func (p *Processor) todo(ctx context.Context, item media.Item, m *media.Manifest, job media.ProcessJob) ([]work, error) {
	k := item.Kind()
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
		if !m.Hidden && !f.Unattached {
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
		if len(w.private) > 0 || len(w.public) > 0 || w.measure {
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
func (p *Processor) publicStale(ctx context.Context, item media.Item, f media.File, pu *media.Public) (bool, error) {
	fp := publicFP(f, pu)
	for _, n := range item.Kind().PublicNames(pu, f.Path) {
		key, err := item.Public(n)
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
// Public writes check their source under the manifest lock; cleanup uses
// the same lock, so removed or hidden sources cannot publish afterward.
func (p *Processor) pass(ctx context.Context, item media.Item, m *media.Manifest, todo []work, zips []*media.Private) (err error) {
	var (
		mu      sync.Mutex
		results []done
		written []string // every public key this pass may have written
	)
	defer func() {
		if err != nil && len(written) > 0 {
			err = errors.Join(err, p.dropIfHidden(ctx, item, written))
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
	var made []string
	for _, d := range results {
		for _, f := range d.outputs {
			made = append(made, f.Blob)
		}
	}
	for _, f := range zipped {
		made = append(made, f.Blob)
	}
	var purge, orphaned []string
	hidden := false
	k := item.Kind()
	_, err = p.c.Manifests.EditExisting(ctx, item.Ref(), func(cur *media.Manifest) error {
		purge, orphaned = purge[:0], orphaned[:0]
		before := map[string]bool{}
		for _, b := range cur.Blobs() {
			before[b] = true
		}
		var gone []string // outputs of uploads removed meanwhile, and of zips that bundled them
		if hidden = cur.Hidden; hidden {
			if err := p.drop(ctx, written); err != nil {
				return err
			}
		}
		for _, d := range results {
			i := cur.Find(d.src.Path)
			if i < 0 || cur.Files[i].Blob != d.src.Blob || cur.Files[i].Edit.Hash() != d.src.Edit.Hash() {
				if i < 0 {
					orphaned = append(orphaned, d.written...)
					for _, f := range d.outputs {
						gone = append(gone, f.Blob)
					}
				}
				continue
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
			if d.dims.W > 0 && !cur.Full { // a Full manifest only shrinks
				cur.Files[i].W, cur.Files[i].H = d.dims.W, d.dims.H
			}
			for _, pr := range d.private {
				if err := cur.SetOutputs(d.src.Path, pr.Name, []media.File{d.outputs[pr.Name]}); err != nil {
					return err
				}
			}
			for _, pu := range d.public {
				cur.ClearPending(d.src.Path, pu.Name)
			}
			purge = append(purge, d.written...)
		}
		for _, z := range zips {
			if f, ok := zipped[z.Name]; ok && f.FP == media.ZipFP(k.ZipInputs(cur, z)) {
				if err := cur.SetOutputs(z.Zip, z.Name, []media.File{f}); err != nil {
					return err
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
		// A blob this edit newly references may have been swept or taken
		// down before it took the folder lock (one shared with an upload
		// removed meanwhile, too): check it is still there.
		after := map[string]bool{}
		for _, b := range cur.Blobs() {
			after[b] = true
		}
		for _, b := range made {
			if !before[b] && after[b] {
				key, _ := item.Blob(b)
				if _, err := p.c.Store.Head(ctx, key); err != nil {
					return fmt.Errorf("media/image: output %s: %w", key, err)
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
			return errors.Join(err, p.dropIfHidden(ctx, item, written))
		}
		return p.dropIfHidden(ctx, item, written)
	case err != nil:
		return err
	}
	if hidden {
		purge, orphaned = written, nil // deleted under the lock
	}
	for _, d := range results {
		if d.err != nil {
			p.failed(ctx, item, d.src.Path, d.err)
		}
	}
	if len(orphaned) > 0 {
		if err := p.syncPublic(ctx, item); err != nil {
			return err
		}
	}
	return p.purge(ctx, purge)
}

// dropIfHidden deletes and purges keys, the public files of a pass that
// failed, if the item is hidden or gone by now: checked under the manifest
// lock, like the closing edit it stands in for. A visible item keeps them:
// they are current, and the retry finds them fresh.
func (p *Processor) dropIfHidden(ctx context.Context, item media.Item, keys []string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	drop := false
	_, err := p.c.Manifests.EditExisting(ctx, item.Ref(), func(cur *media.Manifest) error {
		if drop = cur.Hidden; drop {
			return p.drop(ctx, keys)
		}
		return nil
	})
	if errors.Is(err, media.ErrNotFound) {
		drop, err = true, p.drop(ctx, keys)
	}
	if err != nil || !drop {
		return err
	}
	return p.purge(ctx, keys)
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
			d.err = err
			return d, nil
		}
		return d, err
	}
	k := item.Kind()
	for _, pu := range w.public {
		names := k.PublicNames(pu, w.src.Path)
		outs, dims, err := encodePublic(src, w.src.Type, pu, names, w.src.Edit, p.rules(pu.Image.Animation))
		if d.dims = dims; err != nil {
			return p.permanent(d, err)
		}
		published := false
		_, err = p.c.Manifests.EditExisting(ctx, item.Ref(), func(cur *media.Manifest) error {
			f, ok := cur.Get(w.src.Path)
			if !ok || cur.Hidden || f.Unattached {
				return nil
			}
			if f.Blob != w.src.Blob || f.Edit.Hash() != w.src.Edit.Hash() {
				return nil
			}
			for _, n := range names {
				out := outs[n]
				pk, err := item.Public(n)
				if err != nil {
					return err
				}
				d.written = append(d.written, pk) // a failed put may still land
				if _, err := p.c.Store.Put(ctx, pk, bytes.NewReader(out.webp), int64(len(out.webp)), media.PutOptions{
					ContentType: "image/webp", ChecksumSHA256: sha(out.webp),
					Metadata: map[string]string{"from": url.PathEscape(w.src.Path), "fp": publicFP(w.src, pu)}}); err != nil {
					return err
				}
			}
			published = true
			return nil
		})
		if err != nil && !errors.Is(err, media.ErrNotFound) {
			return d, err
		}
		if !published {
			d.public = nil
			break
		}
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
		blob, err := p.putBlob(ctx, item, bytes.NewReader(out), int64(len(out)), sha(out), "image/webp")
		if err != nil {
			return d, err
		}
		d.outputs[pr.Name] = media.File{Path: k.OutputPath(pr, w.src.Path), Blob: blob, Type: "image/webp", Size: int64(len(out)),
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

// syncPublic deletes (and purges) the public names no attached upload's
// preset expects: a removed upload's, or all of a hidden item's.
func (p *Processor) syncPublic(ctx context.Context, item media.Item) error {
	keys, err := p.c.Manifests.SyncPublic(ctx, item.Ref())
	return errors.Join(err, p.purge(ctx, keys))
}

// editorViews renders the missing editor views of the item's image uploads
// (Config.Editor over the whole oriented source), named by source and spec.
func (p *Processor) editorViews(ctx context.Context, item media.Item, m *media.Manifest) error {
	want := map[string]media.File{}
	for _, f := range m.Files {
		if n := p.reg.EditorView(f); n != "" && f.Fail() == nil {
			want[n] = f
		}
	}
	if len(want) == 0 {
		return nil
	}
	for o, err := range p.c.Store.List(ctx, item.PrivatePrefix()) {
		if err != nil {
			return err
		}
		delete(want, strings.TrimPrefix(o.Key, item.PrivatePrefix()))
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.c.Workers)
	for name, f := range want {
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
			dst, _ := item.Blob(name)
			_, err = p.c.Store.Put(gctx, dst, bytes.NewReader(out), int64(len(out)), media.PutOptions{ContentType: "image/webp"})
			return err
		})
	}
	return g.Wait()
}

func (p *Processor) rules(animation media.Animation) rules {
	return rules{maxPixels: p.c.MaxPixels, maxFrames: p.c.MaxFrames, maxSeconds: p.c.MaxAnimationSeconds, animation: animation}
}

// putBlob stores an immutable blob at private/{sha256} unless it exists.
func (p *Processor) putBlob(ctx context.Context, item media.Item, body io.Reader, size int64, sum []byte, contentType string) (string, error) {
	name := layout.SHA256Name(sum)
	key, err := item.Blob(name)
	if err != nil {
		return "", err
	}
	if obj, err := p.c.Store.Head(ctx, key); err == nil && obj.Size == size {
		return name, nil
	} else if err != nil && !errors.Is(err, media.ErrNotFound) {
		return "", err
	}
	opts := media.PutOptions{ContentType: contentType, ChecksumSHA256: sum}
	if p.c.Store.Capabilities().ConditionalPut {
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

func (p *Processor) purge(ctx context.Context, keys []string) error {
	if p.c.Purge == nil || len(keys) == 0 {
		return nil
	}
	return p.c.Purge(ctx, keys)
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
