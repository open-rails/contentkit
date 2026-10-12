package versions

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

type resolver struct{ s *Service }

// Resolve decides media reads. A content-level item shows like its content,
// its public files only. A version's item shows like the version; its
// private files open only for a kind that publishes nothing, since a
// publishing kind's originals reach viewers through published items alone.
// A published item shows while its publication is current and its pages
// are released, and opens like its version, or within the free segments.
func (r resolver) Resolve(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	s := r.s
	out := map[contentref.ContentKey]access.Resolution{}
	var owned, published, others []contentref.ContentRef
	for _, ref := range refs {
		k, err := s.reg.Kind(ref.ContentKind)
		switch {
		case err != nil || ref.TenantID != k.NS():
		case k.Versions:
			owned = append(owned, ref)
		case k.Of() != nil:
			published = append(published, ref)
		case k.Name == s.reg.Config().Avatars.Kind:
			out[ref.Key()] = access.Resolution{Visible: true, Editor: actor.Person() && actor.ID == ref.ContentID}
		default:
			others = append(others, ref)
		}
	}
	if err := s.resolveOwned(ctx, owned, actor, out); err != nil {
		return nil, err
	}
	if err := s.resolvePublished(ctx, published, actor, out); err != nil {
		return nil, err
	}
	if len(others) > 0 && s.o.Others != nil {
		more, err := s.o.Others.Resolve(ctx, others, actor)
		if err != nil {
			return nil, err
		}
		for _, ref := range others {
			if res, ok := more[ref.Key()]; ok {
				out[ref.Key()] = res
			}
		}
	}
	return out, nil
}

// existing is the targets of refs whose content, of the ref's kind, and
// version exist.
func (s *Service) existing(ctx context.Context, refs []contentref.ContentRef) (map[contentref.ContentKey]Target, error) {
	out := map[contentref.ContentKey]Target{}
	if len(refs) == 0 {
		return out, nil
	}
	kinds, contents, versions := make([]string, len(refs)), make([]string, len(refs)), make([]string, len(refs))
	for i, ref := range refs {
		kinds[i], contents[i], versions[i] = ref.ContentKind, ref.ContentID, ref.Version()
	}
	rows, err := s.o.Pool.Query(ctx, `SELECT t.n FROM unnest($2::text[], $3::text[], $4::text[]) WITH ORDINALITY AS t(kind, content, version, n)
JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = t.content AND c.kind = t.kind
WHERE t.version = '' OR EXISTS (SELECT 1 FROM `+s.t.versions+` v WHERE v.tenant_id = $1 AND v.id = t.version AND v.content_id = t.content)`,
		s.o.Tenant, kinds, contents, versions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		ref := refs[n-1]
		out[ref.Key()] = Target{Content: ref.ContentID, Version: ref.Version()}
	}
	return out, rows.Err()
}

func (s *Service) resolveOwned(ctx context.Context, refs []contentref.ContentRef, actor access.Actor, out map[contentref.ContentKey]access.Resolution) error {
	found, err := s.existing(ctx, refs)
	if err != nil || len(found) == 0 {
		return err
	}
	var targets, versions []Target
	for _, t := range found {
		targets = append(targets, t)
		if t.Version != "" {
			versions = append(versions, t)
		}
	}
	sights, err := s.o.Visibility.See(ctx, actor, targets)
	if err != nil {
		return err
	}
	opens := map[Target]Opening{}
	if len(versions) > 0 {
		if opens, err = s.o.Access.Open(ctx, actor, versions); err != nil {
			return err
		}
	}
	for _, ref := range refs {
		t, ok := found[ref.Key()]
		if !ok || !sights[t].Visible {
			continue
		}
		res := access.Resolution{Visible: true, Editor: sights[t].Editor}
		if t.Version != "" {
			k, _ := s.reg.Kind(ref.ContentKind)
			res.Accessible = opens[t].Opens && k.VersionKind().Publish == nil
		}
		out[ref.Key()] = res
	}
	return nil
}

