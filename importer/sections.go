package importer

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/codes"
	"github.com/open-rails/contentkit/offers"
	"github.com/open-rails/contentkit/signal"
)

// typed adapts a store's batch write to a section's.
func typed[T any](fn func(ctx context.Context, tx pgx.Tx, in []T) error) func(context.Context, pgx.Tx, []any) error {
	return func(ctx context.Context, tx pgx.Tx, in []any) error {
		list := make([]T, len(in))
		for i, v := range in {
			list[i] = v.(T)
		}
		return fn(ctx, tx, list)
	}
}

// sections are the batch's record groups in write order.
func (run *batchRun) sections() []section {
	s, b := run.s, run.b
	out := []section{
		{name: SectionGroups, n: len(b.Groups), prepare: run.group,
			write: typed(func(ctx context.Context, tx pgx.Tx, in []groups.ImportGroup) error {
				return s.o.Groups.ImportGroups(ctx, tx, in, b.ProvisionScopes)
			})},
		{name: SectionContents, n: len(b.Contents), prepare: run.content, write: typed(s.o.Catalog.Import)},
		{name: SectionMemberships, n: len(b.Memberships), prepare: run.memberships,
			write: typed(func(ctx context.Context, tx pgx.Tx, in []groups.ContentGroups) error {
				return s.o.Groups.ImportMemberships(ctx, tx, in)
			})},
		{name: SectionOffers, n: len(b.Offers), prepare: run.offer},
		{name: SectionAliases, n: len(b.Aliases), prepare: run.alias,
			write: typed(func(ctx context.Context, tx pgx.Tx, in []codes.Alias) error {
				return codes.PutAliases(ctx, codes.Pgx(tx), s.qs, s.o.Tenant, in)
			})},
		{name: SectionComments, n: len(b.Comments), prepare: run.comment, write: typed(s.o.Content.ImportComments)},
		{name: SectionReactions, n: len(b.Reactions), prepare: run.reaction, write: typed(s.o.Content.ImportReactions)},
		{name: SectionFavorites, n: len(b.Favorites), prepare: run.favorite, write: typed(s.o.Content.ImportFavorites)},
		{name: SectionPolls, n: len(b.Polls), prepare: run.poll, write: typed(s.o.Content.ImportPolls)},
		{name: SectionVotes, n: len(b.Votes), prepare: run.vote, write: typed(s.o.Content.ImportVotes)},
		{name: SectionAnswers, n: len(b.Answers), prepare: run.answer, write: typed(s.o.Content.ImportAnswers)},
		{name: SectionLegacyReading, n: len(b.LegacyReading), prepare: run.legacyReading,
			write: typed(func(ctx context.Context, _ pgx.Tx, in []discovery.ImportLegacyReading) error {
				return s.o.Signals.ImportLegacyReading(ctx, in)
			})},
		{name: SectionSignals, n: len(b.Signals), prepare: run.signal,
			write: typed(func(ctx context.Context, _ pgx.Tx, in []discovery.ImportSignal) error {
				return s.o.Signals.ImportSignals(ctx, in)
			})},
		{name: SectionDeletes, n: len(b.Deletes), prepare: run.deletion, write: typed(s.deletes),
			first: func(in any) bool { return in.(deletion).entity == EntityGroup }},
	}
	if s.o.Offers == nil {
		out[3].skip = "no billing"
	} else {
		out[3].write = typed(s.o.Offers.PutTx)
	}
	if s.o.Signals == nil || !s.o.Signals.SignalPlane() {
		for i := range out {
			if out[i].name == SectionSignals || out[i].name == SectionLegacyReading {
				out[i].skip = "no signal plane, or it is down"
			}
		}
	}
	return out
}

