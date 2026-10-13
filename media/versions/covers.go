package versions

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/media"
)

// A reader content without a cover of its own shows its first published
// page: its content-level cover upload is a copy of the leading released
// page of its first live version, marked autoCover. A cover an editor
// uploads replaces it and is never touched; once that is removed, the next
// publication brings the automatic one back.
const autoCover = "auto_cover"

// applyCover brings content's automatic cover to its source page, removing
// it when no live version shows a released page.
func (s *Service) applyCover(ctx context.Context, kind, content string) error {
	ref, err := s.reg.Ref(kind, content)
	if err != nil {
		return nil // no content-level uploads
	}
	item, err := s.reg.Item(ref)
	if err != nil {
		return nil
	}
	cover := public(item.Kind(), media.CoverPreset)
	if cover == nil {
		return nil
	}
	unlock, err := media.PGLocker(s.o.Pool).Lock(ctx, "contentkit:covers:"+s.o.Tenant+":"+content)
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
			if f.Meta[autoCover] != true {
				return nil // the editor's
			}
			auto = &f
		}
	}
	src, page, err := s.coverSource(ctx, kind, content)
	if err != nil {
		return err
	}
	var ops []media.Op
	switch {
	case page == nil && auto == nil:
		return nil
	case page == nil:
		ops = []media.Op{{Op: media.OpRemove, Path: auto.Path}}
	case auto != nil && sameBlob(auto.Blob, page.Blob):
		return nil
	default:
		ops = []media.Op{{Op: media.OpCopy, From: &media.CopyFrom{ID: content, Version: src, Path: page.Path}, To: coverTo(item.Kind(), cover.From, auto),
			Meta: map[string]any{autoCover: true}}}
	}
	_, err = s.m.Uploads.Commit(ctx, system, ref, uuid.NewString(), ops)
	if ue, ok := media.AsUploadError(err); ok && ue.Code != media.CodeUnavailable {
		// A page the cover upload cannot take (its type, its size, a dropped
		// original) leaves the content on its kind's default cover.
		s.o.Logger.WarnContext(ctx, "versions: no automatic cover", "content", content, "error", err)
		return nil
	}
	return err
}

// coverSource is the leading page upload of content's first live version
// whose current pages publication has it released, nil without one.
func (s *Service) coverSource(ctx context.Context, kind, content string) (string, *media.File, error) {
	var version string
	err := s.o.Pool.QueryRow(ctx, `SELECT v.id FROM `+s.t.versions+` v
JOIN `+s.t.publications+` p ON p.tenant_id = v.tenant_id AND p.version_id = v.id AND p.kind = 'pages' AND p.state = 'current'
WHERE v.tenant_id = $1 AND v.content_id = $2 AND v.state = 'published' AND (v.publish_at IS NULL OR v.publish_at <= now())
ORDER BY v.position, v.id LIMIT 1`, s.o.Tenant, content).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil
	} else if err != nil {
		return "", nil, err
	}
	t := Target{Content: content, Version: version}
	p, err := s.current(ctx, s.o.Pool, t, kindPages)
	if err != nil || p == nil {
		return "", nil, err
	}
	var snap pagesSnapshot
	if err := json.Unmarshal(p.Source, &snap); err != nil {
		return "", nil, err
	}
	segs, err := s.segments(ctx, s.o.Pool, []string{version})
	if err != nil {
		return "", nil, err
	}
	if len(snap.Pages) == 0 || released(segs[version], len(snap.Pages), s.o.Now()) == 0 {
		return "", nil, nil
	}
	ref, err := s.ref(kind, t)
	if err != nil {
		return "", nil, err
	}
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if errors.Is(err, media.ErrNotFound) {
		return "", nil, nil
	} else if err != nil {
		return "", nil, err
	}
	f, ok := m.Get(snap.Pages[0].Path)
	if !ok || f.Blob == "" {
		return "", nil, nil
	}
	return version, &f, nil
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
