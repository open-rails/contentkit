package versions

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/media"
)

// A reader version without a cover of its own shows its first released
// page: its cover upload is a copy of that page, marked media.MetaAutoCover.
// A cover an editor uploads replaces it and is never touched; once that is
// removed, the next publication brings the automatic one back.

// applyCover brings version t's automatic cover to its source page, removing
// it when the version shows no released page.
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
		if m != nil && m.Meta[archiveReceipts] != nil {
			return s.failArchiveCover(ctx, k, t, "cover_source_unavailable")
		}
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
		if m != nil && m.Meta[archiveReceipts] != nil && permanentCoverRejection(ue.Code) {
			return s.failArchiveCover(ctx, k, t, ue.Code)
		}
		return nil
	}
	if err == nil && page == nil && m != nil && m.Meta[archiveReceipts] != nil {
		return s.failArchiveCover(ctx, k, t, "cover_source_unavailable")
	}
	return err
}

func permanentCoverRejection(code string) bool {
	switch code {
	case media.CodeInvalid, media.CodeNotFound, media.CodeTooManyFiles, media.CodeTooLarge, media.CodeType,
		media.CodeImageTooSmall, media.CodeImageTooLarge, media.CodeImageUnreadable,
		media.CodeAnimationNotAllowed, media.CodeAnimationTooLong, media.CodeAnimationUnsupported:
		return true
	}
	return false
}

// coverSource is the leading page upload of version t, whose manifest is m,
// while its current pages publication has it released; nil without one.
func (s *Service) coverSource(ctx context.Context, t Target, m *media.Manifest) (*media.File, error) {
	p, err := s.current(ctx, s.o.Pool, t, kindPages)
	if err != nil || p == nil || m == nil {
		return nil, err
	}
	var snap pagesSnapshot
	if err := json.Unmarshal(p.Source, &snap); err != nil {
		return nil, err
	}
	segs, err := s.segments(ctx, s.o.Pool, []string{t.Version})
	if err != nil {
		return nil, err
	}
	if len(snap.Pages) == 0 || released(segs[t.Version], len(snap.Pages), s.o.Now()) == 0 {
		return nil, nil
	}
	f, ok := m.Get(snap.Pages[0].Path)
	if !ok || f.Blob == "" {
		return nil, nil
	}
	return &f, nil
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
