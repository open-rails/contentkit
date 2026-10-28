package entitystore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	entities "github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
)

// staffOnlyFields names the fields of u only staff change.
func staffOnlyFields(u entities.EntityUpdate) string {
	switch {
	case u.Internal != nil:
		return "internal"
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

type entityRow struct {
	Ordinal     int             `json:"ordinal"`
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Slug        string          `json:"slug"`
	Internal    bool            `json:"internal"`
	ScopeID     string          `json:"scope_id"`
	ParentID    *string         `json:"parent_id"`
	Attributes  map[string]any  `json:"attributes"`
	Cover       *entities.Cover `json:"cover"`
	Avatar      *string         `json:"avatar"`
	OwnerUserID *string         `json:"owner_user_id"`
	AuthScope   *string         `json:"auth_scope"`
}

type textRow struct {
	EntityID    string `json:"entity_id"`
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

func validateText(id entities.EntityID, text []entities.Text) ([]textRow, error) {
	rows := make([]textRow, 0, len(text))
	seen := map[string]bool{}
	for _, t := range text {
		lang, err := normalizeLanguage(t.Language)
		if err != nil {
			return nil, err
		}
		if seen[lang] {
			return nil, fmt.Errorf("%w: two texts for %s", entities.ErrInvalid, lang)
		}
		seen[lang] = true
		if utf8.RuneCountInString(t.Description) > 20000 || utf8.RuneCountInString(t.SortKey) > 512 {
			return nil, fmt.Errorf("%w: text for %s is too long", entities.ErrInvalid, lang)
		}
		rows = append(rows, textRow{EntityID: string(id), Language: lang, Description: t.Description, SortKey: strings.TrimSpace(t.SortKey)})
	}
	return rows, nil
}

// validateCover accepts an uploaded image name ("i-{uuid}") or a content id;
// an empty cover clears the selection (nil).
func validateCover(c *entities.Cover) (*entities.Cover, error) {
	if c == nil || (c.Image == "" && c.ContentID == "") {
		return nil, nil
	}
	if c.Image != "" && c.ContentID != "" {
		return nil, fmt.Errorf("%w: a cover is an image or a content, not both", entities.ErrInvalid)
	}
	if c.Image != "" {
		u, ok := strings.CutPrefix(c.Image, "i-")
		if parsed, err := uuid.Parse(u); !ok || err != nil || parsed.String() != u {
			return nil, fmt.Errorf("%w: cover image %q is not an image name", entities.ErrInvalid, c.Image)
		}
		return &entities.Cover{Image: c.Image}, nil
	}
	if err := contentref.ValidateID(c.ContentID); err != nil {
		return nil, fmt.Errorf("%w: cover content: %w", entities.ErrInvalid, err)
	}
	return &entities.Cover{ContentID: c.ContentID}, nil
}

// validateAvatar accepts an uploaded avatar's name ("i-{uuid}"); an empty
// avatar clears the selection (nil).
func validateAvatar(a *entities.Avatar) (*string, error) {
	if a == nil || a.Image == "" {
		return nil, nil
	}
	u, ok := strings.CutPrefix(a.Image, "i-")
	if parsed, err := uuid.Parse(u); !ok || err != nil || parsed.String() != u {
		return nil, fmt.Errorf("%w: avatar %q is not an image name", entities.ErrInvalid, a.Image)
	}
	return &a.Image, nil
}

// fields checks an entity's own fields against its kind.
func (s *Store) fields(gk kinds.EntityKind, kind string, g *entityRow) error {
	if (g.ScopeID != "") != (gk.ScopedBy != "") {
		if gk.ScopedBy == "" {
			return fmt.Errorf("%w: %s groups take no scope", entities.ErrInvalid, kind)
		}
		return fmt.Errorf("%w: a %s needs a %s scope", entities.ErrInvalid, kind, gk.ScopedBy)
	}
	if g.ParentID != nil && gk.Parent == "" {
		return fmt.Errorf("%w: %s groups take no parent", entities.ErrInvalid, kind)
	}
	if (g.OwnerUserID != nil) != (gk.Owner == "user") {
		if gk.Owner == "user" {
			return fmt.Errorf("%w: %s groups need an owner", entities.ErrInvalid, kind)
		}
		return fmt.Errorf("%w: %s groups are owned by the site", entities.ErrInvalid, kind)
	}
	if g.AuthScope != nil && !gk.AuthScope {
		return fmt.Errorf("%w: %s groups take no auth scope", entities.ErrInvalid, kind)
	}
	if g.Avatar != nil && !gk.HasAvatar() {
		return fmt.Errorf("%w: %s groups take no avatar", entities.ErrInvalid, kind)
	}
	attrs, err := kinds.ValidateAttributes(gk.Attributes, g.Attributes)
	if err != nil {
		return fmt.Errorf("%w: %w", entities.ErrInvalid, err)
	}
	if err := kinds.ValidateRequiredAttributes(gk.Attributes, attrs); err != nil {
		return fmt.Errorf("%w: %w", entities.ErrInvalid, err)
	}
	g.Attributes = attrs
	return nil
}

// checkLinks verifies, in q, the parents and scopes of the given entities:
// active entities of the kind their kind names, no parent cycle, and a cover
// content that exists.
func (s *Store) checkLinks(ctx context.Context, q Querier, ids []string) error {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT g.id, g.kind, coalesce(p.kind, ''), coalesce(p.state, ''), g.parent_id IS NOT NULL,
 coalesce(sc.kind, ''), coalesce(sc.state, ''), g.scope_id <> '',
 coalesce(g.cover ? 'content_id' AND NOT g.cover ? 'auto' AND NOT EXISTS (SELECT 1 FROM %[2]s c WHERE c.tenant_id = g.tenant_id AND c.id = g.cover->>'content_id'), false)
 FROM %[1]s g
 LEFT JOIN %[1]s p ON p.tenant_id = g.tenant_id AND p.id = g.parent_id
 LEFT JOIN %[1]s sc ON sc.tenant_id = g.tenant_id AND sc.id = g.scope_id
 WHERE g.tenant_id = $1 AND g.id = ANY($2::text[])`, s.table("content_entities"), s.table("content_items")), s.tenant, ids)
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
		gk := s.cfg.EntityKinds[kind]
		if hasParent && (pKind != gk.Parent || pState != string(entities.StateActive)) {
			return fmt.Errorf("%w: the parent of %s must be an active %s", entities.ErrInvalid, id, gk.Parent)
		}
		if hasScope && (sKind != gk.ScopedBy || sState != string(entities.StateActive)) {
			return fmt.Errorf("%w: the scope of %s must be an active %s", entities.ErrInvalid, id, gk.ScopedBy)
		}
		if missingCover {
			return fmt.Errorf("%w: the cover content of %s does not exist", ErrNotFound, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var cycle string
	// Topology writers hold lockHierarchy, so the stored graph is acyclic and
	// the walk ends; UNION also ends it on a cycle no writer here created.
	err = q.QueryRow(ctx, fmt.Sprintf(`WITH RECURSIVE up(start, id) AS (
 SELECT g.id, g.parent_id FROM %[1]s g WHERE g.tenant_id = $1 AND g.id = ANY($2::text[]) AND g.parent_id IS NOT NULL
 UNION
 SELECT up.start, g.parent_id FROM up JOIN %[1]s g ON g.tenant_id = $1 AND g.id = up.id
 WHERE g.parent_id IS NOT NULL AND up.id <> up.start)
SELECT coalesce((SELECT start FROM up WHERE id = start LIMIT 1), '')`, s.table("content_entities")), s.tenant, ids).Scan(&cycle)
	if err != nil {
		return err
	}
	if cycle != "" {
		return fmt.Errorf("%w: group %s would be its own ancestor", entities.ErrInvalid, cycle)
	}
	return nil
}

// lockHierarchy serializes the writers that change an entity's parent or state
// for the transaction's rest. Taken before any entity row lock, it lets each
// validate parents and cycles against the others' committed changes rather
// than each against a graph the other is changing.
func (s *Store) lockHierarchy(ctx context.Context, q Querier) error {
	_, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, s.hierarchyKey())
	return err
}

// LockHierarchyShared holds off hierarchy changes for the transaction's rest
// while letting other membership writers through. A membership writer takes
// it before any content or entity lock, so a hierarchy change either sees its
// membership or is seen by its facets.
func (s *Store) LockHierarchyShared(ctx context.Context, q Querier) error {
	_, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended($1, 0))`, s.hierarchyKey())
	return err
}

func (s *Store) hierarchyKey() string {
	return "contentkit:group-hierarchy:" + s.schema + ":" + s.tenant
}

// CreateEntities creates entities with their names and text in one transaction.
// A taken id or slug is ErrConflict; a kind, scope or parent the kinds config
// does not allow is ErrInvalid. Written as a person (As), each is of a
// user-owned kind and the person's own, within the kind's MaxPerOwner
// (ErrLimit), linked only to entities the person edits; anything else is
// ErrForbidden. An auth_scope kind's entity gets its AuthKit scope (Scopes)
// synchronously after commit; provider failures are returned for caller retry.
func (s *Store) CreateEntities(ctx context.Context, inputs []entities.EntityInput) ([]entities.Entity, error) {
	if len(inputs) == 0 {
		return []entities.Entity{}, nil
	}
	creator, err := s.creator(ctx, inputs)
	if err != nil {
		return nil, err
	}
	rows := make([]entityRow, 0, len(inputs))
	var names []nameRow
	var texts []textRow
	ids := make([]string, len(inputs))
	for i, in := range inputs {
		if !s.channels && (in.OwnerUserID != "" || in.AuthScope != "") {
			return nil, fmt.Errorf("%w: metadata has no authoring owner or scope", entities.ErrInvalid)
		}
		kind := strings.TrimSpace(in.Kind)
		gk, err := s.entityKind(kind)
		if err != nil {
			return nil, err
		}
		slug, err := validateSlug(in.Slug)
		if err != nil {
			return nil, err
		}
		if in.ID == "" {
			in.ID = entities.EntityID(contentref.NewID())
		} else if err := validateID(in.ID); err != nil {
			return nil, err
		}
		if creator != "" {
			in.OwnerUserID = creator
		}
		row := entityRow{Ordinal: i, ID: string(in.ID), Kind: kind, Slug: slug, Internal: in.Internal, Attributes: in.Attributes,
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
	out := make([]entities.Entity, len(inputs))
	parented := slices.ContainsFunc(rows, func(r entityRow) bool { return r.ParentID != nil })
	err = s.run(ctx, func(q Querier) error {
		// Only a parent can race a delete of that parent; name-linked creates,
		// which run under content locks, take no parent and skip the lock.
		if parented {
			if err := s.lockHierarchy(ctx, q); err != nil {
				return err
			}
		}
		if err := s.lockOwners(ctx, q, rows); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (tenant_id, id, kind, slug, internal, scope_id, parent_id, attributes, cover, avatar, owner_user_id, auth_scope)
 SELECT $1, r.id, r.kind, r.slug, r.internal, r.scope_id, r.parent_id, coalesce(r.attributes, '{}'), r.cover, r.avatar, r.owner_user_id, r.auth_scope
 FROM jsonb_to_recordset($2::jsonb) AS r(ordinal int, id text, kind text, slug text, internal boolean, scope_id text, parent_id text, attributes jsonb, cover jsonb, avatar text, owner_user_id text, auth_scope text)
 ORDER BY r.ordinal`, s.table("content_entities")), s.tenant, data); err != nil {
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
			out[i] = created[entities.EntityID(id)]
		}
		if err := s.markDirty(ctx, q, out); err != nil {
			return err
		}
		return s.record(ctx, q, changesOf(entities.EventCreated, out))
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
	_, err = q.Exec(ctx, fmt.Sprintf(`INSERT INTO %s (tenant_id, entity_id, language, description, sort_key)
 SELECT $1, r.entity_id, r.language, r.description, r.sort_key FROM jsonb_to_recordset($2::jsonb) AS r(entity_id text, language text, description text, sort_key text)
 ON CONFLICT (tenant_id, entity_id, language) DO UPDATE SET description = EXCLUDED.description, sort_key = EXCLUDED.sort_key`, s.table("content_entity_text")), s.tenant, data)
	return err
}

func (s *Store) byIDs(ctx context.Context, q Querier, ids []string) (map[entities.EntityID]entities.Entity, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT %s FROM %s g WHERE g.tenant_id = $1 AND g.id = ANY($2::text[]) AND %s`, entityColumns, s.table("content_entities"), s.domainPredicate("g")), s.tenant, ids)
	if err != nil {
		return nil, err
	}
	list, err := scanEntities(rows)
	if err != nil {
		return nil, err
	}
	out := make(map[entities.EntityID]entities.Entity, len(list))
	for _, g := range list {
		out[g.ID] = g
	}
	return out, nil
}

// Entities is a trusted raw lookup, including internal classifications and any
// state, in input order. Reader-facing callers use Cards or Detail instead.
func (s *Store) Entities(ctx context.Context, ids []entities.EntityID) ([]entities.Entity, error) {
	list, err := uniqueIDs(ids)
	if err != nil {
		return nil, err
	}
	out := []entities.Entity{}
	if len(list) == 0 {
		return out, nil
	}
	err = s.read(ctx, func(q Querier) error {
		found, err := s.byIDs(ctx, q, list)
		if err != nil {
			return err
		}
		for _, id := range list {
			if g, ok := found[entities.EntityID(id)]; ok {
				out = append(out, g)
			}
		}
		return nil
	})
	return out, err
}

// UpdateEntities applies each update in one transaction. A merged entity is
// never updated (ErrConflict). Deleting or restoring an entity, or moving it to
// another parent, re-derives the facets of the content it reaches, retires
// or provisions its AuthKit scope and hides or shows its media; deleting a
// user-owned entity deletes the content it owns, restoring it restores that
// content and, within the retention, its media (Options.Owned,
// Options.Folders). Billing product names are managed by the billing provider.
// Written as a person (As), each
// entity is a user-owned one the person owns or holds
// access.EntitySettingsManage in, its slug, links, owner and scope stay, and
// a restore stays within the kind's MaxPerOwner (ErrLimit); anything else is
// ErrForbidden.
func (s *Store) UpdateEntities(ctx context.Context, updates []entities.EntityUpdate) ([]entities.Entity, error) {
	if len(updates) == 0 {
		return []entities.Entity{}, nil
	}
	staff, err := s.staff(ctx)
	if err != nil {
		return nil, err
	}
	if !staff {
		for _, u := range updates {
			if f := staffOnlyFields(u); f != "" {
				return nil, fmt.Errorf("%w: only staff change %s", ErrForbidden, f)
			}
		}
	}
	ids := make([]entities.EntityID, len(updates))
	for i, u := range updates {
		if !s.channels && (u.OwnerUserID != nil || u.AuthScope != nil) {
			return nil, fmt.Errorf("%w: metadata has no authoring owner or scope", entities.ErrInvalid)
		}
		ids[i] = u.ID
	}
	list, err := uniqueIDs(ids)
	if err != nil {
		return nil, err
	}
	if len(list) != len(updates) {
		return nil, fmt.Errorf("%w: a group is updated twice", entities.ErrInvalid)
	}
	// Channel permission providers may share this pool. Resolve the current
	// grants before holding a SQL connection, then fence the authorized identity.
	authorized := map[entities.EntityID]entities.Entity{}
	if s.channels && !staff {
		before, err := s.Entities(ctx, ids)
		if err != nil {
			return nil, err
		}
		if len(before) != len(ids) {
			return nil, ErrNotFound
		}
		if err := s.mayEdit(ctx, before); err != nil {
			return nil, err
		}
		for _, g := range before {
			authorized[g.ID] = g
		}
	}
	topology := slices.ContainsFunc(updates, func(u entities.EntityUpdate) bool { return u.ParentID != nil || u.State != nil })
	out := make([]entities.Entity, len(updates))
	err = s.run(ctx, func(q Querier) error {
		if topology {
			if err := s.lockHierarchy(ctx, q); err != nil {
				return err
			}
		}
		locked, err := s.lock(ctx, q, list)
		if err != nil {
			return err
		}
		if s.channels && !staff {
			for _, g := range locked {
				prior, ok := authorized[g.ID]
				if !ok || prior.Revision != g.Revision || prior.AuthScope != g.AuthScope || prior.State != g.State {
					return ErrConflict
				}
			}
		} else if err := s.mayEdit(ctx, locked); err != nil {
			return err
		}
		current := map[entities.EntityID]entities.Entity{}
		for _, g := range locked {
			current[g.ID] = g
		}
		var rows []updateRow
		var texts []textRow
		var names []nameRow
		var textIDs, nameIDs, reach []string
		var restored []entityRow
		var relabeled []string
		given := map[string][]nameRow{}
		for i, u := range updates {
			g := current[u.ID]
			if g.State == entities.StateMerged {
				return fmt.Errorf("%w: group %s is merged", ErrConflict, g.ID)
			}
			row, moved, err := s.apply(g, u)
			if err != nil {
				return err
			}
			if err := s.checkPolicyBinding(g, entities.Entity{ID: g.ID, Kind: row.Kind, Slug: row.Slug, ScopeID: entities.EntityID(row.ScopeID), State: entities.State(row.State), Internal: row.Internal}); err != nil {
				return err
			}
			row.Ordinal = i
			rows = append(rows, row)
			if moved {
				reach = append(reach, string(g.ID))
			}
			if u.State != nil && *u.State != g.State {
				if *u.State == entities.StateActive {
					tx, ok := q.(pgx.Tx)
					if !ok {
						return errors.New("entities: restore requires a transaction")
					}
					if err := s.checkMediaRestorable(ctx, tx, entities.Ref{Kind: g.Kind, ID: g.ID}); err != nil {
						return err
					}
					restored = append(restored, row.entityRow)
				}
			}
			if row.Slug != g.Slug {
				relabeled = append(relabeled, string(g.ID))
			}
			if u.Names != nil {
				n, err := validateNames(g.ID, u.Names)
				if err != nil {
					return err
				}
				given[string(g.ID)] = n
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
		if _, err := q.Exec(ctx, fmt.Sprintf(`UPDATE %s g SET slug = r.slug, internal = r.internal, scope_id = r.scope_id, parent_id = r.parent_id, state = r.state,
 attributes = coalesce(r.attributes, '{}'), cover = r.cover, avatar = r.avatar, owner_user_id = r.owner_user_id, auth_scope = r.auth_scope, updated_at = now(), revision = g.revision + 1
 FROM jsonb_to_recordset($2::jsonb) AS r(id text, slug text, internal boolean, scope_id text, parent_id text, state text, attributes jsonb, cover jsonb, avatar text, owner_user_id text, auth_scope text)
 WHERE g.tenant_id = $1 AND g.id = r.id`, s.table("content_entities")), s.tenant, data); err != nil {
			return err
		}
		if err := s.checkCaps(ctx, q, restored); err != nil {
			return err
		}

		if len(nameIDs) > 0 {
			stored, err := s.names(ctx, q, nameIDs)
			if err != nil {
				return err
			}
			for _, id := range nameIDs {
				if !sameNames(given[id], stored[entities.EntityID(id)]) && !slices.Contains(relabeled, id) {
					relabeled = append(relabeled, id)
				}
			}
			if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = $1 AND entity_id = ANY($2::text[])`, s.table("content_entity_names")), s.tenant, nameIDs); err != nil {
				return err
			}
			if err := s.insertNames(ctx, q, names); err != nil {
				return err
			}
		}
		if len(textIDs) > 0 {
			if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = $1 AND entity_id = ANY($2::text[])`, s.table("content_entity_text")), s.tenant, textIDs); err != nil {
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
		if err := s.refreshEntityFacets(ctx, q, reach); err != nil {
			return err
		}
		if err := s.namesChanged(ctx, q, relabeled); err != nil {
			return err
		}
		updated, err := s.byIDs(ctx, q, list)
		if err != nil {
			return err
		}
		changes := make([]entities.Change, len(updates))
		for i, u := range updates {
			out[i] = updated[u.ID]
			changes[i] = entities.Change{Type: transition(current[u.ID].State, out[i].State), Kind: out[i].Kind, ID: u.ID}
		}
		if err := s.markDirty(ctx, q, out); err != nil {
			return err
		}
		return s.record(ctx, q, changes)
	})
	if err != nil {
		return nil, err
	}
	if s.channels {
		return s.syncAfter(ctx, out, list)
	}
	return out, nil
}

// updateRow is groupRow plus the state an update writes.
type updateRow struct {
	entityRow
	State string `json:"state"`
}

// apply merges u into g and validates the result; moved reports a change of
// state or parent, which changes what the entity's members are effective in.
func (s *Store) apply(g entities.Entity, u entities.EntityUpdate) (updateRow, bool, error) {
	gk, err := s.entityKind(g.Kind)
	if err != nil {
		return updateRow{}, false, err
	}
	row := updateRow{entityRow: entityRow{ID: string(g.ID), Kind: g.Kind, Slug: g.Slug, Internal: g.Internal, ScopeID: string(g.ScopeID), Attributes: g.Attributes,
		Cover: g.Cover, OwnerUserID: optional(g.OwnerUserID), AuthScope: optional(g.AuthScope)}, State: string(g.State)}
	if g.Avatar != nil {
		row.Avatar = optional(g.Avatar.Image)
	}
	if g.ParentID != "" {
		p := string(g.ParentID)
		row.ParentID = &p
	}
	moved := false
	if u.Internal != nil {
		row.Internal = *u.Internal
		moved = g.Internal != *u.Internal
	}
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
				return row, false, fmt.Errorf("%w: group %s would be its own parent", entities.ErrInvalid, g.ID)
			}
			p := string(*u.ParentID)
			row.ParentID = &p
		}
		moved = moved || string(g.ParentID) != *orEmpty(row.ParentID)
	}
	if u.State != nil {
		if *u.State != entities.StateActive && *u.State != entities.StateDeleted {
			return row, false, fmt.Errorf("%w: state %q is not assignable", entities.ErrInvalid, *u.State)
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
	if s.channels && u.State != nil && *u.State != g.State {
		row.AuthScope = nil
	}
	if err := s.fields(gk, g.Kind, &row.entityRow); err != nil {
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

// DeleteEntities marks entities deleted: they keep their memberships and code but
// are effective nowhere, listed nowhere and lose their documents.
func (s *Store) DeleteEntities(ctx context.Context, ids []entities.EntityID) ([]entities.Entity, error) {
	deleted := entities.StateDeleted
	updates := make([]entities.EntityUpdate, len(ids))
	for i, id := range ids {
		updates[i] = entities.EntityUpdate{ID: id, State: &deleted}
	}
	return s.UpdateEntities(ctx, updates)
}
