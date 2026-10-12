package contentkit

import (
	"context"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/media"
)

// pollFolder is the media kind of poll images (content.Media's default).
const pollFolder = "poll"

// pollMedia is versions.Others: poll images, the one media kind neither the
// catalog nor groups own, through the content module.
type pollMedia struct{ e *Engine }

func (p pollMedia) Resolve(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]resolve.Resolution, error) {
	var polls []contentref.ContentRef
	for _, ref := range refs {
		if ref.ContentKind == pollFolder && p.e.media.polls {
			polls = append(polls, ref)
		}
	}
	if len(polls) == 0 {
		return map[contentref.ContentKey]resolve.Resolution{}, nil
	}
	return p.e.content.MediaResolver().Resolve(ctx, polls, actor)
}

func (p pollMedia) CanUpload(ctx context.Context, actor access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	if t.Ref.ContentKind != pollFolder || !p.e.media.polls {
		return media.UploadGrant{}, nil
	}
	return p.e.content.CanUpload(ctx, actor, t)
}

// groupCovers resolves group covers through media, which is built after
// groups.
type groupCovers struct{ e *Engine }

func (g groupCovers) Covers(ctx context.Context, covers []groups.CoverOf) ([][]groups.Rendition, error) {
	return g.e.media.versions.GroupCovers().Covers(ctx, covers)
}

// interactionTargets resolves what interactions target: content items and
// their versions by the catalog's rule, and groups while active, a merged
// group as its survivor, owned by their owner.
type interactionTargets struct{ e *Engine }

func (t interactionTargets) Resolve(ctx context.Context, refs []contentref.ContentRef, actor access.Actor) (map[contentref.ContentKey]resolve.Resolution, error) {
	cfg := t.e.cfg.Content
	var items []contentref.ContentRef
	var ids []groups.GroupID
	for _, ref := range refs {
		if _, ok := cfg.Kinds[ref.ContentKind]; ok {
			items = append(items, ref)
		} else if _, ok := cfg.GroupKinds[ref.ContentKind]; ok && ref.ContentVersionID == nil && ref.TenantID == t.e.cfg.Tenant {
			ids = append(ids, groups.GroupID(ref.ContentID))
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
	found, err := t.e.groups.Groups(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := map[groups.GroupID]groups.Group{}
	var survivors []groups.GroupID
	for _, g := range found {
		byID[g.ID] = g
		if g.State == groups.StateMerged && g.MergedInto != "" {
			survivors = append(survivors, g.MergedInto)
		}
	}
	if len(survivors) > 0 {
		more, err := t.e.groups.Groups(ctx, survivors)
		if err != nil {
			return nil, err
		}
		for _, g := range more {
			byID[g.ID] = g
		}
	}
	for _, ref := range refs {
		g, ok := byID[groups.GroupID(ref.ContentID)]
		if !ok || g.Kind != ref.ContentKind || ref.ContentVersionID != nil {
			continue
		}
		if g.State == groups.StateMerged {
			if g, ok = byID[g.MergedInto]; !ok {
				continue
			}
		}
		if g.State != groups.StateActive {
			continue
		}
		out[ref.Key()] = resolve.Resolution{Ref: contentref.New(t.e.cfg.Tenant, g.Kind, string(g.ID)), Visible: true, Accessible: true, Owner: g.OwnerUserID}
	}
	return out, nil
}
