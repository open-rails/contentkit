package importstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/entities"
	importer "github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/internal/codes"
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

// sections are the batch's record entities in write order.
func (run *batchRun) sections() []section {
	s, b := run.s, run.b
	out := []section{
		{name: importer.SectionEntities, n: len(b.Entities), prepare: run.entity,
			write: typed(func(ctx context.Context, tx pgx.Tx, in []entities.ImportEntity) error {
				return s.o.Entities.ImportEntities(ctx, tx, in, b.ProvisionScopes)
			})},
		{name: importer.SectionChannels, n: len(b.Channels), prepare: run.channel,
			write: typed(func(ctx context.Context, tx pgx.Tx, in []channels.Import) error {
				return s.o.Channels.ImportTx(ctx, tx, in, b.ProvisionScopes)
			})},
		{name: importer.SectionContents, n: len(b.Contents), prepare: run.content, write: typed(s.o.Catalog.Import)},
		{name: importer.SectionMemberships, n: len(b.Memberships), prepare: run.memberships,
			write: typed(func(ctx context.Context, tx pgx.Tx, in []entities.ContentEntities) error {
				return s.o.Entities.ImportMemberships(ctx, tx, in)
			})},
		{name: importer.SectionAliases, n: len(b.Aliases), prepare: run.alias,
			write: typed(func(ctx context.Context, tx pgx.Tx, in []codes.Alias) error {
				return codes.PutAliases(ctx, codes.Pgx(tx), s.qs, s.o.Tenant, in)
			})},
		{name: importer.SectionComments, n: len(b.Comments), prepare: run.comment, write: typed(s.o.Content.ImportComments)},
		{name: importer.SectionReactions, n: len(b.Reactions), prepare: run.reaction, write: typed(s.o.Content.ImportReactions)},
		{name: importer.SectionFavorites, n: len(b.Favorites), prepare: run.favorite, write: typed(s.o.Content.ImportFavorites)},
		{name: importer.SectionPolls, n: len(b.Polls), prepare: run.poll, write: typed(s.o.Content.ImportPolls)},
		{name: importer.SectionVotes, n: len(b.Votes), prepare: run.vote, write: typed(s.o.Content.ImportVotes)},
		{name: importer.SectionLegacyReading, n: len(b.LegacyReading), prepare: run.legacyReading,
			write: typed(func(ctx context.Context, _ pgx.Tx, in []discovery.ImportLegacyReading) error {
				return s.o.Signals.ImportLegacyReading(ctx, in)
			})},
		{name: importer.SectionSignals, n: len(b.Signals), prepare: run.signal,
			write: typed(func(ctx context.Context, _ pgx.Tx, in []discovery.ImportSignal) error {
				return s.o.Signals.ImportSignals(ctx, in)
			})},
		{name: importer.SectionDeletes, n: len(b.Deletes), prepare: run.deletion, write: typed(s.deletes),
			first: func(in any) bool {
				e := in.(deletion).entity
				return e == importer.RecordEntity || e == importer.RecordChannel
			}},
	}
	if s.o.Signals == nil || !s.o.Signals.SignalPlane() {
		for i := range out {
			if out[i].name == importer.SectionSignals || out[i].name == importer.SectionLegacyReading {
				out[i].skip = "no signal plane, or it is down"
			}
		}
	}
	return out
}

func (run *batchRun) entity(r *resolver, i int) (any, string, error) {
	g := run.b.Entities[i]
	id, err := r.own(importer.RecordEntity, g.ExternalID, g.ID, g.CreatedAt)
	if err != nil {
		return nil, g.ExternalID, err
	}
	in := entities.ImportEntity{ID: entities.EntityID(id), Kind: g.Kind, Slug: g.Slug, Internal: g.Internal, State: entities.State(g.State), Attributes: g.Attributes,
		Names: g.Names, Text: g.Text, Code: g.Code, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt}
	if g.Avatar != "" {
		in.Avatar = &entities.Avatar{Image: g.Avatar}
	}
	for _, link := range []struct {
		ref  *importer.Ref
		into *entities.EntityID
	}{{g.Scope, &in.ScopeID}, {g.Parent, &in.ParentID}} {
		if link.ref == nil {
			continue
		}
		id, err := r.ref(importer.RecordEntity, *link.ref)
		if err != nil {
			return nil, g.ExternalID, err
		}
		*link.into = entities.EntityID(id)
	}
	switch {
	case g.CoverImage != "" && g.CoverContent != nil:
		return nil, g.ExternalID, fmt.Errorf("%w: a cover is an image or a content, not both", errInvalid)
	case g.CoverImage != "":
		in.Cover = &entities.Cover{Image: g.CoverImage}
	case g.CoverContent != nil:
		id, err := r.ref(importer.RecordContent, *g.CoverContent)
		if err != nil {
			return nil, g.ExternalID, err
		}
		in.Cover = &entities.Cover{ContentID: id}
	}
	if g.Edges != nil {
		in.Edges = []entities.Edge{}
		for _, e := range g.Edges {
			to, err := r.ref(importer.RecordEntity, e.To)
			if err != nil {
				return nil, g.ExternalID, err
			}
			in.Edges = append(in.Edges, entities.Edge{Relation: e.Relation, To: entities.EntityID(to)})
		}
	}
	return in, g.ExternalID, nil
}

