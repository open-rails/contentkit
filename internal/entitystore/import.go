package entitystore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	entities "github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/codes"
)

type importRow struct {
	entityRow
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// bound is the store running in tx, the import's.
func (s *Store) bound(tx pgx.Tx) *Store {
	c := *s
	c.tx = tx
	return &c
}

// ImportEntities writes entities in q: new ones are inserted, stored ones updated
// where the import changes them. Codes, search documents and the facets of
// the content a moved or deleted entity reaches follow. A merged entity is
// ErrConflict; a taken slug too. An active entity of an auth_scope kind keeps
// the AuthKit scope it names or has; one with neither is provisioned, by the
// caller after commit with provision (else ErrInvalid).
func (s *Store) ImportEntities(ctx context.Context, q pgx.Tx, in []entities.ImportEntity, provision bool) error {
	if len(in) == 0 {
		return nil
	}
	now := time.Now()
	ids := make([]string, 0, len(in))
	rows := make([]importRow, 0, len(in))
	names := map[string][]nameRow{}
	texts := map[string][]textRow{}
	edges := map[string][]edgeRow{}
	keep := map[codes.Key]string{}
	for i, g := range in {
		row, err := s.checkImport(&g)
		if err != nil {
			return err
		}
		if slices.Contains(ids, row.ID) {
			return fmt.Errorf("%w: group %s is imported twice", entities.ErrInvalid, row.ID)
		}
		row.Ordinal = i
		ids = append(ids, row.ID)
		rows = append(rows, row)
		if g.Names != nil {
			if names[row.ID], err = validateNames(g.ID, g.Names); err != nil {
				return err
			}
		}
		if g.Text != nil {
			if texts[row.ID], err = validateText(g.ID, g.Text); err != nil {
				return err
			}
		}
		if g.Edges != nil {
			for j := range g.Edges {
				g.Edges[j].From = g.ID
			}
			if edges[row.ID], err = validateEdges(g.Edges); err != nil {
				return err
			}
		}
		if g.Code != "" {
			if !codes.ValidCode(g.Code) {
				return fmt.Errorf("%w: code %q is not a content code", entities.ErrInvalid, g.Code)
			}
			keep[codes.Key{Kind: row.Kind, ID: row.ID}] = g.Code
		}
		rows[i].CreatedAt, rows[i].UpdatedAt = now, now
		if g.CreatedAt != nil {
			rows[i].CreatedAt = *g.CreatedAt
		}
		if g.UpdatedAt != nil {
			rows[i].UpdatedAt = *g.UpdatedAt
		} else if g.CreatedAt != nil {
			rows[i].UpdatedAt = *g.CreatedAt
		}
	}
	return mapPGError(s.importEntities(ctx, q, in, ids, rows, names, texts, edges, keep, provision))
}

func (s *Store) importEntities(ctx context.Context, q pgx.Tx, in []entities.ImportEntity, ids []string, rows []importRow, names map[string][]nameRow,
	texts map[string][]textRow, edges map[string][]edgeRow, keep map[codes.Key]string, provision bool) error {
	if err := s.lockHierarchy(ctx, q); err != nil {
		return err
	}
	stored := map[string]entities.Entity{}
	res, err := q.Query(ctx, fmt.Sprintf(`SELECT %s FROM %s g WHERE g.tenant_id = $1 AND g.id = ANY($2::text[]) ORDER BY g.id FOR UPDATE`,
		entityColumns, s.table("content_entities")), s.tenant, ids)
	if err != nil {
		return err
	}
	found, err := scanEntities(res)
	if err != nil {
		return err
	}
	for _, g := range found {
		stored[string(g.ID)] = g
	}
	var moved []string
	for i := range rows {
		r := &rows[i]
		g, ok := stored[r.ID]
		if !provision && r.State == string(entities.StateActive) && r.AuthScope == nil && s.cfg.EntityKinds[r.Kind].AuthScope && (!ok || g.AuthScope == "") {
			return fmt.Errorf("%w: group %s needs its auth_scope, or the import provisions scopes", entities.ErrInvalid, r.ID)
		}
		if !ok {
			continue
		}
		if in[i].Internal == nil {
			r.Internal = g.Internal
		}
		if err := s.checkPolicyBinding(g, entities.Entity{ID: g.ID, Kind: r.Kind, Slug: r.Slug, ScopeID: entities.EntityID(r.ScopeID), State: entities.State(r.State), Internal: r.Internal}); err != nil {
			return err
		}
		switch {
		case g.Kind != r.Kind:
			return fmt.Errorf("%w: group %s is a %s, not a %s", entities.ErrInvalid, r.ID, g.Kind, r.Kind)
		case g.State == entities.StateMerged:
			return fmt.Errorf("%w: group %s is merged into %s", ErrConflict, r.ID, g.MergedInto)
		}
		if in[i].CreatedAt == nil {
			r.CreatedAt = g.CreatedAt
		}
		if string(g.State) != r.State || string(g.ParentID) != *orEmpty(r.ParentID) || g.Internal != r.Internal {
			moved = append(moved, r.ID)
		}
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	// An automatic cover stays while the import selects none.
	got, err := q.Query(ctx, fmt.Sprintf(`INSERT INTO %s AS g (tenant_id, id, kind, slug, internal, scope_id, parent_id, state, attributes, cover, avatar,
 owner_user_id, auth_scope, created_at, updated_at)
 SELECT $1, r.id, r.kind, r.slug, r.internal, r.scope_id, r.parent_id, r.state, coalesce(r.attributes, '{}'), r.cover, r.avatar, r.owner_user_id, r.auth_scope,
  r.created_at, r.updated_at
 FROM jsonb_to_recordset($2::jsonb) AS r(ordinal int, id text, kind text, slug text, internal boolean, scope_id text, parent_id text, state text, attributes jsonb, cover jsonb,
  avatar text, owner_user_id text, auth_scope text, created_at timestamptz, updated_at timestamptz)
 ORDER BY r.ordinal
 ON CONFLICT (tenant_id, id) DO UPDATE SET slug = EXCLUDED.slug, internal = EXCLUDED.internal, scope_id = EXCLUDED.scope_id, parent_id = EXCLUDED.parent_id, state = EXCLUDED.state,
  attributes = EXCLUDED.attributes, cover = CASE WHEN EXCLUDED.cover IS NULL AND g.cover ? 'auto' THEN g.cover ELSE EXCLUDED.cover END,
  avatar = EXCLUDED.avatar, owner_user_id = EXCLUDED.owner_user_id, auth_scope = CASE WHEN g.kind='channel' AND g.state IS DISTINCT FROM EXCLUDED.state THEN NULL ELSE coalesce(EXCLUDED.auth_scope,g.auth_scope) END,
  created_at = EXCLUDED.created_at, updated_at = EXCLUDED.updated_at, revision = g.revision + 1
 WHERE (g.slug, g.internal, g.scope_id, g.parent_id, g.state, g.attributes, g.avatar, g.owner_user_id, g.auth_scope, g.created_at, g.cover)
  IS DISTINCT FROM (EXCLUDED.slug, EXCLUDED.internal, EXCLUDED.scope_id, EXCLUDED.parent_id, EXCLUDED.state, EXCLUDED.attributes, EXCLUDED.avatar, EXCLUDED.owner_user_id,
  coalesce(EXCLUDED.auth_scope, g.auth_scope), EXCLUDED.created_at, CASE WHEN EXCLUDED.cover IS NULL AND g.cover ? 'auto' THEN g.cover ELSE EXCLUDED.cover END)
 RETURNING g.id`, s.table("content_entities")), s.tenant, string(data))
	if err != nil {
		return err
	}
	changedIDs, err := collectStrings(got)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.State == string(entities.StateActive) {
			if err := s.checkMediaRestorable(ctx, q, entities.Ref{Kind: r.Kind, ID: entities.EntityID(r.ID)}); err != nil {
				return err
			}
		}
	}
	changed := map[string]bool{}
	for _, id := range changedIDs {
		changed[id] = true
	}
	relabeled, err := s.replaceNames(ctx, q, names, changed)
	if err != nil {
		return err
	}
	if err := s.replaceText(ctx, q, texts, changed); err != nil {
		return err
	}
	if err := s.checkLinks(ctx, q, ids); err != nil {
		return err
	}
	if err := s.replaceEdges(ctx, q, edges); err != nil {
		return err
	}
	if err := codes.Reserve(ctx, codes.Pgx(q), s.qs, s.tenant, keep); err != nil {
		if errors.Is(err, codes.ErrConflict) {
			return fmt.Errorf("%w: %w", ErrConflict, err)
		}
		return fmt.Errorf("%w: %w", entities.ErrInvalid, err)
	}
	var list []string

	for i, id := range ids {
		if changed[id] {
			list = append(list, id)
		}
		if g, ok := stored[id]; ok && g.Slug != rows[i].Slug {
			relabeled = append(relabeled, id)
		}
	}
	if len(list) == 0 {
		return nil
	}
	if err := s.syncCodes(ctx, q, list); err != nil {
		return err
	}
	if err := s.refreshEntityFacets(ctx, q, moved); err != nil {
		return err
	}
	if err := s.namesChanged(ctx, q, relabeled); err != nil {
		return err
	}
	written, err := s.byIDs(ctx, q, list)
	if err != nil {
		return err
	}
	marks := make([]entities.Entity, 0, len(written))
	changes := make([]entities.Change, 0, len(list))
	for _, id := range list {
		g, ok := written[entities.EntityID(id)]
		if !ok {
			continue
		}
		marks = append(marks, g)
		typ := entities.EventCreated
		if before, ok := stored[id]; ok {
			typ = transition(before.State, g.State)
		}
		changes = append(changes, entities.Change{Type: typ, Kind: g.Kind, ID: g.ID})
	}
	if err := s.markDirty(ctx, q, marks); err != nil {
		return err
	}
	return s.record(ctx, q, changes)
}

// checkImport validates one entity's own fields against its kind.
func (s *Store) checkImport(g *entities.ImportEntity) (importRow, error) {
	if !s.channels && (g.OwnerUserID != "" || g.AuthScope != "") {
		return importRow{}, fmt.Errorf("%w: metadata has no authoring owner or scope", entities.ErrInvalid)
	}
	kind := strings.TrimSpace(g.Kind)
	gk, err := s.entityKind(kind)
	if err != nil {
		return importRow{}, err
	}
	if err := validateID(g.ID); err != nil {
		return importRow{}, err
	}
	slug, err := validateSlug(g.Slug)
	if err != nil {
		return importRow{}, err
	}
	switch g.State {
	case "":
		g.State = entities.StateActive
	case entities.StateActive, entities.StateDeleted:
	default:
		return importRow{}, fmt.Errorf("%w: state %q is not importable", entities.ErrInvalid, g.State)
	}
	row := importRow{entityRow: entityRow{ID: string(g.ID), Kind: kind, Slug: slug, Attributes: g.Attributes,
		OwnerUserID: optional(g.OwnerUserID), AuthScope: optional(g.AuthScope)}, State: string(g.State)}
	if g.Internal != nil {
		row.Internal = *g.Internal
	}
	if g.ScopeID != "" {
		if err := validateID(g.ScopeID); err != nil {
			return row, err
		}
		row.ScopeID = string(g.ScopeID)
	}
	if g.ParentID != "" {
		if err := validateID(g.ParentID); err != nil {
			return row, err
		}
		if g.ParentID == g.ID {
			return row, fmt.Errorf("%w: group %s would be its own parent", entities.ErrInvalid, g.ID)
		}
		p := string(g.ParentID)
		row.ParentID = &p
	}
	if row.Cover, err = validateCover(g.Cover); err != nil {
		return row, err
	}
	if row.Avatar, err = validateAvatar(g.Avatar); err != nil {
		return row, err
	}
	if err := s.fields(gk, kind, &row.entityRow); err != nil {
		return row, err
	}
	return row, nil
}

// replaceNames replaces the names of entities whose given names differ from
// the stored ones, marking them changed, and returns those entities.
func (s *Store) replaceNames(ctx context.Context, q Querier, given map[string][]nameRow, changed map[string]bool) ([]string, error) {
	if len(given) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(given))
	for id := range given {
		ids = append(ids, id)
	}
	stored, err := s.names(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	var replace []string
	var rows []nameRow
	for _, id := range ids {
		if sameNames(given[id], stored[entities.EntityID(id)]) {
			continue
		}
		replace, changed[id] = append(replace, id), true
		for _, r := range given[id] {
			r.Ordinal = len(rows)
			rows = append(rows, r)
		}
	}
	if len(replace) == 0 {
		return nil, nil
	}
	if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = $1 AND entity_id = ANY($2::text[])`, s.table("content_entity_names")), s.tenant, replace); err != nil {
		return nil, err
	}
	return replace, s.insertNames(ctx, q, rows)
}

// sameNames reports whether given names, duplicates aside, are the stored
// ones.
func sameNames(given []nameRow, stored []entities.Name) bool {
	type name struct{ language, kind, name string }
	var want, have []name
	for _, r := range given {
		want = append(want, name{r.Language, r.Kind, r.Name})
	}
	for _, n := range stored {
		have = append(have, name{n.Language, string(n.Kind), n.Name})
	}
	cmp := func(a, b name) int {
		return strings.Compare(a.language+"\x00"+a.kind+"\x00"+a.name, b.language+"\x00"+b.kind+"\x00"+b.name)
	}
	slices.SortFunc(want, cmp)
	slices.SortFunc(have, cmp)
	return slices.Equal(slices.Compact(want), have)
}

// replaceText replaces the text of entities whose given text differs from the
// stored one, marking them changed.
func (s *Store) replaceText(ctx context.Context, q Querier, given map[string][]textRow, changed map[string]bool) error {
	if len(given) == 0 {
		return nil
	}
	ids := make([]string, 0, len(given))
	for id := range given {
		ids = append(ids, id)
	}
	res, err := q.Query(ctx, fmt.Sprintf(`SELECT entity_id, language, description, sort_key FROM %s WHERE tenant_id = $1 AND entity_id = ANY($2::text[])`,
		s.table("content_entity_text")), s.tenant, ids)
	if err != nil {
		return err
	}
	stored := map[string][]textRow{}
	for res.Next() {
		var r textRow
		if err := res.Scan(&r.EntityID, &r.Language, &r.Description, &r.SortKey); err != nil {
			res.Close()
			return err
		}
		stored[r.EntityID] = append(stored[r.EntityID], r)
	}
	res.Close()
	if err := res.Err(); err != nil {
		return err
	}
	cmp := func(a, b textRow) int { return strings.Compare(a.Language, b.Language) }
	var replace []string
	var rows []textRow
	for _, id := range ids {
		want, have := slices.Clone(given[id]), stored[id]
		slices.SortFunc(want, cmp)
		slices.SortFunc(have, cmp)
		if slices.Equal(want, have) {
			continue
		}
		replace, changed[id] = append(replace, id), true
		rows = append(rows, given[id]...)
	}
	if len(replace) == 0 {
		return nil
	}
	if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = $1 AND entity_id = ANY($2::text[])`, s.table("content_entity_text")), s.tenant, replace); err != nil {
		return err
	}
	return s.insertText(ctx, q, rows)
}

