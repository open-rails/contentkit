package versions

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/media"
)

type resolver struct{ s *Service }

// Resolve decides media reads. A version's item shows like the version; its
// private files (a body's images among them) open only for a kind that
// publishes nothing, since a publishing kind's originals reach viewers
// through published items alone.
// A published item shows while its publication is current and its pages
// are released, and opens like its version, or within the free segments.
func (r resolver) Resolve(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]resolve.Resolution, error) {
	s := r.s
	out := map[contentref.ContentKey]resolve.Resolution{}
	var owned, published, grouped, others []contentref.ContentRef
	for _, ref := range refs {
		k, err := s.reg.Kind(ref.ContentKind)
		switch {
		case err != nil || ref.TenantID != k.NS() || k.Versions && ref.Version() == "":
		case k.Versions:
			owned = append(owned, ref)
		case k.Of() != nil:
			published = append(published, ref)
		case s.isGroupKind(k.Name):
			grouped = append(grouped, ref)
		case k.Name == s.reg.Config().Avatars.Kind:
			editor, err := s.avatarEditor(ctx, actor, ref.ContentID)
			if err != nil {
				return nil, err
			}
			out[ref.Key()] = resolve.Resolution{Visible: true, Editor: editor}
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
	if err := s.resolveGroups(ctx, grouped, actor, out); err != nil {
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

func (s *Service) resolveOwned(ctx context.Context, refs []contentref.ContentRef, actor access.Actor, out map[contentref.ContentKey]resolve.Resolution) error {
	found, err := s.existing(ctx, refs)
	if err != nil || len(found) == 0 {
		return err
	}
	var asked []contentref.ContentRef
	for _, ref := range refs {
		if _, ok := found[ref.Key()]; ok {
			asked = append(asked, ref)
		}
	}
	domain := make([]contentref.ContentRef, len(asked))
	for i, ref := range asked {
		domain[i] = ref
		domain[i].TenantID = s.o.Tenant
	}
	res, err := s.o.Resolver.Resolve(ctx, domain, actor)
	if err != nil {
		return err
	}
	for i, ref := range asked {
		r := res[domain[i].Key()]
		if !r.Visible {
			continue
		}
		k, _ := s.reg.Kind(ref.ContentKind)
		out[ref.Key()] = resolve.Resolution{Visible: true, Editor: r.Editor, Owner: r.Owner,
			Accessible: r.Accessible && k.Publish == nil}
	}
	return nil
}

// pubItem is one published item and what it shows.
type pubItem struct {
	id, publication, kind string
	contentKind           string
	version               Target
	current               bool
	start, end            int
	pages                 int
}

func (s *Service) resolvePublished(ctx context.Context, refs []contentref.ContentRef, actor access.Actor, out map[contentref.ContentKey]resolve.Resolution) error {
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
	byKind := map[string][]Target{}
	pages := map[Target]int{}
	for _, it := range items {
		if it.current {
			byKind[it.contentKind] = append(byKind[it.contentKind], it.version)
			pages[it.version] = max(pages[it.version], it.pages)
		}
	}
	ds := map[Target]decision{}
	for kind, vs := range byKind {
		got, err := s.decisions(ctx, actor, kind, vs, pages)
		if err != nil {
			return err
		}
		for t, d := range got {
			ds[t] = d
		}
	}
	for _, ref := range refs {
		it, ok := items[ref.ContentID]
		if !ok || !it.current || ref.ContentKind != it.contentKind+media.PublishedSuffix {
			continue
		}
		d := ds[it.version]
		switch {
		case it.kind == kindPlayback && d.whole():
			out[ref.Key()] = resolve.Resolution{Visible: true, Accessible: true, Editor: d.res.Editor}
		case it.kind == kindPages && d.shows(it.end):
			out[ref.Key()] = resolve.Resolution{Visible: true, Accessible: d.opens(it.end), Editor: d.res.Editor}
		}
	}
	return nil
}

// pubItems loads published items by id: segment items, and playback items
// (a playback publication's id).
func (s *Service) pubItems(ctx context.Context, ids []string) (map[string]pubItem, error) {
	rows, err := s.o.Pool.Query(ctx, `SELECT i.id, p.id, p.kind, c.kind, v.content_id, p.version_id, p.state = 'current', i.start_index, i.end_index,
	jsonb_array_length(p.source->'pages')
FROM `+s.t.pubItems+` i JOIN `+s.t.publications+` p ON p.tenant_id = i.tenant_id AND p.id = i.publication_id
JOIN `+s.t.versions+` v ON v.tenant_id = p.tenant_id AND v.id = p.version_id
JOIN `+s.t.items+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id
WHERE i.tenant_id = $1 AND i.id = ANY($2)
UNION ALL
SELECT p.id, p.id, p.kind, c.kind, v.content_id, p.version_id, p.state = 'current', 0, 0, 0
FROM `+s.t.publications+` p JOIN `+s.t.versions+` v ON v.tenant_id = p.tenant_id AND v.id = p.version_id
JOIN `+s.t.items+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id
WHERE p.tenant_id = $1 AND p.id = ANY($2) AND p.kind = 'playback'`, s.o.Tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]pubItem{}
	for rows.Next() {
		var it pubItem
		if err := rows.Scan(&it.id, &it.publication, &it.kind, &it.contentKind, &it.version.Content, &it.version.Version, &it.current,
			&it.start, &it.end, &it.pages); err != nil {
			return nil, err
		}
		out[it.id] = it
	}
	return out, rows.Err()
}

type uploads struct{ s *Service }

// CanUpload lets editors of a content (staff, its creators, its owner
// group's editors) write its items, a group's editors its item, a person
// their own avatar, and this package alone published items and preview
// copies. A version being forked takes no other writes.
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
	case s.isGroupKind(k.Name):
		return s.canEditGroup(ctx, actor, t)
	case k.Name == s.reg.Config().Avatars.Kind:
		writer, err := s.avatarEditor(ctx, actor, t.Ref.ContentID)
		self := actor.Member() && actor.AccountID() == t.Ref.ContentID
		return media.UploadGrant{Allowed: writer, Owner: t.Ref.ContentID, Exempt: writer && !self}, err
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
	res, err := s.decide(ctx, actor, k.Name, []Target{target})
	if err != nil || !res[target].Editor {
		return media.UploadGrant{}, err
	}
	if target.Version != "" && k.Publish != nil {
		if p := k.Publish.Previews; p != "" && t.Path != "" && strings.HasPrefix(t.Path, previewDir(k)) {
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
	if actor.Member() {
		owner = actor.AccountID()
	}
	return media.UploadGrant{Allowed: true, Owner: owner, Exempt: !actor.Member()}, nil
}

// itemReady records a settled version's facts and moves its media on: an
// archive import, else publication.
func (s *Service) itemReady(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, r media.Readiness) error {
	k, err := s.reg.Kind(ref.ContentKind)
	if err != nil || !k.Versions || ref.Version() == "" {
		return nil
	}
	v := Target{Content: ref.ContentID, Version: ref.Version()}
	// Read the manifest only once v is locked: a delayed callback then
	// records the newest snapshot, not the one current when it was called.
	if err := s.lockTarget(ctx, tx, v); err != nil {
		return err
	}
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if errors.Is(err, media.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if err := s.recordFacts(ctx, tx, k, v, m); err != nil {
		return err
	}
	p := k.Publish
	switch {
	case p == nil:
		return nil
	case p.Import != "" && len(imports(k, m)) > 0:
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
	vk := k
	withdraw := false
	err = pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		// As in itemReady: lock v, then read the newest manifest.
		if err := s.lockTarget(ctx, tx, v); err != nil {
			return err
		}
		m, _, err := s.m.Manifests.Get(ctx, ref)
		if errors.Is(err, media.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
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

// avatarsWriter reports a holder of root:avatars:write: staff, or a site
// forwarding its people's avatars.
func (s *Service) avatarsWriter(ctx context.Context, actor access.Actor) (bool, error) {
	return gate.Can(ctx, s.o.Auth, actor, string(access.AvatarsWrite))
}

func (s *Service) avatarEditor(ctx context.Context, actor access.Actor, account string) (bool, error) {
	if s.o.AccountExists != nil {
		exists, err := s.o.AccountExists(ctx, account)
		if err != nil || !exists {
			return false, err
		}
	}
	if actor.Member() && actor.AccountID() == account {
		return true, nil
	}
	return s.avatarsWriter(ctx, actor)
}
