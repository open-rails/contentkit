package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/workqueue"
)

// standIn is the media worker's stand-in: a job runs shortly after it is
// enqueued, like the real worker's, places staged uploads
// (Manifests.Place) and settles every pending output without encoding. A private image output and the public names reuse the
// upload's bytes, a frame grab is a flat image, a video measures 10 s at
// 1280×720, and missing editor views are copies of their uploads.
type standIn struct {
	store     media.Store
	reg       *media.Registry
	manifests *media.Manifests
	wg        sync.WaitGroup
}

const standInRecipe = "stand-in"

// Commit jobs are delivered by real River transactions; only encoding is a stand-in.
type placeWorker struct {
	river.WorkerDefaults[workqueue.PlaceArgs]
	worker *standIn
}

func (w *placeWorker) Work(ctx context.Context, job *river.Job[workqueue.PlaceArgs]) error {
	return w.worker.process(ctx, media.ProcessJob{Ref: job.Args.Ref, Preset: job.Args.Preset, Force: job.Args.Force, Place: true})
}

type imageWorker struct {
	river.WorkerDefaults[workqueue.ImageArgs]
	worker *standIn
}

func (w *imageWorker) Work(ctx context.Context, job *river.Job[workqueue.ImageArgs]) error {
	return w.worker.process(ctx, media.ProcessJob{Ref: job.Args.Ref, Preset: job.Args.Preset, Force: job.Args.Force, Editor: job.Args.Editor})
}

type videoWorker struct {
	river.WorkerDefaults[workqueue.VideoPlanArgs]
	worker *standIn
}

func (w *videoWorker) Work(ctx context.Context, job *river.Job[workqueue.VideoPlanArgs]) error {
	return w.worker.process(ctx, media.ProcessJob{Ref: job.Args.Ref, Preset: job.Args.Preset, Force: job.Args.Force})
}

func (w *standIn) Enqueue(_ context.Context, j media.ProcessJob) error {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		time.Sleep(200 * time.Millisecond)
		if err := w.process(context.Background(), j); err != nil {
			log.Printf("stand-in worker %s: %v", j.Ref, err)
		}
	}()
	return nil
}

// Frame is the frame picker's still: a flat JPEG, 16:9 at width (default 64).
func (w *standIn) Frame(_ context.Context, _ media.Item, _ media.File, t float64, width int) ([]byte, error) {
	if width == 0 {
		width = 64
	}
	var b bytes.Buffer
	err := jpeg.Encode(&b, flat(width, width*9/16, uint8(t)), nil)
	return b.Bytes(), err
}

func flat(w, h int, shade uint8) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = shade, 80, 160, 255
	}
	return img
}