func (run *batchRun) group(r *resolver, i int) (any, string, error) {
	g := run.b.Groups[i]
	id, err := r.own(EntityGroup, g.ExternalID, g.ID, g.CreatedAt)
	if err != nil {
		return nil, g.ExternalID, err
	}
	in := groups.ImportGroup{ID: groups.GroupID(id), Kind: g.Kind, Slug: g.Slug, Internal: g.Internal, State: groups.State(g.State), Attributes: g.Attributes,
		OwnerUserID: g.OwnerUserID, AuthScope: g.AuthScope, Names: g.Names, Text: g.Text, Code: g.Code, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt}
	if g.Avatar != "" {
		in.Avatar = &groups.Avatar{Image: g.Avatar}
	}
	for _, link := range []struct {
		ref  *Ref
		into *groups.GroupID
	}{{g.Scope, &in.ScopeID}, {g.Parent, &in.ParentID}} {
		if link.ref == nil {
			continue
		}
		id, err := r.ref(EntityGroup, *link.ref)
		if err != nil {
			return nil, g.ExternalID, err
		}
		*link.into = groups.GroupID(id)
	}
	switch {
	case g.CoverImage != "" && g.CoverContent != nil:
		return nil, g.ExternalID, fmt.Errorf("%w: a cover is an image or a content, not both", errInvalid)
	case g.CoverImage != "":
		in.Cover = &groups.Cover{Image: g.CoverImage}
	case g.CoverContent != nil:
		id, err := r.ref(EntityContent, *g.CoverContent)
		if err != nil {
			return nil, g.ExternalID, err
		}
		in.Cover = &groups.Cover{ContentID: id}
	}
	if g.Edges != nil {
		in.Edges = []groups.Edge{}
		for _, e := range g.Edges {
			to, err := r.ref(EntityGroup, e.To)
			if err != nil {
				return nil, g.ExternalID, err
			}
			in.Edges = append(in.Edges, groups.Edge{Relation: e.Relation, To: groups.GroupID(to)})
		}
	}
	return in, g.ExternalID, nil
}