func (run *batchRun) channel(r *resolver, i int) (any, string, error) {
	c := run.b.Channels[i]
	id, err := r.own(importer.RecordChannel, c.ExternalID, c.ID, c.CreatedAt)
	if err != nil {
		return nil, c.ExternalID, err
	}
	in := channels.Import{ID: id, Slug: c.Slug, State: entities.State(c.State),
		OwnerUserID: c.OwnerUserID, AuthScope: c.AuthScope, Names: c.Names, Text: c.Text,
		Code: c.Code, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
	if c.Avatar != "" {
		in.Avatar = &entities.Avatar{Image: c.Avatar}
	}
	switch {
	case c.CoverImage != "" && c.CoverContent != nil:
		return nil, c.ExternalID, fmt.Errorf("%w: a cover is an image or content, not both", errInvalid)
	case c.CoverImage != "":
		in.Cover = &entities.Cover{Image: c.CoverImage}
	case c.CoverContent != nil:
		id, err := r.ref(importer.RecordContent, *c.CoverContent)
		if err != nil {
			return nil, c.ExternalID, err
		}
		in.Cover = &entities.Cover{ContentID: id}
	}
	return in, c.ExternalID, nil
}

func (run *batchRun) content(r *resolver, i int) (any, string, error) {
	c := run.b.Contents[i]
	id, err := r.own(importer.RecordContent, c.ExternalID, c.ID, c.CreatedAt)
	if err != nil {
		return nil, c.ExternalID, err
	}
	in := catalog.ImportContent{ID: id, Kind: c.Kind, State: c.State, PublishAt: c.PublishAt, PublishedAt: c.PublishedAt, DeletedAt: c.DeletedAt,
		Comments: c.Comments, Access: c.Access, Restrictions: c.Restrictions, Attributes: c.Attributes,
		Creators: c.Creators, Code: c.Code, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
	if c.Channel != nil {
		if in.ChannelID, err = r.ref(importer.RecordChannel, *c.Channel); err != nil {
			return nil, c.ExternalID, err
		}
	}
	for _, v := range c.Versions {
		vid, err := r.own(importer.RecordVersion, v.ExternalID, v.ID, v.CreatedAt)
		if err != nil {
			return nil, c.ExternalID, err
		}
		in.Versions = append(in.Versions, catalog.ImportVersion{ID: vid, Code: v.Code, Languages: v.Languages, Traits: v.Traits, Label: v.Label, State: v.State,
			PublishAt: v.PublishAt, PublishedAt: v.PublishedAt, DeletedAt: v.DeletedAt, Access: v.Access, Attributes: v.Attributes, Ordinal: v.Ordinal,
			Cover: v.Cover, Text: v.Text, OriginalTitle: v.OriginalTitle, OriginalLanguage: v.OriginalLanguage, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt})
	}
	return in, c.ExternalID, nil
}

func (run *batchRun) memberships(r *resolver, i int) (any, string, error) {
	set := run.b.Memberships[i]
	id, err := r.ref(importer.RecordContent, set.Content)
	if err != nil {
		return nil, set.Content.ExternalID, err
	}
	out := entities.ContentEntities{ContentID: id, Kinds: set.Kinds, Memberships: []entities.Membership{}}
	for _, m := range set.Memberships {
		gid, err := r.ref(importer.RecordEntity, m.Entity)
		if err != nil {
			return nil, set.Content.ExternalID, err
		}
		gm := entities.Membership{EntityID: entities.EntityID(gid), ContentID: id, Role: m.Role, Position: m.Position, State: entities.MembershipState(m.State)}
		if m.Version != nil {
			if gm.VersionID, err = r.ref(importer.RecordVersion, *m.Version); err != nil {
				return nil, set.Content.ExternalID, err
			}
		}
		out.Memberships = append(out.Memberships, gm)
	}
	return out, set.Content.ExternalID, nil
}

func (run *batchRun) alias(r *resolver, i int) (any, string, error) {
	a := run.b.Aliases[i]
	t, err := r.target(a.Target, allowContent|allowEntity|allowChannel)
	if err != nil {
		return nil, a.Key, err
	}
	return codes.Alias{Source: run.b.Source, Kind: a.Kind, Key: a.Key, Locator: a.Locator, Target: codes.Key{Kind: t.ref.ContentKind, ID: t.ref.ContentID}}, a.Key, nil
}

func (run *batchRun) comment(r *resolver, i int) (any, string, error) {
	c := run.b.Comments[i]
	id, err := r.own(importer.RecordComment, c.ExternalID, c.ID, c.CreatedAt)
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
		if in.ReplyToID, err = r.ref(importer.RecordComment, *c.ReplyTo); err != nil {
			return nil, c.ExternalID, err
		}
	}
	return in, c.ExternalID, nil
}

func (run *batchRun) reaction(r *resolver, i int) (any, string, error) {
	x := run.b.Reactions[i]
	t, err := r.target(x.Target, allowContent|allowVersion|allowEntity|allowChannel|allowComment)
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
	id, err := r.own(importer.RecordPoll, p.ExternalID, p.ID, p.CreatedAt)
	if err != nil {
		return nil, p.ExternalID, err
	}
	in := content.ImportPoll{ID: id, Kind: p.Kind, Question: p.Question, Language: p.Language, Active: p.Active == nil || *p.Active, LiveAt: p.LiveAt,
		ClosesAt: p.ClosesAt, DeletedAt: p.DeletedAt, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
	for _, o := range p.Options {
		oid, err := r.own(importer.RecordOption, o.ExternalID, o.ID, p.CreatedAt)
		if err != nil {
			return nil, p.ExternalID, err
		}
		in.Options = append(in.Options, content.ImportPollOption{ID: oid, Label: o.Label, Position: o.Position})
	}
	return in, p.ExternalID, nil
}

func (run *batchRun) vote(r *resolver, i int) (any, string, error) {
	v := run.b.Votes[i]
	poll, err := r.ref(importer.RecordPoll, v.Poll)
	if err != nil {
		return nil, "", err
	}
	option, err := r.ref(importer.RecordOption, v.Option)
	if err != nil {
		return nil, "", err
	}
	return content.ImportVote{PollID: poll, OptionID: option, UserID: v.UserID, IP: v.IP, CreatedAt: v.CreatedAt}, "", nil
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
	case importer.RecordContent, importer.RecordVersion, importer.RecordEntity, importer.RecordChannel, importer.RecordComment, importer.RecordPoll:
	default:
		return nil, d.ExternalID, fmt.Errorf("%w: entity %q is content, version, group, channel, comment or poll", errInvalid, d.Entity)
	}
	if d.Entity == importer.RecordChannel && run.s.o.Channels == nil {
		return nil, d.ExternalID, fmt.Errorf("%w: channels are not configured", errInvalid)
	}
	if d.ExternalID == "" {
		return nil, "", fmt.Errorf("%w: a delete names an external_id", errInvalid)
	}
	id, err := r.ref(d.Entity, importer.Ref{ExternalID: d.ExternalID})
	// Only an absent import mapping makes this delete a no-op. Store errors
	// (including other not-found failures) still fail the record normally.
	if errors.Is(err, errNotFound) {
		return deletion{entity: d.Entity}, d.ExternalID, nil
	}
	return deletion{entity: d.Entity, id: id}, d.ExternalID, err
}

// deletes deletes what the records name: entities, then content and versions,
// then comments and polls. Entities go first because deleting one takes the
// entity hierarchy lock, which every hierarchy writer takes before content
// rows; deleting content first would hold content rows while waiting on it.
// The section's first puts entity records ahead, so a one-by-one retry keeps
// that order.
func (s *Service) deletes(ctx context.Context, tx pgx.Tx, in []deletion) error {
	by := map[string][]string{}
	for _, d := range in {
		if d.id == "" {
			continue
		}
		by[d.entity] = append(by[d.entity], d.id)
	}
	gs := make([]entities.EntityID, len(by[importer.RecordEntity]))
	for i, id := range by[importer.RecordEntity] {
		gs[i] = entities.EntityID(id)
	}
	if err := s.o.Entities.ImportDeletes(ctx, tx, gs); err != nil {
		return err
	}
	if len(by[importer.RecordChannel]) > 0 {
		if err := s.o.Channels.ImportDeletesTx(ctx, tx, by[importer.RecordChannel]); err != nil {
			return err
		}
	}
	if err := s.o.Catalog.ImportDeletes(ctx, tx, by[importer.RecordContent], by[importer.RecordVersion]); err != nil {
		return err
	}
	return s.o.Content.ImportDeletes(ctx, tx, by[importer.RecordComment], by[importer.RecordPoll])
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
