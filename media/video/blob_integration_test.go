package video_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/video"
	"github.com/open-rails/contentkit/media/workqueue"
)

type sweepBeforePublishLocker struct {
	media.Locker
	before func() error
	skip   int
}

func (l *sweepBeforePublishLocker) Lock(ctx context.Context, key string) (func(), error) {
	if l.before != nil {
		if l.skip > 0 {
			l.skip--
		} else {
			before := l.before
			l.before = nil
			if err := before(); err != nil {
				return nil, err
			}
		}
	}
	return l.Locker.Lock(ctx, key)
}

func TestPublicationRetriesOutputsRemovedBySweep(t *testing.T) {
	for _, path := range []string{"audio", "video", "assembly", "sidecar", "source-subtitles", "poster"} {
		t.Run(path, func(t *testing.T) {
			ctx := t.Context()
			a := media.Audio{}
			e, _ := newAudioEnv(t, a, nil)
			locker := &sweepBeforePublishLocker{Locker: s3test.Locker(t, e.store)}
			var err error
			e.encoder, err = video.New(ctx, video.Config{Store: e.store, Locker: locker, TempDir: t.TempDir(),
				Threads: 2, Encoder: video.EncoderCPU, Codecs: []media.Codec{media.CodecH264}})
			if err != nil {
				t.Fatal(err)
			}
			job := video.Job{Ref: e.ref, Versioned: true, Video: media.Video{PosterWidths: posterWidths}, Audio: &a}
			switch path {
			case "audio":
				e.commitFile(t, audioFixture{ext: "wav", codec: "pcm_s16le", secs: 2, rate: 48000}.make(t), "source", "audio/wav", media.OpInsert)
			case "sidecar":
				e.commitOps(t, e.putSidecar(t, "source.vtt", media.OpInsert, []byte("WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nHello\n"), nil))
			default:
				e.commit(t, fixture{w: 640, h: 360, secs: 2, audio: 1, subs: true, tone: 440}.make(t), media.OpInsert)
			}
			if err := e.encoder.Encode(ctx, job, nil); err != nil {
				t.Fatal(err)
			}
			run := func() error { return e.encoder.Encode(ctx, job, nil) }
			var retry func()
			if path == "assembly" {
				pool := pgtest.Pool(t, nil)
				schema := pgtest.EmptySchema(t, ctx, pool)
				if err := workqueue.Migrate(ctx, pool, schema); err != nil {
					t.Fatal(err)
				}
				wc := video.WorkerConfig{Encoder: e.encoder, Pool: pool, Schema: schema, Kinds: e.kinds}
				contribution, err := video.Contribution(wc)
				if err != nil {
					t.Fatal(err)
				}
				cfg := video.ClientConfig(wc)
				cfg.FetchPollInterval = 100 * time.Millisecond
				worker, err := riverhelpers.New(ctx, pool, cfg, contribution)
				if err != nil {
					t.Fatal(err)
				}
				events, stop := worker.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed, river.EventKindJobCancelled)
				defer stop()
				defer func() {
					stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
					defer cancel()
					if err := worker.StopAndCancel(stopCtx); err != nil {
						t.Error(err)
					}
				}()
				started := false
				var failedID int64
				run = func() error {
					if !started {
						if _, err := worker.Insert(ctx, workqueue.VideoPlanArgs{Ref: e.ref}, workqueue.VideoPlanInsertOpts("")); err != nil {
							return err
						}
						if err := worker.Start(ctx); err != nil {
							return err
						}
						started = true
					}
					for {
						select {
						case event := <-events:
							if event.Kind == river.EventKindJobFailed {
								failedID = event.Job.ID
								return errors.New(event.Job.Errors[len(event.Job.Errors)-1].Error)
							}
							if event.Kind != river.EventKindJobCompleted {
								return fmt.Errorf("unexpected job event %s", event.Kind)
							}
							if event.Job.Kind == (workqueue.VideoAssembleArgs{}).Kind() {
								return nil
							}
						case <-ctx.Done():
							return ctx.Err()
						}
					}
				}
				retry = func() {
					if _, err := worker.JobRetry(ctx, failedID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if path == "poster" {
				err = e.manifests.UpdateSlot(ctx, e.ref.Content(), media.PosterSlot, func(rec *media.SlotRecord) error {
					*rec = media.SlotRecord{}
					return nil
				})
				locker.skip = 1 // Encode's unchanged download-cleanup edit precedes the poster.
			} else {
				_, err = e.manifests.Edit(ctx, e.ref, func(m *media.Manifest) error {
					f := &m.Files[0]
					if path == "source-subtitles" {
						f.HLS.Subs, f.HLS.SubsSpec = nil, "old"
					} else {
						f.HLS, f.Variants, f.Derived, m.Downloads = nil, nil, "", nil
					}
					return nil
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			_, before, err := e.manifests.Root(ctx, e.ref)
			if err != nil {
				t.Fatal(err)
			}
			jobs, err := media.NewJobs(media.JobsConfig{Store: e.store, Kinds: e.kinds, Locker: locker.Locker,
				Now: func() time.Time { return time.Now().Add(72 * time.Hour) }})
			if err != nil {
				t.Fatal(err)
			}
			locker.before = func() error {
				result, err := jobs.Sweep(ctx, e.ref)
				if err != nil {
					return err
				}
				if len(result.Deleted) == 0 {
					return fmt.Errorf("sweep did not remove unused outputs: %+v", result)
				}
				return nil
			}
			err = run()
			if path == "assembly" {
				if err == nil || !strings.Contains(err.Error(), "publish output") {
					t.Fatalf("assembly after sweep: %v", err)
				}
			} else if !errors.Is(err, media.ErrNotFound) {
				t.Fatalf("publication after sweep: %v", err)
			}
			if _, after, err := e.manifests.Root(ctx, e.ref); err != nil || after != before {
				t.Fatalf("failed publication changed manifest: %s -> %s, %v", before, after, err)
			}
			if retry != nil {
				retry()
			}
			if err := run(); err != nil {
				t.Fatalf("retry: %v", err)
			}
			root, _, err := e.manifests.Root(ctx, e.ref)
			if err != nil {
				t.Fatal(err)
			}
			for key := range root.Refs() {
				if _, err := e.store.Head(ctx, e.item(t).Prefix()+key); err != nil {
					t.Fatalf("retry left missing reference %s: %v", key, err)
				}
			}
		})
	}
}
