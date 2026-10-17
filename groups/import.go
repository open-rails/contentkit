package groups

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/internal/codes"
)

// ImportGroup is a group as the bulk import writes it: its id kept. Nil
// Names, Text and Edges keep what is stored; given ones replace it.
type ImportGroup struct {
	ID   GroupID
	Kind string
	Slug string
	// Internal preserves the stored classification when omitted.
	Internal    *bool
	ScopeID     GroupID
	ParentID    GroupID
	State       State
	Attributes  map[string]any
	Cover       *Cover
	Avatar      *Avatar
	OwnerUserID string
	// AuthScope names the group's AuthKit scope; empty keeps the stored one.
	AuthScope string
	Names     []Name
	Text      []Text
	// Edges are the group's edges to others (From is the group).
	Edges     []Edge
	Code      string
	CreatedAt *time.Time
	UpdatedAt *time.Time
}

type importRow struct {
	groupRow
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

// ImportGroups writes groups in q: new ones are inserted, stored ones updated
// where the import changes them. Codes, search documents and the facets of
// the content a moved or deleted group reaches follow. A merged group is
// ErrConflict; a taken slug too. An active group of an auth_scope kind keeps
// the AuthKit scope it names or has; one with neither is provisioned, by the
// scope sync job, only with provision (else ErrInvalid).
func (s *Store) ImportGroups(ctx context.Context, q pgx.Tx, in []ImportGroup, provision bool) error {
	if len(in) == 0 {
		return nil
	}
	if s.references != nil {
		if err := checkReferenceTransaction(ctx, q); err != nil {
			return err
		}
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
			return fmt.Errorf("%w: group %s is imported twice", ErrInvalid, row.ID)
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
				return fmt.Errorf("%w: code %q is not a content code", ErrInvalid, g.Code)
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
	return mapPGError(s.importGroups(ctx, q, in, ids, rows, names, texts, edges, keep, provision))
}

func (s *Store) importGroups(ctx context.Context, q pgx.Tx, in []ImportGroup, ids []string, rows []importRow, names map[string][]nameRow,
	texts map[string][]textRow, edges map[string][]edgeRow, keep map[codes.Key]string, provision bool) error {
	stored := map[string]Group{}
	res, err := q.Query(ctx, fmt.Sprintf(`SELECT %s FROM %s g WHERE g.tenant_id = $1 AND g.id = ANY($2::text[]) ORDER BY g.id FOR UPDATE`,
		groupColumns, s.table("content_groups")), s.tenant, ids)
	if err != nil {
		return err
	}
	found, err := scanGroups(res)
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
		if !provision && r.State == string(StateActive) && r.AuthScope == nil && s.cfg.GroupKinds[r.Kind].AuthScope && (!ok || g.AuthScope == "") {
			return fmt.Errorf("%w: group %s needs its auth_scope, or the import provisions scopes", ErrInvalid, r.ID)
		}
		if !ok {
			continue
		}
		if in[i].Internal == nil {
			r.Internal = g.Internal
		}
		if err := s.checkPolicyBinding(g, Group{ID: g.ID, Kind: r.Kind, Slug: r.Slug, ScopeID: GroupID(r.ScopeID), State: State(r.State), Internal: r.Internal}); err != nil {
			return err
		}
		switch {
		case g.Kind != r.Kind:
			return fmt.Errorf("%w: group %s is a %s, not a %s", ErrInvalid, r.ID, g.Kind, r.Kind)
		case g.State == StateMerged:
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
  avatar = EXCLUDED.avatar, owner_user_id = EXCLUDED.owner_user_id, auth_scope = coalesce(EXCLUDED.auth_scope, g.auth_scope),
  created_at = EXCLUDED.created_at, updated_at = EXCLUDED.updated_at, revision = g.revision + 1
 WHERE (g.slug, g.internal, g.scope_id, g.parent_id, g.state, g.attributes, g.avatar, g.owner_user_id, g.auth_scope, g.created_at, g.cover)
  IS DISTINCT FROM (EXCLUDED.slug, EXCLUDED.internal, EXCLUDED.scope_id, EXCLUDED.parent_id, EXCLUDED.state, EXCLUDED.attributes, EXCLUDED.avatar, EXCLUDED.owner_user_id,
  coalesce(EXCLUDED.auth_scope, g.auth_scope), EXCLUDED.created_at, CASE WHEN EXCLUDED.cover IS NULL AND g.cover ? 'auto' THEN g.cover ELSE EXCLUDED.cover END)
 RETURNING g.id`, s.table("content_groups")), s.tenant, string(data))
	if err != nil {
		return err
	}
	changedIDs, err := collectStrings(got)
	if err != nil {
		return err
	}
	// Check after the upsert: a concurrent creator may have inserted an
	// initially absent group and bound host references before ON CONFLICT
	// acquires its lock. The callback sees those committed links and can roll
	// this state change back in the caller's transaction.
	if s.references != nil {
		persisted, err := s.byIDs(ctx, q, ids)
		if err != nil {
			return err
		}
		var unavailable []Ref
		for _, id := range ids {
			g := persisted[GroupID(id)]
			if g.State == StateDeleted || g.Internal {
				unavailable = append(unavailable, Ref{Kind: g.Kind, ID: g.ID})
			}
		}
		if len(unavailable) > 0 {
			if err := s.references.CheckUnavailable(ctx, q, s.tenant, unavailable); err != nil {
				return err
			}
		}
	}
	changed := map[string]bool{}
	for _, id := range changedIDs {
		changed[id] = true
	}
	if err := s.replaceNames(ctx, q, names, changed); err != nil {
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
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	var list []string
	var renamed []Ref
	for i, id := range ids {
		if changed[id] {
			list, renamed = append(list, id), append(renamed, Ref{Kind: rows[i].Kind, ID: GroupID(id)})
		}
	}
	if len(list) == 0 {
		return nil
	}
	if err := s.resyncSold(ctx, q, renamed); err != nil {
		return err
	}
	if err := s.syncCodes(ctx, q, list); err != nil {
		return err
	}
	if err := s.refreshGroupFacets(ctx, q, moved); err != nil {
		return err
	}
	written, err := s.byIDs(ctx, q, list)
	if err != nil {
		return err
	}
	marks := make([]Group, 0, len(written))
	var exposed []Ref
	for _, g := range written {
		marks = append(marks, g)
		if previous, exists := stored[string(g.ID)]; exists && previous.Internal != g.Internal {
			exposed = append(exposed, Ref{Kind: g.Kind, ID: g.ID})
		}
	}
	if s.folders != nil && len(exposed) > 0 {
		if err := s.folders.ExposeTx(ctx, q, exposed); err != nil {
			return err
		}
	}
	if err := s.markDirty(ctx, q, marks); err != nil {
		return err
	}
	// A deleted group's scope is retired, an unscoped one provisioned, by the job.
	var sync []string
	for _, g := range marks {
		if g.State != StateActive && g.AuthScope != "" || provision && slices.Contains(s.unscoped([]Group{g}), string(g.ID)) {
			sync = append(sync, string(g.ID))
		}
	}
	return s.queueSync(ctx, q, sync)
}

// checkImport validates one group's own fields against its kind.
func (s *Store) checkImport(g *ImportGroup) (importRow, error) {
	kind := strings.TrimSpace(g.Kind)
	gk, err := s.groupKind(kind)
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
		g.State = StateActive
	case StateActive, StateDeleted:
	default:
		return importRow{}, fmt.Errorf("%w: state %q is not importable", ErrInvalid, g.State)
	}
	row := importRow{groupRow: groupRow{ID: string(g.ID), Kind: kind, Slug: slug, Attributes: g.Attributes,
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
			return row, fmt.Errorf("%w: group %s would be its own parent", ErrInvalid, g.ID)
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
	if err := s.fields(gk, kind, &row.groupRow); err != nil {
		return row, err
	}
	return row, nil
}

// replaceNames replaces the names of groups whose given names differ from
// the stored ones, marking them changed.
func (s *Store) replaceNames(ctx context.Context, q Querier, given map[string][]nameRow, changed map[string]bool) error {
	if len(given) == 0 {
		return nil
	}
	ids := make([]string, 0, len(given))
	for id := range given {
		ids = append(ids, id)
	}
	stored, err := s.names(ctx, q, ids)
	if err != nil {
		return err
	}
	type name struct{ language, kind, name string }
	var replace []string
	var rows []nameRow
	for _, id := range ids {
		var want, have []name
		for _, r := range given[id] {
			want = append(want, name{r.Language, r.Kind, r.Name})
		}
		for _, n := range stored[GroupID(id)] {
			have = append(have, name{n.Language, string(n.Kind), n.Name})
		}
		cmp := func(a, b name) int {
			return strings.Compare(a.language+"\x00"+a.kind+"\x00"+a.name, b.language+"\x00"+b.kind+"\x00"+b.name)
		}
		slices.SortFunc(want, cmp)
		slices.SortFunc(have, cmp)
		if slices.Equal(slices.Compact(want), have) {
			continue
		}
		replace, changed[id] = append(replace, id), true
		for _, r := range given[id] {
			r.Ordinal = len(rows)
			rows = append(rows, r)
		}
	}
	if len(replace) == 0 {
		return nil
	}
	if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = $1 AND group_id = ANY($2::text[])`, s.table("content_group_names")), s.tenant, replace); err != nil {
		return err
	}
	return s.insertNames(ctx, q, rows)
}

// replaceText replaces the text of groups whose given text differs from the
// stored one, marking them changed.
func (s *Store) replaceText(ctx context.Context, q Querier, given map[string][]textRow, changed map[string]bool) error {
	if len(given) == 0 {
		return nil
	}
	ids := make([]string, 0, len(given))
	for id := range given {
		ids = append(ids, id)
	}
	res, err := q.Query(ctx, fmt.Sprintf(`SELECT group_id, language, description, sort_key FROM %s WHERE tenant_id = $1 AND group_id = ANY($2::text[])`,
		s.table("content_group_text")), s.tenant, ids)
	if err != nil {
		return err
	}
	stored := map[string][]textRow{}
	for res.Next() {
		var r textRow
		if err := res.Scan(&r.GroupID, &r.Language, &r.Description, &r.SortKey); err != nil {
			res.Close()
			return err
		}
		stored[r.GroupID] = append(stored[r.GroupID], r)
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
	if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE tenant_id = $1 AND group_id = ANY($2::text[])`, s.table("content_group_text")), s.tenant, replace); err != nil {
		return err
	}
	return s.insertText(ctx, q, rows)
}

// replaceEdges sets each group's edges to others to the given ones.
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
		s.table("content_group_edges")), s.tenant, from, string(data)); err != nil {
		return err
	}
	if len(all) == 0 {
		return nil
	}
	edges := make([]Edge, len(all))
	for i, r := range all {
		edges[i] = Edge{From: GroupID(r.From), Relation: r.Relation, To: GroupID(r.To)}
	}
	return s.bound(q).AddEdges(ctx, edges)
}

// ImportMemberships replaces, in q, each content's memberships in the group
// kinds its set names (SetContentGroups); a set equal to what is stored
// writes nothing.
func (s *Store) ImportMemberships(ctx context.Context, q pgx.Tx, sets []ContentGroups) error {
	var contentIDs []string
	for _, set := range sets {
		if len(set.Kinds) == 0 {
			return fmt.Errorf("%w: an imported membership set names its group kinds", ErrInvalid)
		}
		contentIDs = append(contentIDs, set.ContentID)
	}
	if len(sets) == 0 {
		return nil
	}
	b := s.bound(q)
	stored, err := b.MembershipsOf(ctx, contentIDs)
	if err != nil {
		return err
	}
	var groupIDs []string
	for _, ms := range stored {
		for _, m := range ms {
			groupIDs = append(groupIDs, string(m.GroupID))
		}
	}
	for _, set := range sets {
		for _, m := range set.Memberships {
			groupIDs = append(groupIDs, string(m.GroupID))
		}
	}
	f, err := s.facts(ctx, q, slices.Compact(slices.Sorted(slices.Values(groupIDs))), nil, nil)
	if err != nil {
		return err
	}
	var write []ContentGroups
	for _, set := range sets {
		if !s.sameMemberships(f, set, stored[set.ContentID]) {
			write = append(write, set)
		}
	}
	return mapPGError(b.SetContentGroups(ctx, write))
}

// sameMemberships reports whether set leaves the stored memberships as they
// are: the same ones in its kinds, positions omitted being kept.
func (s *Store) sameMemberships(f facts, set ContentGroups, stored []Membership) bool {
	key := func(m Membership) string {
		return string(m.GroupID) + "\x00" + m.VersionID + "\x00" + strings.TrimSpace(m.Role)
	}
	have := map[string]Membership{}
	for _, m := range stored {
		if g, ok := f.groups[string(m.GroupID)]; ok && slices.Contains(set.Kinds, g.Kind) {
			have[key(m)] = m
		}
	}
	if len(have) != len(set.Memberships) {
		return false
	}
	for _, m := range set.Memberships {
		h, ok := have[key(m)]
		state := m.State
		if state == "" {
			state = MembershipActive
		}
		if !ok || h.State != state || !sameTime(h.ActiveFrom, m.ActiveFrom) || !sameTime(h.ActiveUntil, m.ActiveUntil) ||
			m.Position != nil && (h.Position == nil || *h.Position != *m.Position) {
			return false
		}
	}
	return true
}

func sameTime(a, b *time.Time) bool { return (a == nil) == (b == nil) && (a == nil || a.Equal(*b)) }

// ImportDeletes deletes the active groups among ids in q, as DeleteGroups
// does; deleted and merged ones stay as they are.
func (s *Store) ImportDeletes(ctx context.Context, q pgx.Tx, ids []GroupID) error {
	if len(ids) == 0 {
		return nil
	}
	list, err := uniqueIDs(ids)
	if err != nil {
		return err
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT id FROM %s WHERE tenant_id = $1 AND id = ANY($2::text[]) AND state = 'active' ORDER BY id`,
		s.table("content_groups")), s.tenant, list)
	if err != nil {
		return err
	}
	active, err := pgx.CollectRows(rows, pgx.RowTo[GroupID])
	if err != nil || len(active) == 0 {
		return err
	}
	_, err = s.bound(q).DeleteGroups(ctx, active)
	return err
}
