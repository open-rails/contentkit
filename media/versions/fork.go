package versions

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/media"
)

// Fork fills Version, a version still without media, from From, a sibling
// of the same content: its uploads (audio only with Playback; never page
// archives or previews), copied with their outputs, its reader metadata
// (audio cues only with Playback) and, with Segments, its segments.
type Fork struct {
	Version  Target
	From     string
	Playback bool
	Segments bool
}

const (
	forkMeta   = "fork"   // the destination's barrier while it is copied
	forkedMeta = "forked" // a finished fork's receipt
)

type forkIntent struct {
	From     string `json:"from"`
	Snapshot string `json:"snapshot"`
	Playback bool   `json:"playback,omitempty"`
	Segments bool   `json:"segments,omitempty"`
}

// errForkConflict is a fork whose destination holds other media, or whose
// source changed while it was copied: discard the destination and fork again.
var errForkConflict = errors.New("versions: fork destination has other media or its source changed")

// ForkTx queues forks in tx; each runs as a durable job, and its destination
// takes no other writes until it finishes.
func (s *Service) ForkTx(ctx context.Context, tx pgx.Tx, forks []Fork) error {
	c, err := s.bound()
	if err != nil {
		return err
	}
	var params []river.InsertManyParams
	for _, f := range forks {
		params = append(params, river.InsertManyParams{Args: forkArgs{Content: f.Version.Content, Version: f.Version.Version, From: f.From,
			Playback: f.Playback, Segments: f.Segments}, InsertOpts: &river.InsertOpts{Queue: Queue, UniqueOpts: media.PendingOnce}})
	}
	if len(params) == 0 {
		return nil
	}
	_, err = c.InsertManyTx(ctx, tx, params)
	return err
}

func intent(m *media.Manifest, key string) (forkIntent, bool) {
	var out forkIntent
	if m == nil || m.Meta[key] == nil {
		return out, false
	}
	b, err := json.Marshal(m.Meta[key])
	return out, err == nil && json.Unmarshal(b, &out) == nil
}

// forkSource is what a fork copies from m, and its fingerprint.
func forkSource(k *media.Kind, m *media.Manifest, playback bool) (string, []media.File, map[string]any, error) {
	meta := maps.Clone(m.Meta)
	delete(meta, forkMeta)
	delete(meta, forkedMeta)
	if rm, ok := readerMetaOf(m); ok && !playback {
		rm.Background, rm.Foreground = nil, nil
		meta[readerMeta] = rm
	}
	var audio string
	if k.Publish != nil && k.Publish.Playback != "" {
		audio = private(k, k.Publish.Playback).From
	}
	var files []media.File
	for _, f := range m.Files {
		if !f.IsUpload() || f.Unattached || k.Publish != nil && (k.Publish.Import != "" && inPattern(k.Publish.Import, f.Path) ||
			k.Publish.Previews != "" && inPattern(public(k, k.Publish.Previews).From, f.Path)) || !playback && audio != "" && inPattern(audio, f.Path) {
			continue
		}
		if f.Blob == "" {
			return "", nil, nil, ErrNotReady
		}
		files = append(files, media.File{Path: f.Path, Blob: f.Blob, Edit: f.Edit, Frame: f.Frame, Meta: f.Meta})
	}
	b, err := json.Marshal(struct {
		Files []media.File   `json:"files"`
		Meta  map[string]any `json:"meta"`
	}{files, meta})
	if err != nil {
		return "", nil, nil, err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), files, meta, nil
}