// pubItem is one published item and what it shows.
type pubItem struct {
	id, publication, kind string
	version               Target
	current               bool
	start, end            int
	pages                 int
}

func (s *Service) resolvePublished(ctx context.Context, refs []contentref.ContentRef, actor access.Actor, out map[contentref.ContentKey]access.Resolution) error {
	if len(refs) == 0 {
		return nil
	}
	ids := make([]string, len(refs))
	for i, ref := range refs {
		ids[i] = ref.ContentID
	}
	items, err := s.pubItems(ctx, ids)
	if err != nil {
		return err
	}
	var targets []Target
	var vids []string
	for _, it := range items {
		if it.current {
			targets, vids = append(targets, it.version), append(vids, it.version.Version)
		}
	}
	if len(targets) == 0 {
		return nil
	}
	sights, err := s.o.Visibility.See(ctx, actor, targets)
	if err != nil {
		return err
	}
	opens, err := s.o.Access.Open(ctx, actor, targets)
	if err != nil {
		return err
	}
	segs, err := s.segments(ctx, s.o.Pool, vids)
	if err != nil {
		return err
	}
	now := s.o.Now()
	for _, ref := range refs {
		it, ok := items[ref.ContentID]
		if !ok || !it.current {
			continue
		}
		d := decision{sight: sights[it.version], open: opens[it.version], segs: segs[it.version.Version], pages: it.pages, now: now}
		switch {
		case it.kind == kindPlayback && d.playback():
			out[ref.Key()] = access.Resolution{Visible: true, Accessible: true, Editor: d.sight.Editor}
		case it.kind == kindPages && d.shows(it.end):
			out[ref.Key()] = access.Resolution{Visible: true, Accessible: d.opens(it.end), Editor: d.sight.Editor}
		}
	}
	return nil
}

// pubItems loads published items by id: segment items, and playback items
// (a playback publication's id).
func (s *Service) pubItems(ctx context.Context, ids []string) (map[string]pubItem, error) {
	rows, err := s.o.Pool.Query(ctx, `SELECT i.id, p.id, p.kind, v.content_id, p.version_id, p.state = 'current', i.start_index, i.end_index,
	jsonb_array_length(p.source->'pages')
FROM `+s.t.pubItems+` i JOIN `+s.t.publications+` p ON p.tenant_id = i.tenant_id AND p.id = i.publication_id
JOIN `+s.t.versions+` v ON v.tenant_id = p.tenant_id AND v.id = p.version_id
WHERE i.tenant_id = $1 AND i.id = ANY($2)
UNION ALL
SELECT p.id, p.id, p.kind, v.content_id, p.version_id, p.state = 'current', 0, 0, 0
FROM `+s.t.publications+` p JOIN `+s.t.versions+` v ON v.tenant_id = p.tenant_id AND v.id = p.version_id
WHERE p.tenant_id = $1 AND p.id = ANY($2) AND p.kind = 'playback'`, s.o.Tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]pubItem{}
	for rows.Next() {
		var it pubItem
		if err := rows.Scan(&it.id, &it.publication, &it.kind, &it.version.Content, &it.version.Version, &it.current, &it.start, &it.end,
			&it.pages); err != nil {
			return nil, err
		}
		out[it.id] = it
	}
	return out, rows.Err()
}

type uploads struct{ s *Service }

// CanUpload lets editors of a content (staff, its creators, its owner
// group's editors) write its items, a person write their own avatar, and
// this package alone write published items and preview copies. A version
// being forked takes no other writes.
func (u uploads) CanUpload(ctx context.Context, actor access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	s := u.s
	k, err := s.reg.Kind(t.Ref.ContentKind)
	switch {
	case err != nil || t.Ref.TenantID != k.NS():
		return media.UploadGrant{}, nil
	case isSystem(actor) && (k.Versions || k.Of() != nil):
		return media.UploadGrant{Allowed: true, Exempt: true}, nil
	case k.Of() != nil:
		return media.UploadGrant{}, nil
	case k.Versions:
		return s.canEdit(ctx, actor, k, t)
	case k.Name == s.reg.Config().Avatars.Kind:
		return media.UploadGrant{Allowed: actor.Person() && actor.ID == t.Ref.ContentID, Owner: actor.ID}, nil
	case s.o.Others != nil:
		return s.o.Others.CanUpload(ctx, actor, t)
	}
	return media.UploadGrant{}, nil
}