func (run *batchRun) content(r *resolver, i int) (any, string, error) {
	c := run.b.Contents[i]
	id, err := r.own(EntityContent, c.ExternalID, c.ID, c.CreatedAt)
	if err != nil {
		return nil, c.ExternalID, err
	}
	in := catalog.ImportContent{ID: id, Kind: c.Kind, State: c.State, PublishAt: c.PublishAt, PublishedAt: c.PublishedAt, DeletedAt: c.DeletedAt,
		OriginalTitle: c.OriginalTitle, OriginalLanguage: c.OriginalLanguage, Comments: c.Comments, Access: c.Access, Restrictions: c.Restrictions, Attributes: c.Attributes,
		Creators: c.Creators, Text: c.Text, Code: c.Code, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
	if c.OwnerGroup != nil {
		if in.OwnerGroupID, err = r.ref(EntityGroup, *c.OwnerGroup); err != nil {
			return nil, c.ExternalID, err
		}
	}
	for _, v := range c.Versions {
		vid, err := r.own(EntityVersion, v.ExternalID, v.ID, v.CreatedAt)
		if err != nil {
			return nil, c.ExternalID, err
		}
		in.Versions = append(in.Versions, catalog.ImportVersion{ID: vid, Code: v.Code, Languages: v.Languages, Traits: v.Traits, Label: v.Label, State: v.State,
			PublishAt: v.PublishAt, PublishedAt: v.PublishedAt, DeletedAt: v.DeletedAt, Access: v.Access, Attributes: v.Attributes, Ordinal: v.Ordinal,
			Cover: v.Cover, Text: v.Text, Segments: v.Segments, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt})
	}
	return in, c.ExternalID, nil
}

func (run *batchRun) memberships(r *resolver, i int) (any, string, error) {
	set := run.b.Memberships[i]
	id, err := r.ref(EntityContent, set.Content)
	if err != nil {
		return nil, set.Content.ExternalID, err
	}
	out := groups.ContentGroups{ContentID: id, Kinds: set.Kinds, Memberships: []groups.Membership{}}
	for _, m := range set.Memberships {
		gid, err := r.ref(EntityGroup, m.Group)
		if err != nil {
			return nil, set.Content.ExternalID, err
		}
		gm := groups.Membership{GroupID: groups.GroupID(gid), ContentID: id, Role: m.Role, Position: m.Position, ActiveFrom: m.ActiveFrom,
			ActiveUntil: m.ActiveUntil, State: groups.MembershipState(m.State)}
		if m.Version != nil {
			if gm.VersionID, err = r.ref(EntityVersion, *m.Version); err != nil {
				return nil, set.Content.ExternalID, err
			}
		}
		out.Memberships = append(out.Memberships, gm)
	}
	return out, set.Content.ExternalID, nil
}

func (run *batchRun) offer(r *resolver, i int) (any, string, error) {
	o := run.b.Offers[i]
	t, err := r.target(o.Target, allowContent|allowVersion|allowGroup)
	if err != nil {
		return nil, "", err
	}
	var target offers.Target
	switch {
	case t.group != "":
		target = offers.Target{Kind: offers.GroupTarget, ID: t.group}
	case t.version != "":
		target = offers.Target{Kind: offers.VersionTarget, ID: t.version, Content: t.content}
	default:
		target = offers.Target{Kind: offers.ContentTarget, ID: t.content}
	}
	return offers.Put{Target: target, Terms: o.Terms}, "", nil
}

func (run *batchRun) alias(r *resolver, i int) (any, string, error) {
	a := run.b.Aliases[i]
	t, err := r.target(a.Target, allowContent|allowGroup)
	if err != nil {
		return nil, a.Key, err
	}
	return codes.Alias{Source: run.b.Source, Kind: a.Kind, Key: a.Key, Locator: a.Locator, Target: codes.Key{Kind: t.ref.ContentKind, ID: t.ref.ContentID}}, a.Key, nil
}

func (run *batchRun) comment(r *resolver, i int) (any, string, error) {
	c := run.b.Comments[i]
	id, err := r.own(EntityComment, c.ExternalID, c.ID, c.CreatedAt)
	if err != nil {
		return nil, c.ExternalID, err
	}
	t, err := r.target(c.Target, allowContent|allowVersion)
	if err != nil {
		return nil, c.ExternalID, err
	}
	in := content.ImportComment{ID: id, Ref: t.ref, Language: c.Language, UserID: c.UserID, AnonName: c.AnonName, Body: c.Body, DeletedAt: c.DeletedAt,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
	if c.ReplyTo != nil {
		if in.ReplyToID, err = r.ref(EntityComment, *c.ReplyTo); err != nil {
			return nil, c.ExternalID, err
		}
	}
	return in, c.ExternalID, nil
}

func (run *batchRun) reaction(r *resolver, i int) (any, string, error) {
	x := run.b.Reactions[i]
	t, err := r.target(x.Target, allowContent|allowVersion|allowGroup|allowComment)
	if err != nil {
		return nil, "", err
	}
	return content.ImportReaction{Ref: t.ref, UserID: x.UserID, IP: x.IP, Value: x.Value, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt}, "", nil
}

func (run *batchRun) favorite(r *resolver, i int) (any, string, error) {
	f := run.b.Favorites[i]
	t, err := r.target(f.Target, allowContent|allowVersion)
	if err != nil {
		return nil, "", err
	}
	return content.ImportFavorite{Ref: t.ref, UserID: f.UserID, CreatedAt: f.CreatedAt}, "", nil
}

func (run *batchRun) poll(r *resolver, i int) (any, string, error) {
	p := run.b.Polls[i]
	id, err := r.own(EntityPoll, p.ExternalID, p.ID, p.CreatedAt)
	if err != nil {
		return nil, p.ExternalID, err
	}
	in := content.ImportPoll{ID: id, Kind: p.Kind, Question: p.Question, Language: p.Language, Active: p.Active == nil || *p.Active, LiveAt: p.LiveAt,
		ClosesAt: p.ClosesAt, DeletedAt: p.DeletedAt, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
	for _, o := range p.Options {
		oid, err := r.own(EntityOption, o.ExternalID, o.ID, p.CreatedAt)
		if err != nil {
			return nil, p.ExternalID, err
		}
		in.Options = append(in.Options, content.ImportPollOption{ID: oid, Label: o.Label, Position: o.Position})
	}
	return in, p.ExternalID, nil
}

func (run *batchRun) vote(r *resolver, i int) (any, string, error) {
	v := run.b.Votes[i]
	poll, err := r.ref(EntityPoll, v.Poll)
	if err != nil {
		return nil, "", err
	}
	option, err := r.ref(EntityOption, v.Option)
	if err != nil {
		return nil, "", err
	}
	return content.ImportVote{PollID: poll, OptionID: option, UserID: v.UserID, IP: v.IP, CreatedAt: v.CreatedAt}, "", nil
}

func (run *batchRun) answer(r *resolver, i int) (any, string, error) {
	a := run.b.Answers[i]
	poll, err := r.ref(EntityPoll, a.Poll)
	if err != nil {
		return nil, "", err
	}
	return content.ImportAnswer{PollID: poll, UserID: a.UserID, Text: a.Text, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}, "", nil
}

func (run *batchRun) signal(r *resolver, i int) (any, string, error) {
	x := run.b.Signals[i]
	t, err := r.target(x.Target, allowContent|allowVersion)
	if err != nil {
		return nil, x.EventID, err
	}
	if (x.UserID == "") == (x.AnonKey == "") {
		return nil, x.EventID, fmt.Errorf("%w: a signal names a user_id or an anon_key", errInvalid)
	}
	return discovery.ImportSignal{ContentID: t.content, VersionID: t.version, Subject: signal.Subject{UserID: x.UserID, AnonKey: x.AnonKey}, Type: x.Type,
		EventID: x.EventID, Revision: x.Revision, OccurredAt: x.OccurredAt}, x.EventID, nil
}

// deletion is one record to delete, resolved to its id.
type deletion struct {
	entity, id string
}

func (run *batchRun) deletion(r *resolver, i int) (any, string, error) {
	d := run.b.Deletes[i]
	switch d.Entity {
	case EntityContent, EntityVersion, EntityGroup, EntityComment, EntityPoll:
	default:
		return nil, d.ExternalID, fmt.Errorf("%w: entity %q is content, version, group, comment or poll", errInvalid, d.Entity)
	}
	if d.ExternalID == "" {
		return nil, "", fmt.Errorf("%w: a delete names an external_id", errInvalid)
	}
	id, err := r.ref(d.Entity, Ref{ExternalID: d.ExternalID})
	// Only an absent import mapping makes this delete a no-op. Store errors
	// (including other not-found failures) still fail the record normally.
	if errors.Is(err, errNotFound) {
		return deletion{entity: d.Entity}, d.ExternalID, nil
	}
	return deletion{entity: d.Entity, id: id}, d.ExternalID, err
}

// deletes deletes what the records name: groups, then content and versions,
// then comments and polls. Groups go first because deleting one takes the
// group hierarchy lock, which every hierarchy writer takes before content
// rows; deleting content first would hold content rows while waiting on it.
// The section's first puts group records ahead, so a one-by-one retry keeps
// that order.
func (s *Service) deletes(ctx context.Context, tx pgx.Tx, in []deletion) error {
	by := map[string][]string{}
	for _, d := range in {
		if d.id == "" {
			continue
		}
		by[d.entity] = append(by[d.entity], d.id)
	}
	gs := make([]groups.GroupID, len(by[EntityGroup]))
	for i, id := range by[EntityGroup] {
		gs[i] = groups.GroupID(id)
	}
	if err := s.o.Groups.ImportDeletes(ctx, tx, gs); err != nil {
		return err
	}
	if err := s.o.Catalog.ImportDeletes(ctx, tx, by[EntityContent], by[EntityVersion]); err != nil {
		return err
	}
	return s.o.Content.ImportDeletes(ctx, tx, by[EntityComment], by[EntityPoll])
}

func (run *batchRun) legacyReading(r *resolver, i int) (any, string, error) {
	row := run.b.LegacyReading[i]
	target, err := r.target(row.Target, allowContent|allowVersion)
	if err != nil {
		return nil, row.UserID, err
	}
	subject := signal.Subject{UserID: row.UserID, AnonKey: row.AnonKey}
	if err := subject.Validate(); err != nil {
		return nil, row.UserID, fmt.Errorf("%w: %v", errInvalid, err)
	}
	key := subject.Kind() + ":" + subject.Key() + "/" + target.content + "/" + target.version
	return discovery.ImportLegacyReading{ContentID: target.content, VersionID: target.version, Subject: subject, FirstReadAt: row.FirstReadAt,
		LastReadAt: row.LastReadAt, Progress: row.Progress, ProgressMax: row.ProgressMax, Completed: row.Completed, Resume: row.Resume, Revision: row.Revision}, key, nil
}
