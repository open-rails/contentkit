package entitystore

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/open-rails/contentkit/access"
	entities "github.com/open-rails/contentkit/entities"
)

// Mine pages the active entities the store's viewer (As) owns, edits or
// writes content of through its AuthKit scope (Options.Scopes) or holds the
// key of (Options.Gate), filtered, ordered and paged as List.
func (s *Store) Mine(ctx context.Context, opts ListOptions) (entities.MyEntityPage, error) {
	out := entities.MyEntityPage{Entities: []entities.MyEntity{}}
	if err := s.requireInternalRead(ctx, opts.IncludeInternal); err != nil {
		return out, err
	}
	v := s.viewer()
	if !v.Member() {
		return out, fmt.Errorf("%w: only a signed-in person has groups", ErrForbidden)
	}
	scopes, err := s.heldScopes(ctx, v, opts.Kind)
	if err != nil {
		return out, err
	}
	var joined []string
	if s.gate != nil {
		filter, err := s.gate.Filter(ctx, v)
		if err != nil {
			return out, err
		}
		joined = filter.Channels()
	}
	type mine struct{ owner, editor, writer, moderator, member, canPost, canEditOwn bool }
	rel := map[entities.EntityID]mine{}
	err = s.read(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT id, kind, coalesce(auth_scope, ''), coalesce(owner_user_id, '') = $2, coalesce(id = ANY($4::text[]), false)
 FROM %s WHERE tenant_id = $1 AND state = 'active' AND (owner_user_id = $2 OR auth_scope = ANY($3::text[]) OR id = ANY($4::text[]))`,
			s.table("content_entities")), s.tenant, v.AccountID(), slices.Collect(maps.Keys(scopes)), joined)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id entities.EntityID
			var kind, scope string
			var m mine
			if err := rows.Scan(&id, &kind, &scope, &m.owner, &m.member); err != nil {
				return err
			}
			held := scopes[scope]
			directOwner := m.owner && scope == "" && !s.cfg.EntityKinds[kind].AuthScope
			m.owner = m.owner && (directOwner || slices.Contains(held, access.ChannelSettingsManage))
			m.editor = directOwner || slices.Contains(held, access.ChannelSettingsManage)
			m.writer = directOwner || slices.Contains(held, access.ChannelContentWrite)
			m.canPost = m.writer || slices.Contains(held, access.ChannelContentCreate)
			m.canEditOwn = m.writer || slices.Contains(held, access.ChannelContentEditOwn)
			m.moderator = directOwner || slices.Contains(held, access.ChannelCommentsModerate)
			if m.owner || m.editor || m.writer || m.moderator || m.member || m.canPost || m.canEditOwn {
				rel[id] = m
			}
		}
		return rows.Err()
	})
	if err != nil {
		return out, err
	}
	ids := make([]string, 0, len(rel))
	for id := range rel {
		ids = append(ids, string(id))
	}
	slices.Sort(ids)
	c, err := s.compile(opts, ids)
	if err != nil {
		return out, err
	}
	var page entities.EntityPage
	if err := s.read(ctx, func(q Querier) (err error) {
		page, err = s.cards(ctx, q, c)
		return err
	}); err != nil {
		return out, err
	}
	shows := make([]shown, len(page.Entities))
	for i := range page.Entities {
		shows[i] = cardShown(&page.Entities[i])
	}
	if err := s.fill(ctx, shows, opts.Language); err != nil {
		return out, err
	}
	for _, g := range page.Entities {
		m := rel[g.ID]
		out.Entities = append(out.Entities, entities.MyEntity{EntityCard: g, Owner: m.owner, Editor: m.editor, Writer: m.writer, Moderator: m.moderator, Member: m.member, CanPost: m.canPost, CanEditOwn: m.canEditOwn})
	}
	out.NextCursor = page.NextCursor
	return out, nil
}
