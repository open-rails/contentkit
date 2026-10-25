package versions

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/media"
)

// Versions share no files: each holds its own copy of its content's body
// images (Upload.Body) in its private/, named as the body names them
// (i-{uuid}).

// BodyCopy copies version From's body images into each version of To: those
// Names, or all when Names is empty. A name a version holds already stays.
type BodyCopy struct {
	Content string   `json:"content"`
	From    string   `json:"from"`
	To      []string `json:"to"`
	Names   []string `json:"names,omitempty"`
}

func (BodyCopy) Kind() string { return "contentkit_version_body_copy" }

// CopyBodiesTx queues copies in tx; each runs as a durable job. Copies of a
// kind without body images do nothing.
func (s *Service) CopyBodiesTx(ctx context.Context, tx pgx.Tx, copies []BodyCopy) error {
	c, err := s.bound()
	if err != nil {
		return err
	}
	var params []river.InsertManyParams
	for _, cp := range copies {
		cp.To = slices.DeleteFunc(slices.Clone(cp.To), func(v string) bool { return v == cp.From })
		if cp.From != "" && len(cp.To) > 0 {
			params = append(params, river.InsertManyParams{Args: cp, InsertOpts: &river.InsertOpts{Queue: QueueFor(s.o.Tenant)}})
		}
	}
	if len(params) == 0 {
		return nil
	}
	_, err = c.InsertManyTx(ctx, tx, params)
	return err
}

// copyBodies runs one BodyCopy.
func (s *Service) copyBodies(ctx context.Context, c BodyCopy) error {
	v, err := s.one(ctx, Target{Content: c.Content, Version: c.From})
	if err != nil {
		return err
	}
	k, err := s.reg.Kind(v.Kind)
	if err != nil || !slices.ContainsFunc(k.Uploads, func(u media.Upload) bool { return u.Body }) {
		return nil // no body images
	}
	src, err := s.ref(k.Name, Target{Content: c.Content, Version: c.From})
	if err != nil {
		return err
	}
	m, _, err := s.m.Manifests.Get(ctx, src)
	if errors.Is(err, media.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	var files []media.File
	for _, f := range m.Files {
		if !f.IsUpload() || f.Unattached || !k.IsBody(f.Path) || len(c.Names) > 0 && !slices.Contains(c.Names, k.NameOf(f.Path)) {
			continue
		}
		if f.Blob == "" && f.Fail() == nil {
			return ErrNotReady
		}
		if f.Blob != "" {
			files = append(files, f)
		}
	}
	for _, to := range c.To {
		dst, err := s.ref(k.Name, Target{Content: c.Content, Version: to})
		if err != nil {
			return err
		}
		have, _, err := s.m.Manifests.Get(ctx, dst)
		if err != nil && !errors.Is(err, media.ErrNotFound) {
			return err
		}
		var ops []media.Op
		for _, f := range files {
			if have != nil && have.Find(f.Path) >= 0 {
				continue
			}
			ops = append(ops, media.Op{Op: media.OpCopy, From: &media.CopyFrom{ID: c.Content, Version: c.From, Path: f.Path}, To: f.Path})
		}
		for batch := range slices.Chunk(ops, 1000) {
			if _, err := s.m.Uploads.Commit(ctx, system, dst, uuid.NewString(), batch); err != nil {
				return err
			}
		}
	}
	return nil
}

type bodyCopyWorker struct {
	river.WorkerDefaults[BodyCopy]
	s *Service
}

func (w *bodyCopyWorker) Timeout(*river.Job[BodyCopy]) time.Duration { return 30 * time.Minute }

func (w *bodyCopyWorker) Work(ctx context.Context, job *river.Job[BodyCopy]) error {
	return outcome(w.s.copyBodies(ctx, job.Args))
}