func (w *standIn) process(ctx context.Context, j media.ProcessJob) error {
	item, err := w.reg.Item(j.Ref)
	if err != nil {
		return err
	}
	if _, err := w.manifests.Place(ctx, j.Ref); err != nil {
		return err
	}
	k := item.Kind()
	_, err = w.manifests.EditExisting(ctx, j.Ref, func(m *media.Manifest) error {
		public := map[string]bool{}
		for _, f := range slices.Clone(m.Files) {
			if !f.IsUpload() || f.Gone {
				continue
			}
			if f, err = w.settle(ctx, item, m, f); err != nil {
				return err
			}
			if fp := w.reg.EditorFingerprint(f); j.Editor && fp != "" && w.reg.EditorView(f) == "" {
				sum, _ := layout.BlobDigest(f.Blob)
				view, err := w.manifests.NewBlob(ctx, item.Ref(), sum)
				if err != nil {
					return err
				}
				if err := w.copy(ctx, item, f.Blob, item.PrivatePrefix()+view); err != nil {
					return err
				}
				m.Files[m.Find(f.Path)].Editor = &media.EditorImage{Blob: view, FP: fp}
			}
		}
		// Public names no upload feeds any more go, as the worker's sync does.
		for _, name := range k.PublicKept(m) {
			public[name] = true
		}
		for o, err := range w.store.List(ctx, item.PublicPrefix()) {
			if err != nil {
				return err
			}
			if !public[strings.TrimPrefix(o.Key, item.PublicPrefix())] {
				if err := w.store.Delete(ctx, o.Key); err != nil {
					return err
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

// settle grabs, measures and renders upload f in m.
func (w *standIn) settle(ctx context.Context, item media.Item, m *media.Manifest, f media.File) (media.File, error) {
	k := item.Kind()
	i := m.Find(f.Path)
	if f.Frame != nil && f.Blob == "" {
		var b bytes.Buffer
		if err := png.Encode(&b, flat(64, 36, uint8(f.Frame.T))); err != nil {
			return f, err
		}
		sum := sha256.Sum256(b.Bytes())
		blob, err := w.manifests.NewBlob(ctx, item.Ref(), sum[:])
		if err != nil {
			return f, err
		}
		f.Blob, f.Type, f.Size, f.W, f.H = blob, "image/png", int64(b.Len()), 64, 36
		f.Frame = &media.Frame{T: f.Frame.T, Auto: f.Frame.Auto, Of: w.video(k, m, f)}
		key, _ := item.Blob(f.Blob)
		if _, err := w.store.Put(ctx, key, bytes.NewReader(b.Bytes()), f.Size, media.PutOptions{ContentType: f.Type}); err != nil {
			return f, err
		}
	}
	if f.Blob == "" {
		return f, nil
	}
	key, _ := item.Blob(f.Blob)
	switch {
	case strings.HasPrefix(f.Type, "image/") && f.W == 0:
		rc, _, err := w.store.Get(ctx, key, media.GetOptions{})
		if err != nil {
			return f, err
		}
		cfg, _, err := image.DecodeConfig(io.LimitReader(rc, 1<<20))
		rc.Close()
		if err == nil {
			f.W, f.H = cfg.Width, cfg.Height
		}
	case strings.HasPrefix(f.Type, "video/") && f.Dur == 0:
		f.Dur, f.W, f.H = 10, 1280, 720
	}
	m.Files[i] = f
	for _, p := range k.PrivateFor(f.Path) {
		if p.Image != nil && slices.Contains(f.Pending, p.Name) {
			out := media.File{Path: k.OutputPath(p, f.Path), Blob: f.Blob, Type: f.Type, Size: f.Size, W: f.W, H: f.H,
				FP: media.SpecFP(f, p.Image, standInRecipe)}
			if err := m.SetOutputs(f.Path, p.Name, []media.File{out}); err != nil {
				return f, err
			}
		}
	}
	for _, p := range k.PublicFor(f.Path) {
		if !slices.Contains(f.Pending, p.Name) {
			continue
		}
		names := k.PublicNames(m, p, f.Path)
		if len(names) == 0 || m.Hidden || f.Unattached {
			continue
		}
		pub := media.Publication{Preset: p.Name, Source: f.Key(), FP: standInRecipe, Generation: uuid.NewString(),
			Names: names, Dims: make([]media.Dims, len(names)), State: media.PublicationReady}
		for i := range pub.Dims {
			pub.Dims[i] = media.Dims{W: f.W, H: f.H}
		}
		for _, name := range pub.NamesOnDisk() {
			if err := w.copy(ctx, item, f.Blob, item.PublicPrefix()+name); err != nil {
				return f, err
			}
		}
		m.SetPublication(f.Path, pub)
		m.ClearPending(f.Path, p.Name)
	}
	return m.Files[m.Find(f.Path)], nil
}

// video is the blob of the video upload a frame upload f is grabbed from.
func (w *standIn) video(k *media.Kind, m *media.Manifest, f media.File) string {
	for _, u := range k.Uploads {
		if u.Frames == "" || !strings.HasPrefix(f.Path, u.Path+".") {
			continue
		}
		for _, v := range m.Files {
			if v.IsUpload() && strings.HasPrefix(v.Path, u.Frames+".") {
				return v.Blob
			}
		}
	}
	return ""
}

func (w *standIn) copy(ctx context.Context, item media.Item, blob, dst string) error {
	src, err := item.Blob(blob)
	if err != nil {
		return err
	}
	_, err = w.store.Copy(ctx, src, dst, media.CopyOptions{})
	return err
}
