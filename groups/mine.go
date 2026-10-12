package groups

import (
	"context"
	"fmt"
	"slices"
)

// MyGroup is a group of the viewer's, with how it is theirs.
type MyGroup struct {
	GroupCard
	// Owner: the viewer owns it.
	Owner bool `json:"owner"`
	// Editor: the viewer holds its kind's access.GroupSettingsManage in its
	// AuthKit scope.
	Editor bool `json:"editor"`
	// Member: the viewer holds its key.
	Member bool `json:"member"`
}

// MyGroupPage is one page of the viewer's groups.
type MyGroupPage struct {
	Groups     []MyGroup `json:"groups"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// Mine pages the active groups the store's viewer (As) owns, edits through
// its AuthKit scope (Options.Scopes) or holds the key of (Options.Sales),
// filtered, ordered and paged as List.
func (s *Store) Mine(ctx context.Context, opts ListOptions) (MyGroupPage, error) {
	out := MyGroupPage{Groups: []MyGroup{}}
	v := s.viewer()
	if !v.Person() {
		return out, fmt.Errorf("%w: only a signed-in person has groups", ErrForbidden)
	}
	scopes, err := s.heldScopes(ctx, v.ID, opts.Kind)
	if err != nil {
		return out, err
	}
	var joined []string
	if s.sales != nil {
		if joined, err = s.sales.Joined(ctx, v); err != nil {
			return out, err
		}
	}
	type mine struct{ owner, editor, member bool }
	rel := map[GroupID]mine{}
	err = s.read(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT id, coalesce(owner_user_id, '') = $2, coalesce(auth_scope = ANY($3::text[]), false), coalesce(id = ANY($4::text[]), false)
 FROM %s WHERE tenant_id = $1 AND state = 'active' AND (owner_user_id = $2 OR auth_scope = ANY($3::text[]) OR id = ANY($4::text[]))`,
			s.table("content_groups")), s.tenant, v.ID, scopes, joined)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id GroupID
			var m mine
			if err := rows.Scan(&id, &m.owner, &m.editor, &m.member); err != nil {
				return err
			}
			rel[id] = m
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
	var page GroupPage
	if err := s.read(ctx, func(q Querier) (err error) {
		page, err = s.cards(ctx, q, c)
		return err
	}); err != nil {
		return out, err
	}
	shows := make([]shown, len(page.Groups))
	for i := range page.Groups {
		shows[i] = cardShown(&page.Groups[i])
	}
	if err := s.fill(ctx, shows); err != nil {
		return out, err
	}
	for _, g := range page.Groups {
		m := rel[g.ID]
		out.Groups = append(out.Groups, MyGroup{GroupCard: g, Owner: m.owner, Editor: m.editor, Member: m.member})
	}
	out.NextCursor = page.NextCursor
	return out, nil
}
