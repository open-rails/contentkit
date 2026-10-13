package groups

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/kinds"
)

// GroupInput creates one group.
type GroupInput struct {
	// ID is optional; omitted ids are generated.
	ID   GroupID `json:"id,omitempty"`
	Kind string  `json:"kind"`
	Slug string  `json:"slug"`
	// ScopeID is required for a kind scoped by another (a character's series).
	ScopeID    GroupID        `json:"scope_id,omitempty"`
	ParentID   GroupID        `json:"parent_id,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Cover      *Cover         `json:"cover,omitempty"`
	Avatar     *Avatar        `json:"avatar,omitempty"`
	// OwnerUserID owns a user-owned group; a person creating one owns it.
	OwnerUserID string `json:"owner_user_id,omitempty"`
	// AuthScope is staff's (an import's); ContentKit provisions it otherwise.
	AuthScope string `json:"auth_scope,omitempty"`
	Names     []Name `json:"names,omitempty"`
	Text      []Text `json:"text,omitempty"`
}

// GroupUpdate changes one group's given fields; nil fields are unchanged.
type GroupUpdate struct {
	// ID names the group; routes take it from the path.
	ID      GroupID  `json:"-"`
	Slug    *string  `json:"slug,omitempty"`
	ScopeID *GroupID `json:"scope_id,omitempty"`
	// ParentID "" detaches the group from its parent.
	ParentID *GroupID `json:"parent_id,omitempty"`
	// State restores (active) or deletes a group.
	State *State `json:"state,omitempty"`
	// Attributes replaces every attribute.
	Attributes map[string]any `json:"attributes,omitempty"`
	// Cover selects a cover; an empty one returns to the automatic cover.
	Cover *Cover `json:"cover,omitempty"`
	// Avatar selects a user-owned group's avatar; an empty one returns to
	// the first.
	Avatar      *Avatar `json:"avatar,omitempty"`
	OwnerUserID *string `json:"owner_user_id,omitempty"`
	AuthScope   *string `json:"auth_scope,omitempty"`
	// Names replaces every name and alias.
	Names []Name `json:"names,omitempty"`
	// Text replaces the text of every language.
	Text []Text `json:"text,omitempty"`
}

// staffOnly names the fields of u only staff change.
func (u GroupUpdate) staffOnly() string {
	switch {
	case u.Slug != nil:
		return "slug"
	case u.ScopeID != nil:
		return "scope_id"
	case u.ParentID != nil:
		return "parent_id"
	case u.OwnerUserID != nil:
		return "owner_user_id"
	case u.AuthScope != nil:
		return "auth_scope"
	}
	return ""
}

type groupRow struct {
	Ordinal     int            `json:"ordinal"`
	ID          string         `json:"id"`
	Kind        string         `json:"kind"`
	Slug        string         `json:"slug"`
	ScopeID     string         `json:"scope_id"`
	ParentID    *string        `json:"parent_id"`
	Attributes  map[string]any `json:"attributes"`
	Cover       *Cover         `json:"cover"`
	Avatar      *string        `json:"avatar"`
	OwnerUserID *string        `json:"owner_user_id"`
	AuthScope   *string        `json:"auth_scope"`
}

type textRow struct {
	GroupID     string `json:"group_id"`
	Language    string `json:"language"`
	Description string `json:"description"`
	SortKey     string `json:"sort_key"`
}

func optional(s string) *string {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return &s
}

func validateText(id GroupID, text []Text) ([]textRow, error) {
	rows := make([]textRow, 0, len(text))
	seen := map[string]bool{}
	for _, t := range text {
		lang, err := normalizeLanguage(t.Language)
		if err != nil {
			return nil, err
		}
		if seen[lang] {
			return nil, fmt.Errorf("%w: two texts for %s", ErrInvalid, lang)
		}
		seen[lang] = true
		if utf8.RuneCountInString(t.Description) > 20000 || utf8.RuneCountInString(t.SortKey) > 512 {
			return nil, fmt.Errorf("%w: text for %s is too long", ErrInvalid, lang)
		}
		rows = append(rows, textRow{GroupID: string(id), Language: lang, Description: t.Description, SortKey: strings.TrimSpace(t.SortKey)})
	}
	return rows, nil
}

// validateCover accepts an uploaded image name ("i-{uuid}") or a content id;
// an empty cover clears the selection (nil).
func validateCover(c *Cover) (*Cover, error) {
	if c == nil || (c.Image == "" && c.ContentID == "") {
		return nil, nil
	}
	if c.Image != "" && c.ContentID != "" {
		return nil, fmt.Errorf("%w: a cover is an image or a content, not both", ErrInvalid)
	}
	if c.Image != "" {
		u, ok := strings.CutPrefix(c.Image, "i-")
		if parsed, err := uuid.Parse(u); !ok || err != nil || parsed.String() != u {
			return nil, fmt.Errorf("%w: cover image %q is not an image name", ErrInvalid, c.Image)
		}
		return &Cover{Image: c.Image}, nil
	}
	if err := contentref.ValidateID(c.ContentID); err != nil {
		return nil, fmt.Errorf("%w: cover content: %w", ErrInvalid, err)
	}
	return &Cover{ContentID: c.ContentID}, nil
}

// validateAvatar accepts an uploaded avatar's name ("i-{uuid}"); an empty
// avatar clears the selection (nil).
func validateAvatar(a *Avatar) (*string, error) {
	if a == nil || a.Image == "" {
		return nil, nil
	}
	u, ok := strings.CutPrefix(a.Image, "i-")
	if parsed, err := uuid.Parse(u); !ok || err != nil || parsed.String() != u {
		return nil, fmt.Errorf("%w: avatar %q is not an image name", ErrInvalid, a.Image)
	}
	return &a.Image, nil
}

// fields checks a group's own fields against its kind.
func (s *Store) fields(gk kinds.GroupKind, kind string, g *groupRow) error {
	if (g.ScopeID != "") != (gk.ScopedBy != "") {
		if gk.ScopedBy == "" {
			return fmt.Errorf("%w: %s groups take no scope", ErrInvalid, kind)
		}
		return fmt.Errorf("%w: a %s needs a %s scope", ErrInvalid, kind, gk.ScopedBy)
	}
	if g.ParentID != nil && gk.Parent == "" {
		return fmt.Errorf("%w: %s groups take no parent", ErrInvalid, kind)
	}
	if (g.OwnerUserID != nil) != (gk.Owner == "user") {
		if gk.Owner == "user" {
			return fmt.Errorf("%w: %s groups need an owner", ErrInvalid, kind)
		}
		return fmt.Errorf("%w: %s groups are owned by the site", ErrInvalid, kind)
	}
	if g.AuthScope != nil && !gk.AuthScope {
		return fmt.Errorf("%w: %s groups take no auth scope", ErrInvalid, kind)
	}
	if g.Avatar != nil && gk.Owner != "user" {
		return fmt.Errorf("%w: %s groups take no avatar", ErrInvalid, kind)
	}
	attrs, err := kinds.ValidateAttributes(gk.Attributes, g.Attributes)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	g.Attributes = attrs
	return nil
}

// checkLinks verifies, in q, the parents and scopes of the given groups:
// active groups of the kind their kind names, no parent cycle, and a cover
// content that exists.
func (s *Store) checkLinks(ctx context.Context, q Querier, ids []string) error {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT g.id, g.kind, coalesce(p.kind, ''), coalesce(p.state, ''), g.parent_id IS NOT NULL,
 coalesce(sc.kind, ''), coalesce(sc.state, ''), g.scope_id <> '',
 coalesce(g.cover ? 'content_id' AND NOT g.cover ? 'auto' AND NOT EXISTS (SELECT 1 FROM %[2]s c WHERE c.tenant_id = g.tenant_id AND c.id = g.cover->>'content_id'), false)
 FROM %[1]s g
 LEFT JOIN %[1]s p ON p.tenant_id = g.tenant_id AND p.id = g.parent_id
 LEFT JOIN %[1]s sc ON sc.tenant_id = g.tenant_id AND sc.id = g.scope_id
 WHERE g.tenant_id = $1 AND g.id = ANY($2::text[])`, s.table("content_groups"), s.table("content_items")), s.tenant, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind, pKind, pState, sKind, sState string
		var hasParent, hasScope, missingCover bool
		if err := rows.Scan(&id, &kind, &pKind, &pState, &hasParent, &sKind, &sState, &hasScope, &missingCover); err != nil {
			return err
		}
		gk := s.cfg.GroupKinds[kind]
		if hasParent && (pKind != gk.Parent || pState != string(StateActive)) {
			return fmt.Errorf("%w: the parent of %s must be an active %s", ErrInvalid, id, gk.Parent)
		}
		if hasScope && (sKind != gk.ScopedBy || sState != string(StateActive)) {
			return fmt.Errorf("%w: the scope of %s must be an active %s", ErrInvalid, id, gk.ScopedBy)
		}
		if missingCover {
			return fmt.Errorf("%w: the cover content of %s does not exist", ErrNotFound, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var cycle string
	err = q.QueryRow(ctx, fmt.Sprintf(`WITH RECURSIVE up(start, id, depth) AS (
 SELECT g.id, g.parent_id, 1 FROM %[1]s g WHERE g.tenant_id = $1 AND g.id = ANY($2::text[]) AND g.parent_id IS NOT NULL
 UNION ALL
 SELECT up.start, g.parent_id, up.depth + 1 FROM up JOIN %[1]s g ON g.tenant_id = $1 AND g.id = up.id
 WHERE g.parent_id IS NOT NULL AND up.id <> up.start AND up.depth < 64)
SELECT coalesce((SELECT start FROM up WHERE id = start LIMIT 1), '')`, s.table("content_groups")), s.tenant, ids).Scan(&cycle)
	if err != nil {
		return err
	}
	if cycle != "" {
		return fmt.Errorf("%w: group %s would be its own ancestor", ErrInvalid, cycle)
	}
	return nil
}

// CreateGroups creates groups with their names and text in one transaction.
// A taken id or slug is ErrConflict; a kind, scope or parent the kinds config
// does not allow is ErrInvalid. Written as a person (As), each is of a
// user-owned kind and the person's own, within the kind's MaxPerOwner
// (ErrLimit), linked only to groups the person edits; anything else is
// ErrForbidden. An auth_scope kind's group gets its AuthKit scope (Scopes)
// after commit, else from the scope sync job.
func (s *Store) CreateGroups(ctx context.Context, inputs []GroupInput) ([]Group, error) {
	if len(inputs) == 0 {
		return []Group{}, nil
	}
	creator, err := s.creator(ctx, inputs)
	if err != nil {
		return nil, err
	}
	rows := make([]groupRow, 0, len(inputs))
	var names []nameRow
	var texts []textRow
	ids := make([]string, len(inputs))
	for i, in := range inputs {
		kind := strings.TrimSpace(in.Kind)
		gk, err := s.groupKind(kind)
		if err != nil {
			return nil, err
		}
		slug, err := validateSlug(in.Slug)
		if err != nil {
			return nil, err
		}
		if in.ID == "" {
			in.ID = GroupID(contentref.NewID())
		} else if err := validateID(in.ID); err != nil {
			return nil, err
		}
		if creator != "" {
			in.OwnerUserID = creator
		}
		row := groupRow{Ordinal: i, ID: string(in.ID), Kind: kind, Slug: slug, Attributes: in.Attributes,
			OwnerUserID: optional(in.OwnerUserID), AuthScope: optional(in.AuthScope)}
		if row.Avatar, err = validateAvatar(in.Avatar); err != nil {
			return nil, err
		}
		if in.ScopeID != "" {
			if err := validateID(in.ScopeID); err != nil {
				return nil, err
			}
			row.ScopeID = string(in.ScopeID)
		}
		if in.ParentID != "" {
			if err := validateID(in.ParentID); err != nil {
				return nil, err
			}
			p := string(in.ParentID)
			row.ParentID = &p
		}
		if row.Cover, err = validateCover(in.Cover); err != nil {
			return nil, err
		}
		if err := s.fields(gk, kind, &row); err != nil {
			return nil, err
		}
		n, err := validateNames(in.ID, in.Names)
		if err != nil {
			return nil, err
		}
		for _, r := range n {
			r.Ordinal = len(names)
			names = append(names, r)
		}
		t, err := validateText(in.ID, in.Text)
		if err != nil {
			return nil, err
		}
		texts = append(texts, t...)
		rows = append(rows, row)
		ids[i] = row.ID
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	out := make([]Group, len(inputs))
	err = s.run(ctx, func(q Querier) error {
		if err := s.lockOwners(ctx, q, rows); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (tenant_id, id, kind, slug, scope_id, parent_id, attributes, cover, avatar, owner_user_id, auth_scope)
 SELECT $1, r.id, r.kind, r.slug, r.scope_id, r.parent_id, coalesce(r.attributes, '{}'), r.cover, r.avatar, r.owner_user_id, r.auth_scope
 FROM jsonb_to_recordset($2::jsonb) AS r(ordinal int, id text, kind text, slug text, scope_id text, parent_id text, attributes jsonb, cover jsonb, avatar text, owner_user_id text, auth_scope text)
 ORDER BY r.ordinal`, s.table("content_groups")), s.tenant, data); err != nil {
			return err
		}
		if err := s.checkLinks(ctx, q, ids); err != nil {
			return err
		}
		if err := s.checkCaps(ctx, q, rows); err != nil {
			return err
		}
		if err := s.mayLink(ctx, q, rows); err != nil {
			return err
		}
		if err := s.insertNames(ctx, q, names); err != nil {
			return err
		}
		if err := s.insertText(ctx, q, texts); err != nil {
			return err
		}
		if err := s.syncCodes(ctx, q, ids); err != nil {
			return err
		}
		created, err := s.byIDs(ctx, q, ids)
		if err != nil {
			return err
		}
		for i, id := range ids {
			out[i] = created[GroupID(id)]
		}
		if err := s.markDirty(ctx, q, out); err != nil {
			return err
		}
		return s.queueSync(ctx, q, s.unscoped(out))
	})
	if err != nil {
		return nil, err
	}
	return s.syncAfter(ctx, out, s.unscoped(out))
}

func (s *Store) insertText(ctx context.Context, q Querier, rows []textRow) error {
	if len(rows) == 0 {
		return nil
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (tenant_id, group_id, language, description, sort_key)
 SELECT $1, r.group_id, r.language, r.description, r.sort_key FROM jsonb_to_recordset($2::jsonb) AS r(group_id text, language text, description text, sort_key text)
 ON CONFLICT (tenant_id, group_id, language) DO UPDATE SET description = EXCLUDED.description, sort_key = EXCLUDED.sort_key`, s.table("content_group_text")), s.tenant, data)
	return err
}

func (s *Store) byIDs(ctx context.Context, q Querier, ids []string) (map[GroupID]Group, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT %s FROM %s g WHERE g.tenant_id = $1 AND g.id = ANY($2::text[])`, groupColumns, s.table("content_groups")), s.tenant, ids)
	if err != nil {
		return nil, err
	}
	list, err := scanGroups(rows)
	if err != nil {
		return nil, err
	}
	out := make(map[GroupID]Group, len(list))
	for _, g := range list {
		out[g.ID] = g
	}
	return out, nil
}

// Groups returns the requested groups that exist, in any state, in input
// order.
func (s *Store) Groups(ctx context.Context, ids []GroupID) ([]Group, error) {
	list, err := uniqueIDs(ids)
	if err != nil {
		return nil, err
	}
	out := []Group{}
	if len(list) == 0 {
		return out, nil
	}
	err = s.read(ctx, func(q Querier) error {
		found, err := s.byIDs(ctx, q, list)
		for _, id := range list {
			if g, ok := found[GroupID(id)]; ok {
				out = append(out, g)
			}
		}
		return err
	})
	return out, err
}

// UpdateGroups applies each update in one transaction. A merged group is
// never updated (ErrConflict). Deleting or restoring a group, or moving it to
// another parent, re-derives the facets of the content it reaches, retires
// or provisions its AuthKit scope and hides or shows its media; deleting a
// user-owned group deletes the content it owns, restoring it restores that
// content and, within the retention, its media (Options.Owned,
// Options.Folders). Renaming a sellable group renames its offer's product.
// Written as a person (As), each
// group is a user-owned one the person owns or holds
// access.GroupSettingsManage in, its slug, links, owner and scope stay, and
// a restore stays within the kind's MaxPerOwner (ErrLimit); anything else is
// ErrForbidden.
func (s *Store) UpdateGroups(ctx context.Context, updates []GroupUpdate) ([]Group, error) {
	if len(updates) == 0 {
		return []Group{}, nil
	}
	staff, err := s.staff(ctx)
	if err != nil {
		return nil, err
	}
	if !staff {
		for _, u := range updates {
			if f := u.staffOnly(); f != "" {
				return nil, fmt.Errorf("%w: only staff change %s", ErrForbidden, f)
			}
		}
	}
	ids := make([]GroupID, len(updates))
	for i, u := range updates {
		ids[i] = u.ID
	}
	list, err := uniqueIDs(ids)
	if err != nil {
		return nil, err
	}
	if len(list) != len(updates) {
		return nil, fmt.Errorf("%w: a group is updated twice", ErrInvalid)
	}
	out := make([]Group, len(updates))
	var changed []string
	err = s.run(ctx, func(q Querier) error {
		locked, err := s.lock(ctx, q, list)
		if err != nil {
			return err
		}
		if err := s.mayEdit(ctx, locked); err != nil {
			return err
		}
		current := map[GroupID]Group{}
		for _, g := range locked {
			current[g.ID] = g
		}
		var rows []updateRow
		var texts []textRow
		var names []nameRow
		changed = changed[:0]
		var textIDs, nameIDs, reach []string
		var restored []groupRow
		var flipped, renamed []Ref
		for i, u := range updates {
			g := current[u.ID]
			if g.State == StateMerged {
				return fmt.Errorf("%w: group %s is merged", ErrConflict, g.ID)
			}
			row, moved, err := s.apply(g, u)
			if err != nil {
				return err
			}
			row.Ordinal = i
			rows = append(rows, row)
			if moved {
				reach = append(reach, string(g.ID))
			}
			if u.State != nil && *u.State != g.State {
				gk := s.cfg.GroupKinds[g.Kind]
				if gk.AuthScope || g.AuthScope != "" || gk.Owner == "user" {
					changed = append(changed, string(g.ID))
				}
				if *u.State == StateActive {
					restored = append(restored, row.groupRow)
				}
				flipped = append(flipped, Ref{Kind: g.Kind, ID: g.ID})
			}
			if u.Names != nil || u.Slug != nil {
				renamed = append(renamed, Ref{Kind: g.Kind, ID: g.ID})
			}
			if u.Names != nil {
				n, err := validateNames(g.ID, u.Names)
				if err != nil {
					return err
				}
				for _, r := range n {
					r.Ordinal = len(names)
					names = append(names, r)
				}
				nameIDs = append(nameIDs, string(g.ID))
			}
			if u.Text != nil {
				t, err := validateText(g.ID, u.Text)
				if err != nil {
					return err
				}
				texts = append(texts, t...)
				textIDs = append(textIDs, string(g.ID))
			}
		}
		data, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		if err := s.lockOwners(ctx, q, restored); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, fmt.Sprintf(`UPDATE %s g SET slug = r.slug, scope_id = r.scope_id, parent_id = r.parent_id, state = r.state,
 attributes = coalesce(r.attributes, '{}'), cover = r.cover, avatar = r.avatar, owner_user_id = r.owner_user_id, auth_scope = r.auth_scope, updated_at = now(), revision = g.revision + 1
 FROM jsonb_to_recordset($2::jsonb) AS r(id text, slug text, scope_id text, parent_id text, state text, attributes jsonb, cover jsonb, avatar text, owner_user_id text, auth_scope text)
 WHERE g.tenant_id = $1 AND g.id = r.id`, s.table("content_groups")), s.tenant, data); err != nil {
			return err
		}
		if err := s.checkCaps(ctx, q, restored); err != nil {
			return err
		}
		if tx, ok := q.(pgx.Tx); ok && s.folders != nil && len(flipped) > 0 {
			if err := s.folders.ExposeTx(ctx, tx, flipped); err != nil {
				return err
			}
		}
		if len(nameIDs) > 0 {
			if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = $1 AND group_id = ANY($2::text[])`, s.table("content_group_names")), s.tenant, nameIDs); err != nil {
				return err
			}
			if err := s.insertNames(ctx, q, names); err != nil {
				return err
			}
		}
		if err := s.resyncSold(ctx, q, renamed); err != nil {
			return err
		}
		if len(textIDs) > 0 {
			if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = $1 AND group_id = ANY($2::text[])`, s.table("content_group_text")), s.tenant, textIDs); err != nil {
				return err
			}
			if err := s.insertText(ctx, q, texts); err != nil {
				return err
			}
		}
		if err := s.checkLinks(ctx, q, list); err != nil {
			return err
		}
		if err := s.syncCodes(ctx, q, list); err != nil {
			return err
		}
		if err := s.refreshGroupFacets(ctx, q, reach); err != nil {
			return err
		}
		updated, err := s.byIDs(ctx, q, list)
		if err != nil {
			return err
		}
		for i, u := range updates {
			out[i] = updated[u.ID]
		}
		if err := s.markDirty(ctx, q, out); err != nil {
			return err
		}
		return s.queueSync(ctx, q, changed)
	})
	if err != nil {
		return nil, err
	}
	return s.syncAfter(ctx, out, changed)
}

// updateRow is groupRow plus the state an update writes.
type updateRow struct {
	groupRow
	State string `json:"state"`
}

// apply merges u into g and validates the result; moved reports a change of
// state or parent, which changes what the group's members are effective in.
func (s *Store) apply(g Group, u GroupUpdate) (updateRow, bool, error) {
	gk, err := s.groupKind(g.Kind)
	if err != nil {
		return updateRow{}, false, err
	}
	row := updateRow{groupRow: groupRow{ID: string(g.ID), Kind: g.Kind, Slug: g.Slug, ScopeID: string(g.ScopeID), Attributes: g.Attributes,
		Cover: g.Cover, OwnerUserID: optional(g.OwnerUserID), AuthScope: optional(g.AuthScope)}, State: string(g.State)}
	if g.Avatar != nil {
		row.Avatar = optional(g.Avatar.Image)
	}
	if g.ParentID != "" {
		p := string(g.ParentID)
		row.ParentID = &p
	}
	moved := false
	if u.Slug != nil {
		if row.Slug, err = validateSlug(*u.Slug); err != nil {
			return row, false, err
		}
	}
	if u.ScopeID != nil {
		if *u.ScopeID != "" {
			if err := validateID(*u.ScopeID); err != nil {
				return row, false, err
			}
		}
		row.ScopeID = string(*u.ScopeID)
	}
	if u.ParentID != nil {
		row.ParentID = nil
		if *u.ParentID != "" {
			if err := validateID(*u.ParentID); err != nil {
				return row, false, err
			}
			if *u.ParentID == g.ID {
				return row, false, fmt.Errorf("%w: group %s would be its own parent", ErrInvalid, g.ID)
			}
			p := string(*u.ParentID)
			row.ParentID = &p
		}
		moved = string(g.ParentID) != *orEmpty(row.ParentID)
	}
	if u.State != nil {
		if *u.State != StateActive && *u.State != StateDeleted {
			return row, false, fmt.Errorf("%w: state %q is not assignable", ErrInvalid, *u.State)
		}
		moved = moved || *u.State != g.State
		row.State = string(*u.State)
	}
	if u.Attributes != nil {
		row.Attributes = u.Attributes
	}
	if u.Cover != nil {
		if row.Cover, err = validateCover(u.Cover); err != nil {
			return row, false, err
		}
	}
	if u.Avatar != nil {
		if row.Avatar, err = validateAvatar(u.Avatar); err != nil {
			return row, false, err
		}
	}
	if u.OwnerUserID != nil {
		row.OwnerUserID = optional(*u.OwnerUserID)
	}
	if u.AuthScope != nil {
		row.AuthScope = optional(*u.AuthScope)
	}
	if err := s.fields(gk, g.Kind, &row.groupRow); err != nil {
		return row, false, err
	}
	return row, moved, nil
}

func orEmpty(s *string) *string {
	if s == nil {
		e := ""
		return &e
	}
	return s
}

// DeleteGroups marks groups deleted: they keep their memberships and code but
// are effective nowhere, listed nowhere and lose their documents.
func (s *Store) DeleteGroups(ctx context.Context, ids []GroupID) ([]Group, error) {
	deleted := StateDeleted
	updates := make([]GroupUpdate, len(ids))
	for i, id := range ids {
		updates[i] = GroupUpdate{ID: id, State: &deleted}
	}
	return s.UpdateGroups(ctx, updates)
}
