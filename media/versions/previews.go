package versions

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/media"
)

// A locked version shows its leading released pages: the Previews preset's
// uploads in the version's item, each a put of a current page's first
// rendition blob. They are public files, shown to whoever sees the version.

// applyPreviews brings v's preview uploads to its current pages
// publication: min(First, pages, released) of them.
func (s *Service) applyPreviews(ctx context.Context, k *media.Kind, v Target) error {
	unlock, err := s.lockSource(ctx, v)
	if err != nil {
		return err
	}
	defer unlock()
	p, err := s.current(ctx, s.o.Pool, v, kindPages)
	if err != nil || p == nil {
		return err
	}
	count := 0
	var snap pagesSnapshot
	if k.Publish.Previews != "" {
		if err := json.Unmarshal(p.Source, &snap); err != nil {
			return err
		}
		segs, err := s.segments(ctx, s.o.Pool, []string{v.Version})
		if err != nil {
			return err
		}
		count = min(public(k, k.Publish.Previews).First, len(snap.Pages), released(segs[v.Version], len(snap.Pages), s.o.Now()))
		ref, err := s.ref(k.Name, v)
		if err != nil {
			return err
		}
		m, _, err := s.m.Manifests.Get(ctx, ref)
		if err != nil && !errors.Is(err, media.ErrNotFound) {
			return err
		}
		want := map[string]rendition{}
		for i := range count {
			want[previewDir(k)+strconv.Itoa(i+1)+".webp"] = snap.Pages[i].Files[0]
		}
		var ops []media.Op
		if m != nil {
			for _, f := range m.Files {
				if r, ok := want[f.Path]; f.IsUpload() && inPattern(public(k, k.Publish.Previews).From, f.Path) && (!ok || !sameBlob(f.Blob, r.Blob)) {
					ops = append(ops, media.Op{Op: media.OpRemove, Path: f.Path})
				}
			}
		}
		for i := range count {
			path := previewDir(k) + strconv.Itoa(i+1) + ".webp"
			if m != nil {
				if f, ok := m.Get(path); ok && sameBlob(f.Blob, want[path].Blob) {
					continue
				}
			}
			ops = append(ops, media.Op{Op: media.OpPut, Path: path, Blob: want[path].Blob})
		}
		if len(ops) > 0 {
			if _, err := s.m.Uploads.Commit(ctx, system, ref, uuid.NewString(), ops); err != nil {
				return err
			}
		}
	}
	return pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE `+s.t.publications+` SET previews = $3, previews_at = now() WHERE tenant_id = $1 AND id = $2`,
			s.o.Tenant, p.ID, count)
		return err
	})
}

// withdrawPreviews removes v's preview uploads: their pages publication went.
// It runs from a commit's hook, so it takes no source lock.
func (s *Service) withdrawPreviews(ctx context.Context, k *media.Kind, v Target) error {
	if k.Publish.Previews == "" {
		return nil
	}
	ref, err := s.ref(k.Name, v)
	if err != nil {
		return err
	}
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if errors.Is(err, media.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	var ops []media.Op
	for _, f := range m.Files {
		if f.IsUpload() && inPattern(public(k, k.Publish.Previews).From, f.Path) {
			ops = append(ops, media.Op{Op: media.OpRemove, Path: f.Path})
		}
	}
	if len(ops) == 0 {
		return nil
	}
	_, err = s.m.Uploads.Commit(ctx, system, ref, uuid.NewString(), ops)
	return err
}
