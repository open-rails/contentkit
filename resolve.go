package contentkit

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/media"
)

// pollFolder is the media kind of poll images (content.Media's default).
const pollFolder = "poll"

// siteMedia is versions.Others: the media kinds neither the catalog nor
// entities own. Poll images go through the content module. Any other kind of
// the site's namespace holds site images, such as a cover of a record the
// host keeps (Doujins' translation requests): anyone sees their public
// files; root:content:write and root:import:write holders upload them.
type siteMedia struct{ e *Engine }

func (m siteMedia) Resolve(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]resolve.Resolution, error) {
	var polls, site []contentref.ContentRef
	for _, ref := range refs {
		switch {
		case ref.ContentKind == pollFolder && m.e.media.polls:
			polls = append(polls, ref)
		case m.site(ref):
			site = append(site, ref)
		}
	}
	out := map[contentref.ContentKey]resolve.Resolution{}
	if len(polls) > 0 {
		var err error
		if out, err = m.e.content.MediaResolver().Resolve(ctx, polls, actor); err != nil {
			return nil, err
		}
	}
	if len(site) == 0 {
		return out, nil
	}
	editor, err := m.writer(ctx, actor)
	if err != nil {
		return nil, err
	}
	for _, ref := range site {
		out[ref.Key()] = resolve.Resolution{Visible: true, Editor: editor}
	}
	return out, nil
}

func (m siteMedia) CanUpload(ctx context.Context, actor access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	switch {
	case t.Ref.ContentKind == pollFolder && m.e.media.polls:
		return m.e.content.CanUpload(ctx, actor, t)
	case !m.site(t.Ref):
		return media.UploadGrant{}, nil
	}
	ok, err := m.writer(ctx, actor)
	return media.UploadGrant{Allowed: ok}, err
}

// site reports a site image item: one id, no version, in the site's own
// namespace.
func (m siteMedia) site(ref contentref.ContentRef) bool {
	return ref.ContentKind != pollFolder && ref.ContentVersionID == nil && ref.TenantID == m.e.media.reg.Namespace()
}

func (m siteMedia) writer(ctx context.Context, actor access.Actor) (bool, error) {
	for _, perm := range []access.Permission{access.ContentWrite, access.ImportWrite} {
		if ok, err := gate.Can(ctx, m.e.auth, actor, string(perm)); ok || err != nil {
			return ok, err
		}
	}
	return false, nil
}

// groupCovers resolves entity covers through media, which is built after
// entities.
type entityCovers struct{ e *Engine }

func (g entityCovers) Covers(ctx context.Context, covers []entities.CoverOf) ([][]entities.Rendition, error) {
	return g.e.media.versions.EntityCovers().Covers(ctx, covers)
}

// entityFolders binds durable retirement to the configured media owner.
type entityFolders struct{ e *Engine }

func (g entityFolders) RetireTx(ctx context.Context, tx pgx.Tx, refs []entities.Ref) error {
	return g.e.media.versions.EntityFolders().RetireTx(ctx, tx, refs)
}

func (g entityFolders) RetirementStartedTx(ctx context.Context, tx pgx.Tx, r entities.Ref) (bool, error) {
	return g.e.media.versions.EntityFolders().RetirementStartedTx(ctx, tx, r)
}
func (g entityFolders) RetirementComplete(ctx context.Context, r entities.Ref) (bool, error) {
	return g.e.media.versions.EntityFolders().RetirementComplete(ctx, r)
}
func (g entityFolders) PurgeRetired(ctx context.Context, limit int) (int, error) {
	return g.e.media.jobs.PurgeRetired(ctx, limit)
}

// ownedContent deletes and restores user-owned entities' content through the
// catalog, built after entities.
type ownedContent struct{ e *Engine }

func (o ownedContent) DeleteOwned(ctx context.Context, entities []string) error {
	return o.e.catalog.DeleteOwned(ctx, entities)
}

func (o ownedContent) RestoreOwned(ctx context.Context, entities []string) error {
	return o.e.catalog.RestoreOwned(ctx, entities)
}

// interactionTargets resolves what interactions target: content items and
// their versions by the catalog's rule, and entities while active, a merged
// entity as its survivor, owned by their owner.
type interactionTargets struct{ e *Engine }

func (t interactionTargets) Resolve(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]resolve.Resolution, error) {
	cfg := t.e.cfg.Content
	var items []contentref.ContentRef
	var ids []entities.EntityID
	for _, ref := range refs {
		if _, ok := cfg.Kinds[ref.ContentKind]; ok {
			items = append(items, ref)
		} else if _, ok := cfg.EntityKinds[ref.ContentKind]; ok && ref.ContentVersionID == nil && ref.TenantID == t.e.cfg.Tenant {
			ids = append(ids, entities.EntityID(ref.ContentID))
		}
	}
	out := map[contentref.ContentKey]resolve.Resolution{}
	if len(items) > 0 {
		var err error
		if out, err = t.e.catalog.Resolver().Resolve(ctx, items, actor); err != nil {
			return nil, err
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	found, err := t.e.entities.Entities(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := map[entities.EntityID]entities.Entity{}
	var survivors []entities.EntityID
	for _, g := range found {
		byID[g.ID] = g
		if g.State == entities.StateMerged && g.MergedInto != "" {
			survivors = append(survivors, g.MergedInto)
		}
	}
	if len(survivors) > 0 {
		more, err := t.e.entities.Entities(ctx, survivors)
		if err != nil {
			return nil, err
		}
		for _, g := range more {
			byID[g.ID] = g
		}
	}
	for _, ref := range refs {
		g, ok := byID[entities.EntityID(ref.ContentID)]
		if !ok || g.Internal || g.Kind != ref.ContentKind || ref.ContentVersionID != nil {
			continue
		}
		if g.State == entities.StateMerged {
			if g, ok = byID[g.MergedInto]; !ok {
				continue
			}
		}
		if g.State != entities.StateActive || g.Internal {
			continue
		}
		res := resolve.Resolution{Ref: contentref.New(t.e.cfg.Tenant, g.Kind, string(g.ID)), Visible: true, Accessible: true, Owner: g.OwnerUserID}
		if cfg.EntityKinds[g.Kind].AuthScope {
			// Scoped channel threads use group moderation, not personal account blocks.
			res.Owner = ""
		}
		if (cfg.EntityKinds[g.Kind].Owner == "user" || cfg.EntityKinds[g.Kind].AuthScope) && (!cfg.EntityKinds[g.Kind].AuthScope || g.AuthScope != "") {
			res.OwnerEntity = &resolve.OwnerEntity{Kind: g.Kind, ID: string(g.ID), Owner: g.OwnerUserID, Scope: g.AuthScope}
		}
		out[ref.Key()] = res
	}
	return out, nil
}