// fork runs one Fork job. The destination's barrier is written before any
// copy and cleared after the last, so a retry resumes the same copy.
func (s *Service) fork(ctx context.Context, a forkArgs) error {
	dst, src := Target{Content: a.Content, Version: a.Version}, Target{Content: a.Content, Version: a.From}
	if dst == src {
		return errForkConflict
	}
	vs, err := s.versions(ctx, s.o.Pool, []Target{dst, src})
	if err != nil {
		return err
	}
	if len(vs) != 2 {
		return ErrNotFound
	}
	mk, err := s.reg.Kind(vs[dst].Kind)
	if err != nil || !mk.Versions {
		return ErrNotFound
	}
	k := mk
	first, second := dst, src
	if second.Version < first.Version {
		first, second = second, first
	}
	for _, t := range []Target{first, second} {
		unlock, err := s.lockSource(ctx, t)
		if err != nil {
			return err
		}
		defer unlock()
	}
	dref, err := s.ref(k.Name, dst)
	if err != nil {
		return err
	}
	sref, err := s.ref(k.Name, src)
	if err != nil {
		return err
	}
	dest, _, err := s.m.Manifests.Get(ctx, dref)
	if err != nil && !errors.Is(err, media.ErrNotFound) {
		return err
	}
	prior, pending := intent(dest, forkMeta)
	if done, ok := intent(dest, forkedMeta); ok && !pending {
		if done.From == a.From && done.Playback == a.Playback && done.Segments == a.Segments {
			return nil
		}
		return errForkConflict
	}
	if !pending && dest != nil && (len(dest.Files) > 0 || len(dest.Meta) > 0) {
		return errForkConflict
	}
	from, _, err := s.m.Manifests.Get(ctx, sref)
	if errors.Is(err, media.ErrNotFound) {
		from = &media.Manifest{V: media.ManifestVersion}
	} else if err != nil {
		return err
	}
	if from.Meta[forkMeta] != nil {
		return ErrNotReady
	}
	snap, files, meta, err := forkSource(k, from, a.Playback)
	if err != nil {
		return err
	}
	want := forkIntent{From: a.From, Snapshot: snap, Playback: a.Playback, Segments: a.Segments}
	if pending && prior != want {
		return errForkConflict
	}
	if !pending {
		barrier := maps.Clone(meta)
		if barrier == nil {
			barrier = map[string]any{}
		}
		barrier[forkMeta] = want
		if _, err := s.m.Uploads.Commit(ctx, system, dref, uuid.NewString(), []media.Op{{Op: media.OpMeta, Meta: barrier}}); err != nil {
			return err
		}
	}
	var ops []media.Op
	pages := 0
	for _, f := range files {
		ops = append(ops, media.Op{Op: media.OpCopy, From: &media.CopyFrom{ID: a.Content, Version: a.From, Path: f.Path}, To: f.Path})
		if k.Publish != nil && inPattern(k.Publish.Pages, f.Path) {
			at := pages
			ops = append(ops, media.Op{Op: media.OpMove, Path: f.Path, Index: &at})
			pages++
		}
	}
	for batch := range slices.Chunk(ops, 1000) {
		if _, err := s.m.Uploads.Commit(ctx, system, dref, uuid.NewString(), batch); err != nil {
			return err
		}
	}
	now, _, err := s.m.Manifests.Get(ctx, sref)
	if err != nil && !errors.Is(err, media.ErrNotFound) {
		return err
	}
	if now == nil {
		now = &media.Manifest{V: media.ManifestVersion}
	}
	if again, _, _, err := forkSource(k, now, a.Playback); err != nil || again != snap {
		return errors.Join(errForkConflict, err)
	}
	err = pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		if a.Segments {
			if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.segments+` (tenant_id, version_id, position, title, start_index, end_index, release_at)
SELECT tenant_id, $2, position, title, start_index, end_index, release_at FROM `+s.t.segments+` WHERE tenant_id = $1 AND version_id = $3
AND NOT EXISTS (SELECT 1 FROM `+s.t.segments+` WHERE tenant_id = $1 AND version_id = $2)`, s.o.Tenant, a.Version, a.From); err != nil {
				return err
			}
		}
		return s.enqueueTx(ctx, tx, publishArgs{Content: a.Content, Version: a.Version})
	})
	if err != nil {
		return err
	}
	_, err = s.m.Manifests.EditExisting(ctx, dref, func(m *media.Manifest) error {
		delete(m.Meta, forkMeta)
		m.Meta[forkedMeta] = want
		return nil
	})
	return err
}

// RequestFork queues f for an editor of its destination who sees its source.
func (s *Service) RequestFork(ctx context.Context, actor access.Actor, f Fork) error {
	src := Target{Content: f.Version.Content, Version: f.From}
	if f.From == "" || src == f.Version {
		return fmt.Errorf("%w: a fork needs another version of the content", ErrInvalid)
	}
	vs, err := s.versions(ctx, s.o.Pool, []Target{f.Version, src})
	if err != nil {
		return err
	}
	if len(vs) != 2 {
		return ErrNotFound
	}
	res, err := s.decide(ctx, actor, vs[src].Kind, []Target{f.Version, src})
	if err != nil {
		return err
	}
	switch {
	case !res[f.Version].Visible || !res[src].Visible:
		return ErrNotFound
	case !res[f.Version].Editor:
		return ErrForbidden
	}
	return pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error { return s.ForkTx(ctx, tx, []Fork{f}) })
}
