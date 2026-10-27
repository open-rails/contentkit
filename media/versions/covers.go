package versions

import (
	"context"
	"errors"
	"path"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/media"
)

// A reader version without a cover of its own shows its first ready
// page: its cover upload is a copy of that page, marked media.MetaAutoCover.
// A cover an editor uploads replaces it and is never touched; once that is
// removed, the next publication brings the automatic one back.

// applyCover brings version t's automatic cover to its source page, removing
// it when the source page is removed. Release dates do not alter public files.
func (s *Service) applyCover(ctx context.Context, k *media.Kind, t Target) error {
	cover := public(k, media.CoverPreset)
	if cover == nil {
		return nil
	}
	ref, err := s.ref(k.Name, t)
	if err != nil {
		return err
	}
	unlock, err := media.PGLocker(s.o.Pool).Lock(ctx, "contentkit:covers:"+s.o.Tenant+":"+t.Version)
	if err != nil {
		return err
	}
	defer unlock()
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if err != nil && !errors.Is(err, media.ErrNotFound) {
		return err
	}
	var auto *media.File
	if m != nil {
		for _, f := range m.Files {
			if !f.IsUpload() || f.Unattached || !coverPath(cover.From, f.Path) {
				continue
			}
			if f.Meta[media.MetaAutoCover] != true {
				return nil // the editor's
			}
			auto = &f
		}
	}
	page, err := s.coverSource(ctx, t, m)
	if err != nil {
		return err
	}
	var ops []media.Op
	switch {
	case page == nil && auto == nil:
		return nil
	case page == nil:
		ops = []media.Op{{Op: media.OpRemove, Path: auto.Path, Meta: map[string]any{media.MetaAutoCover: true}}}
	case auto != nil && sameBlob(auto.Blob, page.Blob):
		return nil
	default:
		ops = []media.Op{{Op: media.OpCopy, From: &media.CopyFrom{ID: t.Content, Version: t.Version, Path: page.Path}, To: coverTo(k, cover.From, auto),
			Meta: map[string]any{media.MetaAutoCover: true}}}
	}
	_, err = s.m.Uploads.Commit(ctx, system, ref, uuid.NewString(), ops)
	if ue, ok := media.AsUploadError(err); ok && ue.Code != media.CodeUnavailable {
		// A page the cover upload cannot take (its type, its size, a dropped
		// original) leaves the version on its kind's default cover.
		s.o.Logger.WarnContext(ctx, "versions: no automatic cover", "version", t.Version, "error", err)
		return nil
	}
	return err
}

// coverSource uses the prepared authoring page order, so generating the cover
// never depends on an already-published reader snapshot.
func (s *Service) coverSource(ctx context.Context, t Target, m *media.Manifest) (*media.File, error) {
	if m == nil {
		return nil, nil
	}
	v, err := s.one(ctx, t)
	if err != nil {
		return nil, err
	}
	k, err := s.reg.Kind(v.Kind)
	if err != nil {
		return nil, err
	}
	for _, f := range sourcePages(k, m) {
		if media.PageIncluded(f) && f.Blob != "" && f.Fail() == nil {
			return &f, nil
		}
	}
	return nil, nil
}

// coverPath reports path an upload of the cover upload pattern.
func coverPath(pattern, p string) bool {
	if strings.HasSuffix(pattern, "{name}") {
		return inPattern(pattern, p)
	}
	return strings.TrimSuffix(p, path.Ext(p)) == pattern
}

// coverTo is where the automatic cover goes: the cover path, or under a
// {name} pattern, auto's place or a new name.
func coverTo(k *media.Kind, pattern string, auto *media.File) string {
	dir, named := strings.CutSuffix(pattern, "{name}")
	switch {
	case !named:
		return pattern
	case auto != nil:
		return strings.TrimSuffix(auto.Path, path.Ext(auto.Path))
	}
	for _, u := range k.Uploads {
		if u.Path == pattern && u.Named {
			return dir + media.NewName()
		}
	}
	return dir + "auto"
}