// replaceEdges sets each entity's edges to others to the given ones.
func (s *Store) replaceEdges(ctx context.Context, q pgx.Tx, given map[string][]edgeRow) error {
	if len(given) == 0 {
		return nil
	}
	var from []string
	all := []edgeRow{}
	for id, rows := range given {
		from = append(from, id)
		all = append(all, rows...)
	}
	data, err := json.Marshal(all)
	if err != nil {
		return err
	}
	if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s e WHERE e.tenant_id = $1 AND e.from_id = ANY($2::text[]) AND NOT EXISTS (
 SELECT 1 FROM jsonb_to_recordset($3::jsonb) AS r("from" text, relation text, "to" text) WHERE r."from" = e.from_id AND r.relation = e.relation AND r."to" = e.to_id)`,
		s.table("content_entity_edges")), s.tenant, from, string(data)); err != nil {
		return err
	}
	if len(all) == 0 {
		return nil
	}
	edges := make([]entities.Edge, len(all))
	for i, r := range all {
		edges[i] = entities.Edge{From: entities.EntityID(r.From), Relation: r.Relation, To: entities.EntityID(r.To)}
	}
	return s.bound(q).AddEdges(ctx, edges)
}

// ImportMemberships replaces, in q, each content's memberships in the entity
// kinds its set names (SetContentEntities); a set equal to what is stored
// writes nothing.
func (s *Store) ImportMemberships(ctx context.Context, q pgx.Tx, sets []entities.ContentEntities) error {
	var contentIDs []string
	for _, set := range sets {
		if len(set.Kinds) == 0 {
			return fmt.Errorf("%w: an imported membership set names its group kinds", entities.ErrInvalid)
		}
		contentIDs = append(contentIDs, set.ContentID)
	}
	if len(sets) == 0 {
		return nil
	}
	if err := s.LockHierarchyShared(ctx, q); err != nil {
		return err
	}
	b := s.bound(q)
	stored, err := b.MembershipsOf(ctx, contentIDs)
	if err != nil {
		return err
	}
	var entityIDs []string
	for _, ms := range stored {
		for _, m := range ms {
			entityIDs = append(entityIDs, string(m.EntityID))
		}
	}
	for _, set := range sets {
		for _, m := range set.Memberships {
			entityIDs = append(entityIDs, string(m.EntityID))
		}
	}
	f, err := s.facts(ctx, q, slices.Compact(slices.Sorted(slices.Values(entityIDs))), nil, nil)
	if err != nil {
		return err
	}
	var write []entities.ContentEntities
	for _, set := range sets {
		if !s.sameMemberships(f, set, stored[set.ContentID]) {
			write = append(write, set)
		}
	}
	return mapPGError(b.SetContentEntities(ctx, write))
}

// sameMemberships reports whether set leaves the stored memberships as they
// are: the same ones in its kinds, positions omitted being kept.
func (s *Store) sameMemberships(f facts, set entities.ContentEntities, stored []entities.Membership) bool {
	key := func(m entities.Membership) string {
		return string(m.EntityID) + "\x00" + m.VersionID + "\x00" + strings.TrimSpace(m.Role)
	}
	have := map[string]entities.Membership{}
	for _, m := range stored {
		if g, ok := f.entities[string(m.EntityID)]; ok && slices.Contains(set.Kinds, g.Kind) {
			have[key(m)] = m
		}
	}
	// Duplicates collapse as the write keeps them: the last one wins.
	want := map[string]entities.Membership{}
	for _, m := range set.Memberships {
		want[key(m)] = m
	}
	if len(have) != len(want) {
		return false
	}
	for k, m := range want {
		h, ok := have[k]
		state := m.State
		if state == "" {
			state = entities.MembershipActive
		}
		if !ok || h.State != state ||
			m.Position != nil && (h.Position == nil || *h.Position != *m.Position) {
			return false
		}
	}
	return true
}

// ImportDeletes deletes the active entities among ids in q, as DeleteEntities
// does; deleted and merged ones stay as they are.
func (s *Store) ImportDeletes(ctx context.Context, q pgx.Tx, ids []entities.EntityID) error {
	if len(ids) == 0 {
		return nil
	}
	list, err := uniqueIDs(ids)
	if err != nil {
		return err
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT id FROM %s WHERE tenant_id = $1 AND id = ANY($2::text[]) AND state = 'active' ORDER BY id`,
		s.table("content_entities")), s.tenant, list)
	if err != nil {
		return err
	}
	active, err := pgx.CollectRows(rows, pgx.RowTo[entities.EntityID])
	if err != nil || len(active) == 0 {
		return err
	}
	_, err = s.bound(q).DeleteEntities(ctx, active)
	return err
}
