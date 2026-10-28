package versions

import (
	"context"
	"errors"

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
	var owned, grouped, others []contentref.ContentRef
	for _, ref := range refs {
		k, err := s.reg.Kind(ref.ContentKind)
		switch {
		case err != nil || ref.TenantID != k.NS() || k.Versions && ref.Version() == "":
		case k.Versions:
			owned = append(owned, ref)
		case s.isEntityKind(k.Name):
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
	if err := s.resolveEntities(ctx, grouped, actor, out); err != nil {
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
	kinds, contents, vers := make([]string, len(refs)), make([]string, len(refs)), make([]string, len(refs))
	for i, ref := range refs {
		kinds[i], contents[i], vers[i] = ref.ContentKind, ref.ContentID, ref.Version()
	}
	rows, err := s.o.Pool.Query(ctx, `SELECT t.n FROM unnest($2::text[], $3::text[], $4::text[]) WITH ORDINALITY AS t(kind, content, version, n)
JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = t.content AND c.kind = t.kind
WHERE t.version = '' OR EXISTS (SELECT 1 FROM `+s.t.versions+` v WHERE v.tenant_id = $1 AND v.id = t.version AND v.content_id = t.content)`,
		s.o.Tenant, kinds, contents, vers)
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
		rr := resolve.Resolution{Visible: true, Editor: r.Editor, ReadRestricted: r.ReadRestricted, Owner: r.Owner, Accessible: r.Accessible}

		out[ref.Key()] = rr
	}
	return nil
}

type uploads struct{ s *Service }

// CanUpload lets editors of a content (staff, its authenticated author, its owner
// group's editors) write its items, a group's editors its item, a person
// their own avatar.
func (u uploads) CanUpload(ctx context.Context, actor access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	s := u.s
	k, err := s.reg.Kind(t.Ref.ContentKind)
	switch {
	case err != nil || t.Ref.TenantID != k.NS():
		return media.UploadGrant{}, nil
	case isSystem(actor) && k.Versions:
		return media.UploadGrant{Allowed: true, Exempt: true, ArchiveTo: k.ArchiveDestination(t.Path)}, nil
	case k.Versions:
		g, err := s.canEdit(ctx, actor, k, t)
		g.ArchiveTo = k.ArchiveDestination(t.Path)
		return g, err
	case s.isEntityKind(k.Name):
		g, err := s.canEditEntity(ctx, actor, t)
		g.ArchiveTo = k.ArchiveDestination(t.Path)
		return g, err
	case k.Name == s.reg.Config().Avatars.Kind:
		writer, err := s.avatarEditor(ctx, actor, t.Ref.ContentID)
		self := actor.Member() && actor.AccountID() == t.Ref.ContentID
		return media.UploadGrant{Allowed: writer, Owner: t.Ref.ContentID, Exempt: writer && !self, ArchiveTo: k.ArchiveDestination(t.Path)}, err
	case s.o.Others != nil:
		return s.o.Others.CanUpload(ctx, actor, t)
	}
	return media.UploadGrant{}, nil
}

func (s *Service) canEdit(ctx context.Context, actor access.Actor, k *media.RegisteredKind, t media.UploadTarget) (media.UploadGrant, error) {
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
	owner := ""
	if actor.Member() {
		owner = actor.AccountID()
	}
	return media.UploadGrant{Allowed: true, Owner: owner, Exempt: !actor.Member()}, nil
}

// itemReady records a settled version's facts and moves its media on: an
// ready-file metadata publication.
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
	m, _, err := s.m.Manifests.GetTx(ctx, tx, ref)
	if errors.Is(err, media.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if err := s.recordFacts(ctx, tx, k, v, m); err != nil {
		return err
	}
	return s.wakePublicationTx(ctx, tx, v)

}

// itemProgressed records partial media facts and wakes reader snapshots before
// unrelated processing finishes. The wakeup commits with those facts.
func (s *Service) itemProgressed(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef) error {
	k, err := s.reg.Kind(ref.ContentKind)
	if err != nil || !k.Versions || ref.Version() == "" {
		return nil
	}
	v := Target{Content: ref.ContentID, Version: ref.Version()}
	// As in itemReady: lock v, then read the newest manifest.
	if err := s.lockTarget(ctx, tx, v); err != nil {
		return err
	}
	m, _, err := s.m.Manifests.GetTx(ctx, tx, ref)
	if errors.Is(err, media.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if err := s.recordFacts(ctx, tx, k, v, m); err != nil {
		return err
	}
	return s.wakePublicationTx(ctx, tx, v)
}

// beforeRetire serializes snapshot invalidation with publishers. A publisher
// that already holds the version row finishes first and is then invalidated;
// one that follows reads the newly committed manifest. No media lock is taken.
func (s *Service) beforeRetire(ctx context.Context, ref contentref.ContentRef, current *media.Manifest) error {
	if ctx.Value(releaseMutationContextKey{}) == true || current.PublicationLayoutReady() {
		// The completed delivery owns exact reader allocations until the
		// publisher commits its replacement and retires the old delivery.
		return nil
	}
	k, err := s.reg.Kind(ref.ContentKind)
	if err != nil || !k.Versions || ref.Version() == "" || k.Publish == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		v := Target{Content: ref.ContentID, Version: ref.Version()}
		if err := s.lockTarget(ctx, tx, v); err != nil {
			return err
		}
		_, err := s.withdrawStaleTx(ctx, tx, k, v, current)
		return err
	})
}

// itemCommitted records a changed version's facts, withdraws its
// publications at once when an edit removed or replaced what they show, and
// queues its publication.
func (s *Service) itemCommitted(ctx context.Context, ref contentref.ContentRef) error {
	if ctx.Value(releaseMutationContextKey{}) == true {
		return nil
	}
	k, err := s.reg.Kind(ref.ContentKind)
	if err != nil || !k.Versions || ref.Version() == "" {
		return nil
	}
	v := Target{Content: ref.ContentID, Version: ref.Version()}
	vk := k
	return pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		// As in itemReady: lock v, then read the newest manifest.
		if err := s.lockTarget(ctx, tx, v); err != nil {
			return err
		}
		m, _, err := s.m.Manifests.GetTx(ctx, tx, ref)
		if errors.Is(err, media.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		if err := s.recordFacts(ctx, tx, vk, v, m); err != nil {
			return err
		}
		if !m.PublicationLayoutReady() && vk.Publish != nil {
			if _, err := s.withdrawStaleTx(ctx, tx, vk, v, m); err != nil {
				return err
			}
		}
		return s.SchedulePublicationTx(ctx, tx, []Target{v})
	})
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