func (s *Service) canEdit(ctx context.Context, actor access.Actor, k *media.Kind, t media.UploadTarget) (media.UploadGrant, error) {
	if actor.Anonymous || actor.ID == "" {
		return media.UploadGrant{}, nil
	}
	found, err := s.existing(ctx, []contentref.ContentRef{t.Ref})
	if err != nil {
		return media.UploadGrant{}, err
	}
	target, ok := found[t.Ref.Key()]
	if !ok {
		return media.UploadGrant{}, nil
	}
	sights, err := s.o.Visibility.See(ctx, actor, []Target{target})
	if err != nil || !sights[target].Editor {
		return media.UploadGrant{}, err
	}
	if v := k.VersionKind(); target.Version != "" && v.Publish != nil {
		if p := v.Publish.Previews; p != "" && t.Path != "" && strings.HasPrefix(t.Path, previewDir(v)) {
			return media.UploadGrant{}, nil
		}
		m, _, err := s.m.Manifests.Get(ctx, t.Ref)
		if err != nil && !errors.Is(err, media.ErrNotFound) {
			return media.UploadGrant{}, err
		}
		if m != nil && m.Meta[forkMeta] != nil {
			return media.UploadGrant{}, &media.UploadError{Code: media.CodeConflict, Message: "the version is being forked; edit it once the fork finishes"}
		}
	}
	owner := ""
	if actor.Person() {
		owner = actor.ID
	}
	return media.UploadGrant{Allowed: true, Owner: owner, Exempt: !actor.Person()}, nil
}

// itemReady records a settled version's facts and moves its media on: an
// archive import, else publication.
func (s *Service) itemReady(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, r media.Readiness) error {
	k, err := s.reg.Kind(ref.ContentKind)
	if err != nil || !k.Versions || ref.Version() == "" {
		return nil
	}
	v := Target{Content: ref.ContentID, Version: ref.Version()}
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if errors.Is(err, media.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if err := s.recordFacts(ctx, tx, k.VersionKind(), v, m); err != nil {
		return err
	}
	p := k.VersionKind().Publish
	switch {
	case p == nil:
		return nil
	case p.Import != "" && len(imports(k.VersionKind(), m)) > 0:
		return s.enqueueTx(ctx, tx, importArgs{Content: v.Content, Version: v.Version})
	}
	return s.enqueueTx(ctx, tx, publishArgs{Content: v.Content, Version: v.Version})
}

// itemCommitted records a changed version's facts, withdraws its
// publications at once when an edit removed or replaced what they show, and
// queues its publication.
func (s *Service) itemCommitted(ctx context.Context, ref contentref.ContentRef) error {
	k, err := s.reg.Kind(ref.ContentKind)
	if err != nil || !k.Versions || ref.Version() == "" {
		return nil
	}
	v := Target{Content: ref.ContentID, Version: ref.Version()}
	vk := k.VersionKind()
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if errors.Is(err, media.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	withdraw := false
	err = pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		if err := s.recordFacts(ctx, tx, vk, v, m); err != nil {
			return err
		}
		if vk.Publish == nil {
			return nil
		}
		if withdraw, err = s.withdrawStaleTx(ctx, tx, vk, v, m); err != nil {
			return err
		}
		return s.enqueueTx(ctx, tx, publishArgs{Content: v.Content, Version: v.Version})
	})
	if err != nil || !withdraw {
		return err
	}
	return s.withdrawPreviews(ctx, vk, v)
}

func (s *Service) failed(ctx context.Context, ref contentref.ContentRef, path string, err error) {
	s.o.Logger.WarnContext(ctx, "versions: media upload cannot be processed", "ref", ref.String(), "path", path, "error", err)
}
