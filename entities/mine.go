package entities

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/open-rails/contentkit/access"
)

// MyEntity is an entity of the viewer's, with how it is theirs.
type MyEntity struct {
	EntityCard
	// Owner: the viewer is its recorded owner and retains ownership authority.
	Owner bool `json:"owner"`
	// Editor: the viewer holds its kind's access.EntitySettingsManage in its
	// AuthKit scope.
	Editor bool `json:"editor"`
	// CanPost permits new personal posts in this channel.
	CanPost bool `json:"can_post"`
	// CanEditOwn permits editing the viewer's own posts, not other authors.
	CanEditOwn bool `json:"can_edit_own"`
	// Writer: the viewer holds its kind's access.EntityContentWrite there.
	Writer bool `json:"writer"`
	// Moderator: the viewer holds its kind's access.EntityCommentsModerate
	// there, or owns it.
	Moderator bool `json:"moderator"`
	// Member: the viewer holds its key.
	Member bool `json:"member"`
}

// MyEntityPage is one page of the viewer's entities.
type MyEntityPage struct {
	Entities   []MyEntity `json:"entities"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// Mine pages the active entities the store's viewer (As) owns, edits or
// writes content of through its AuthKit scope (Options.Scopes) or holds the
// key of (Options.Gate), filtered, ordered and paged as List.
func (s *Store) Mine(ctx context.Context, opts ListOptions) (MyEntityPage, error) {
	out := MyEntityPage{Entities: []MyEntity{}}
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
		joined = filter.Entities()
	}
	type mine struct{ owner, editor, writer, moderator, member, canPost, canEditOwn bool }
	rel := map[EntityID]mine{}
	err = s.read(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT id, kind, coalesce(auth_scope, ''), coalesce(owner_user_id, '') = $2, coalesce(id = ANY($4::text[]), false)
 FROM %s WHERE tenant_id = $1 AND state = 'active' AND (owner_user_id = $2 OR auth_scope = ANY($3::text[]) OR id = ANY($4::text[]))`,
			s.table("content_entities")), s.tenant, v.AccountID(), slices.Collect(maps.Keys(scopes)), joined)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id EntityID
			var kind, scope string
			var m mine
			if err := rows.Scan(&id, &kind, &scope, &m.owner, &m.member); err != nil {
				return err
			}
			held := scopes[scope]
			directOwner := m.owner && scope == ""
			m.owner = m.owner && (directOwner || slices.Contains(held, access.EntitySettingsManage(kind)))
			m.editor = directOwner || slices.Contains(held, access.EntitySettingsManage(kind))
			m.writer = directOwner || slices.Contains(held, access.EntityContentWrite(kind))
			m.canPost = m.writer || slices.Contains(held, access.EntityContentCreate(kind))
			m.canEditOwn = m.writer || slices.Contains(held, access.EntityContentEditOwn(kind))
			m.moderator = directOwner || slices.Contains(held, access.EntityCommentsModerate(kind))
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
	var page EntityPage
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
		out.Entities = append(out.Entities, MyEntity{EntityCard: g, Owner: m.owner, Editor: m.editor, Writer: m.writer, Moderator: m.moderator, Member: m.member, CanPost: m.canPost, CanEditOwn: m.canEditOwn})
	}
	out.NextCursor = page.NextCursor
	return out, nil
}
