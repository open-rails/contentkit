package entities

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/kinds"
)

// MembershipState: a proposed membership (a suggestion awaiting review) is
// stored but effective nowhere until it is accepted.
type MembershipState string

const (
	MembershipActive   MembershipState = "active"
	MembershipProposed MembershipState = "proposed"
)

// Membership places content, or one version of it, in an entity. Position
// orders an ordered entity (appended when omitted); the window bounds when a
// windowed membership (a featured rail) is effective.
type Membership struct {
	EntityID    EntityID        `json:"group_id,omitempty"`
	ContentID   string          `json:"content_id,omitempty"`
	VersionID   string          `json:"version_id,omitempty"`
	Role        string          `json:"role,omitempty"`
	Position    *int            `json:"position,omitempty"`
	ActiveFrom  *time.Time      `json:"active_from,omitempty"`
	ActiveUntil *time.Time      `json:"active_until,omitempty"`
	State       MembershipState `json:"state,omitempty"`
}

// ContentEntities is one content's memberships in the entity kinds Kinds names.
type ContentEntities struct {
	// ContentID names the content; routes take it from the path.
	ContentID string `json:"-"`
	// VersionScope limits replacement to one ownership scope. Nil preserves
	// the legacy all-version replacement; empty means shared content links;
	// otherwise it names the one version whose links may change.
	VersionScope *string `json:"-"`
	// Kinds limits the write to these entity kinds; empty means every entity
	// kind the content's kind declares.
	Kinds       []string     `json:"kinds,omitempty"`
	Memberships []Membership `json:"memberships"`
	// Create links these names too, creating entities of creatable kinds that
	// no active entity names yet, in the same transaction.
	Create []NewEntity `json:"create,omitempty"`
}

// MemberRef names a member of an entity: content, or one version of it.
type MemberRef struct {
	ContentID string `json:"content_id"`
	VersionID string `json:"version_id,omitempty"`
}

// Order sets the member order of one ordered entity.
type Order struct {
	// EntityID names the entity; routes take it from the path.
	EntityID EntityID    `json:"-"`
	Members  []MemberRef `json:"members"`
}

type memberKey struct{ entity, content, version, role string }

type memberRow struct {
	EntityID    string     `json:"entity_id"`
	ContentID   string     `json:"content_id"`
	VersionID   *string    `json:"version_id"`
	Role        string     `json:"role"`
	Position    *int       `json:"position"`
	ActiveFrom  *time.Time `json:"active_from"`
	ActiveUntil *time.Time `json:"active_until"`
	State       string     `json:"state"`
}

func (r memberRow) key() memberKey {
	v := ""
	if r.VersionID != nil {
		v = *r.VersionID
	}
	return memberKey{r.EntityID, r.ContentID, v, r.Role}
}

// facts are what validation reads about the entities, contents and versions a
// batch names.
type facts struct {
	entities map[string]Entity
	contents map[string]string // content id -> content kind
	versions map[string]string // version id -> content id
}

func (s *Store) facts(ctx context.Context, q Querier, entityIDs, contentIDs, versionIDs []string) (facts, error) {
	f := facts{entities: map[string]Entity{}, contents: map[string]string{}, versions: map[string]string{}}
	if len(entityIDs) > 0 {
		// A share lock keeps a concurrent merge or delete from moving the entities.
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT %s FROM %s g WHERE g.tenant_id = $1 AND g.id = ANY($2::text[]) ORDER BY g.id FOR SHARE`,
			entityColumns, s.table("content_entities")), s.tenant, entityIDs)
		if err != nil {
			return f, err
		}
		found, err := scanEntities(rows)
		if err != nil {
			return f, err
		}
		for _, g := range found {
			f.entities[string(g.ID)] = g
		}
	}
	read := func(sql string, ids []string, into map[string]string) error {
		if len(ids) == 0 {
			return nil
		}
		rows, err := q.Query(ctx, sql, s.tenant, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				return err
			}
			into[k] = v
		}
		return rows.Err()
	}
	if err := read(fmt.Sprintf(`SELECT id, kind FROM %s WHERE tenant_id = $1 AND id = ANY($2::text[])`, s.table("content_items")), contentIDs, f.contents); err != nil {
		return f, err
	}
	err := read(fmt.Sprintf(`SELECT id, content_id FROM %s WHERE tenant_id = $1 AND id = ANY($2::text[])`, s.table("content_versions")), versionIDs, f.versions)
	return f, err
}

// prepare validates memberships against the kinds config and the stored
// entities, contents and versions, and resolves omitted positions: an existing
// membership keeps its own, a new one in an ordered entity is appended.
func (s *Store) prepare(ctx context.Context, q Querier, ms []Membership) ([]memberRow, facts, error) {
	var entityIDs, contentIDs, versionIDs []string
	for _, m := range ms {
		if err := validateID(m.EntityID); err != nil {
			return nil, facts{}, err
		}
		if err := contentref.ValidateID(m.ContentID); err != nil {
			return nil, facts{}, fmt.Errorf("%w: content id: %w", ErrInvalid, err)
		}
		entityIDs = append(entityIDs, string(m.EntityID))
		contentIDs = append(contentIDs, m.ContentID)
		if m.VersionID != "" {
			versionIDs = append(versionIDs, m.VersionID)
		}
	}
	f, err := s.facts(ctx, q, entityIDs, contentIDs, versionIDs)
	if err != nil {
		return nil, f, err
	}
	rows := make([]memberRow, 0, len(ms))
	index := map[memberKey]int{}
	for _, m := range ms {
		g, ok := f.entities[string(m.EntityID)]
		if !ok || g.State != StateActive {
			return nil, f, fmt.Errorf("%w: group %s is not an active group", ErrNotFound, m.EntityID)
		}
		gk := s.cfg.EntityKinds[g.Kind]
		contentKind, ok := f.contents[m.ContentID]
		if !ok {
			return nil, f, fmt.Errorf("%w: content %s", ErrNotFound, m.ContentID)
		}
		if k, ok := s.cfg.Kinds[contentKind]; !ok || !slices.Contains(k.Entities, g.Kind) {
			return nil, f, fmt.Errorf("%w: %s content does not join %s groups", ErrInvalid, contentKind, g.Kind)
		}
		row := memberRow{EntityID: string(m.EntityID), ContentID: m.ContentID, Role: strings.TrimSpace(m.Role), Position: m.Position,
			ActiveFrom: m.ActiveFrom, ActiveUntil: m.ActiveUntil, State: string(m.State)}
		if m.VersionID == "" && len(gk.MembershipScopes) > 0 && !gk.AllowsMembership(kinds.ContentScope) {
			return nil, f, fmt.Errorf("%w: %s memberships are version-level", ErrInvalid, g.Kind)
		}
		if m.VersionID != "" {
			if !gk.AllowsMembership(kinds.VersionScope) {
				return nil, f, fmt.Errorf("%w: %s memberships are content-level", ErrInvalid, g.Kind)
			}
			if f.versions[m.VersionID] != m.ContentID {
				return nil, f, fmt.Errorf("%w: version %s of content %s", ErrNotFound, m.VersionID, m.ContentID)
			}
			v := m.VersionID
			row.VersionID = &v
		}
		if row.Role != "" && !slices.Contains(gk.Roles, row.Role) {
			return nil, f, fmt.Errorf("%w: %s groups have no role %q", ErrInvalid, g.Kind, row.Role)
		}
		if row.Position != nil && (!gk.Ordered || *row.Position < 0) {
			return nil, f, fmt.Errorf("%w: positions belong to ordered groups and are not negative", ErrInvalid)
		}
		if (row.ActiveFrom != nil || row.ActiveUntil != nil) && !gk.Windows {
			return nil, f, fmt.Errorf("%w: %s memberships take no window", ErrInvalid, g.Kind)
		}
		if row.ActiveFrom != nil && row.ActiveUntil != nil && !row.ActiveUntil.After(*row.ActiveFrom) {
			return nil, f, fmt.Errorf("%w: a window must end after it starts", ErrInvalid)
		}
		switch MembershipState(row.State) {
		case "":
			row.State = string(MembershipActive)
		case MembershipActive, MembershipProposed:
		default:
			return nil, f, fmt.Errorf("%w: membership state %q", ErrInvalid, row.State)
		}
		if i, dup := index[row.key()]; dup {
			rows[i] = row
			continue
		}
		index[row.key()] = len(rows)
		rows = append(rows, row)
	}
	return rows, f, s.positions(ctx, q, rows, f)
}

// positions fills omitted positions of ordered entities.
func (s *Store) positions(ctx context.Context, q Querier, rows []memberRow, f facts) error {
	var ordered []string
	for _, r := range rows {
		if r.Position == nil && s.cfg.EntityKinds[f.entities[r.EntityID].Kind].Ordered && !slices.Contains(ordered, r.EntityID) {
			ordered = append(ordered, r.EntityID)
		}
	}
	if len(ordered) == 0 {
		return nil
	}
	res, err := q.Query(ctx, fmt.Sprintf(`SELECT entity_id, content_id, coalesce(version_id, ''), role, coalesce(position, -1) FROM %s
 WHERE tenant_id = $1 AND entity_id = ANY($2::text[])`, s.table("content_memberships")), s.tenant, ordered)
	if err != nil {
		return err
	}
	defer res.Close()
	// Every role of one member shares its position.
	existing := map[memberKey]int{}
	next := map[string]int{}
	for res.Next() {
		var k memberKey
		var p int
		if err := res.Scan(&k.entity, &k.content, &k.version, &k.role, &p); err != nil {
			return err
		}
		if k.role = ""; p >= 0 {
			existing[k] = p
		}
		next[k.entity] = max(next[k.entity], p+1)
	}
	if err := res.Err(); err != nil {
		return err
	}
	for i := range rows {
		r := &rows[i]
		if r.Position != nil || !slices.Contains(ordered, r.EntityID) {
			continue
		}
		k := r.key()
		k.role = ""
		p, ok := existing[k]
		if !ok {
			p = max(next[r.EntityID], 1)
			next[r.EntityID], existing[k] = p+1, p
		}
		r.Position = &p
	}
	return nil
}

func (s *Store) upsert(ctx context.Context, q Querier, rows []memberRow) error {
	if len(rows) == 0 {
		return nil
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, fmt.Sprintf(`INSERT INTO %s AS m (tenant_id, entity_id, content_id, version_id, role, position, active_from, active_until, state)
 SELECT $1, r.entity_id, r.content_id, r.version_id, r.role, r.position, r.active_from, r.active_until, r.state
 FROM jsonb_to_recordset($2::jsonb) AS r(entity_id text, content_id text, version_id text, role text, position int, active_from timestamptz, active_until timestamptz, state text)
 ON CONFLICT (tenant_id, entity_id, content_id, version_id, role) DO UPDATE
 SET position = EXCLUDED.position, active_from = EXCLUDED.active_from, active_until = EXCLUDED.active_until, state = EXCLUDED.state`,
		s.table("content_memberships")), s.tenant, data)
	return err
}

func contentIDsOf(rows []memberRow) []string {
	var out []string
	for _, r := range rows {
		if !slices.Contains(out, r.ContentID) {
			out = append(out, r.ContentID)
		}
	}
	return out
}

// AddMemberships adds memberships, or updates the position, window and state
// of existing ones (same entity, content, version and role).
func (s *Store) AddMemberships(ctx context.Context, ms []Membership) error {
	if len(ms) == 0 {
		return nil
	}
	return s.run(ctx, func(q Querier) error {
		if err := s.LockHierarchyShared(ctx, q); err != nil {
			return err
		}
		rows, _, err := s.prepare(ctx, q, ms)
		if err != nil {
			return err
		}
		if err := s.upsert(ctx, q, rows); err != nil {
			return err
		}
		return s.refreshFacets(ctx, q, contentIDsOf(rows))
	})
}

// RemoveMemberships deletes memberships matched by entity, content, version
// and role; unknown ones are ignored.
func (s *Store) RemoveMemberships(ctx context.Context, ms []Membership) error {
	if len(ms) == 0 {
		return nil
	}
	rows := make([]memberRow, 0, len(ms))
	for _, m := range ms {
		if err := validateID(m.EntityID); err != nil {
			return err
		}
		if err := contentref.ValidateID(m.ContentID); err != nil {
			return fmt.Errorf("%w: content id: %w", ErrInvalid, err)
		}
		r := memberRow{EntityID: string(m.EntityID), ContentID: m.ContentID, Role: strings.TrimSpace(m.Role)}
		if m.VersionID != "" {
			v := m.VersionID
			r.VersionID = &v
		}
		rows = append(rows, r)
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	return s.run(ctx, func(q Querier) error {
		if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s m USING jsonb_to_recordset($2::jsonb) AS r(entity_id text, content_id text, version_id text, role text)
 WHERE m.tenant_id = $1 AND m.entity_id = r.entity_id AND m.content_id = r.content_id AND m.version_id IS NOT DISTINCT FROM r.version_id AND m.role = r.role`,
			s.table("content_memberships")), s.tenant, data); err != nil {
			return err
		}
		return s.refreshFacets(ctx, q, contentIDsOf(rows))
	})
}

// SetContentEntities replaces, per content, its content- and version-level
// memberships in the entity kinds the set names; memberships in other kinds
// are untouched. Positions omitted for existing memberships are kept.
func (s *Store) SetContentEntities(ctx context.Context, sets []ContentEntities) error {
	if len(sets) == 0 {
		return nil
	}
	type scope struct {
		ContentID    string  `json:"content_id"`
		Kind         string  `json:"kind"`
		VersionScope *string `json:"version_scope"`
	}
	sets = slices.Clone(sets)
	var contentIDs []string
	for i, set := range sets {
		if err := contentref.ValidateID(set.ContentID); err != nil {
			return fmt.Errorf("%w: content id: %w", ErrInvalid, err)
		}
		if slices.Contains(contentIDs, set.ContentID) {
			return fmt.Errorf("%w: content %s is set twice", ErrInvalid, set.ContentID)
		}
		contentIDs = append(contentIDs, set.ContentID)
		if set.VersionScope != nil {
			version := *set.VersionScope
			sets[i].VersionScope = &version
			if version != "" {
				if err := contentref.ValidateID(version); err != nil {
					return fmt.Errorf("%w: version id: %w", ErrInvalid, err)
				}
			}
		}
		sets[i].Memberships = slices.Clone(set.Memberships)
		for j, m := range sets[i].Memberships {
			if m.ContentID != "" && m.ContentID != set.ContentID {
				return fmt.Errorf("%w: a membership of %s names content %s", ErrInvalid, set.ContentID, m.ContentID)
			}
			sets[i].Memberships[j].ContentID = set.ContentID
			if set.VersionScope != nil {
				if m.VersionID != "" && m.VersionID != *set.VersionScope {
					return fmt.Errorf("%w: membership version is outside the replacement scope", ErrInvalid)
				}
				sets[i].Memberships[j].VersionID = *set.VersionScope
			}
		}
	}
	return s.run(ctx, func(q Querier) error {
		if err := s.LockHierarchyShared(ctx, q); err != nil {
			return err
		}
		// Concurrent sets of one content serialize, so neither merges into the other.
		if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('contentkit:groups:' || $1 || ':' || id, 0)) FROM unnest($2::text[]) AS id ORDER BY id`,
			s.tenant, contentIDs); err != nil {
			return err
		}
		var all []Membership
		for i := range sets {
			if err := s.scopeContentEntities(ctx, q, &sets[i]); err != nil {
				return err
			}
			if len(sets[i].Create) > 0 {
				known, err := s.facts(ctx, q, nil, []string{sets[i].ContentID}, nil)
				if err != nil {
					return err
				}
				contentKind, ok := known.contents[sets[i].ContentID]
				if !ok {
					return fmt.Errorf("%w: content %s", ErrNotFound, sets[i].ContentID)
				}
				if err := s.linkNames(ctx, q.(pgx.Tx), contentKind, &sets[i]); err != nil {
					return err
				}
			}
			all = append(all, sets[i].Memberships...)
		}
		rows, f, err := s.prepare(ctx, q, all)
		if err != nil {
			return err
		}
		known, err := s.facts(ctx, q, nil, contentIDs, nil)
		if err != nil {
			return err
		}
		scopes := []scope{}
		for _, set := range sets {
			contentKind, ok := known.contents[set.ContentID]
			if !ok {
				return fmt.Errorf("%w: content %s", ErrNotFound, set.ContentID)
			}
			declared := s.cfg.Kinds[contentKind].Entities
			kinds := set.Kinds
			if len(kinds) == 0 && set.VersionScope == nil {
				kinds = declared
			}
			for _, k := range kinds {
				if !slices.Contains(declared, k) {
					return fmt.Errorf("%w: %s content does not join %s groups", ErrInvalid, contentKind, k)
				}
				scopes = append(scopes, scope{set.ContentID, k, set.VersionScope})
			}
			for _, r := range rows {
				if r.ContentID == set.ContentID && !slices.Contains(kinds, f.entities[r.EntityID].Kind) {
					return fmt.Errorf("%w: a %s membership is outside the kinds written", ErrInvalid, f.entities[r.EntityID].Kind)
				}
			}
		}
		if err := s.upsert(ctx, q, rows); err != nil {
			return err
		}
		keep, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		scoped, err := json.Marshal(scopes)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, fmt.Sprintf(`DELETE FROM %s m USING %s g, jsonb_to_recordset($2::jsonb) AS sc(content_id text, kind text, version_scope text)
 WHERE m.tenant_id = $1 AND m.content_id = sc.content_id AND g.tenant_id = $1 AND g.id = m.entity_id AND g.kind = sc.kind
   AND (sc.version_scope IS NULL OR coalesce(m.version_id, '') = sc.version_scope)
   AND NOT EXISTS (SELECT 1 FROM jsonb_to_recordset($3::jsonb) AS r(entity_id text, content_id text, version_id text, role text)
     WHERE r.entity_id = m.entity_id AND r.content_id = m.content_id AND r.version_id IS NOT DISTINCT FROM m.version_id AND r.role = m.role)`,
			s.table("content_memberships"), s.table("content_entities")), s.tenant, scoped, keep); err != nil {
			return err
		}
		return s.refreshFacets(ctx, q, contentIDs)
	})
}

// scopeContentEntities validates a scoped replacement even when it clears every
// membership. Its caller holds the same content advisory lock as legacy sets.
func (s *Store) scopeContentEntities(ctx context.Context, q Querier, set *ContentEntities) error {
	if set.VersionScope == nil {
		return nil
	}
	var contentKind string
	if err := q.QueryRow(ctx, `SELECT kind FROM `+s.table("content_items")+` WHERE tenant_id=$1 AND id=$2`, s.tenant, set.ContentID).Scan(&contentKind); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: content %s", ErrNotFound, set.ContentID)
		}
		return err
	}
	versionScope := *set.VersionScope
	if versionScope != "" {
		var exists bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.table("content_versions")+` WHERE tenant_id=$1 AND id=$2 AND content_id=$3 AND state <> 'deleted')`, s.tenant, versionScope, set.ContentID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: version %s of content %s", ErrNotFound, versionScope, set.ContentID)
		}
	}
	declared := s.cfg.Kinds[contentKind].Entities
	allowed := func(kind string) bool {
		scope := kinds.ContentScope
		if versionScope != "" {
			scope = kinds.VersionScope
		}
		return s.cfg.EntityKinds[kind].AllowsMembership(scope)
	}
	if len(set.Kinds) == 0 {
		set.Kinds = []string{}
		for _, kind := range declared {
			if allowed(kind) {
				set.Kinds = append(set.Kinds, kind)
			}
		}
	}
	for _, kind := range set.Kinds {
		if !slices.Contains(declared, kind) || !allowed(kind) {
			return fmt.Errorf("%w: %s entities are outside the replacement scope", ErrInvalid, kind)
		}
	}
	for _, entity := range set.Create {
		if !slices.Contains(set.Kinds, entity.Kind) {
			return fmt.Errorf("%w: %s entities are outside the replacement scope", ErrInvalid, entity.Kind)
		}
	}
	return nil
}

// MembershipsOf returns every membership of each content and its
// versions, by content id, ordered by entity and version (content first).
func (s *Store) MembershipsOf(ctx context.Context, contentIDs []string) (map[string][]Membership, error) {
	out := map[string][]Membership{}
	if len(contentIDs) == 0 {
		return out, nil
	}
	for _, id := range contentIDs {
		if err := contentref.ValidateID(id); err != nil {
			return nil, fmt.Errorf("%w: content id: %w", ErrInvalid, err)
		}
	}
	err := s.read(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT %s FROM %s m WHERE m.tenant_id = $1 AND m.content_id = ANY($2::text[])
 ORDER BY m.content_id, m.entity_id, m.version_id NULLS FIRST, m.role`, membershipColumns, s.table("content_memberships")), s.tenant, contentIDs)
		if err != nil {
			return err
		}
		ms, err := scanMemberships(rows)
		for _, m := range ms {
			out[m.ContentID] = append(out[m.ContentID], m)
		}
		return err
	})
	return out, err
}

const membershipColumns = `m.entity_id, m.content_id, coalesce(m.version_id, ''), m.role, m.position, m.active_from, m.active_until, m.state`

func scanMemberships(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}) ([]Membership, error) {
	defer rows.Close()
	out := []Membership{}
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.EntityID, &m.ContentID, &m.VersionID, &m.Role, &m.Position, &m.ActiveFrom, &m.ActiveUntil, &m.State); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MemberOptions pages Members.
type MemberOptions struct {
	// Cursor is the previous page's NextCursor.
	Cursor string
	// Limit defaults to 50 and is capped at 500.
	Limit int
}

// MemberPage is one page of an entity's memberships.
type MemberPage struct {
	Members []Membership `json:"members"`
	// NextCursor pages on; empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

type memberCursor struct {
	Position int    `json:"p"`
	Content  string `json:"c"`
	Version  string `json:"v"`
	Role     string `json:"r"`
}

// Members pages an entity's memberships in member order: by position (an
// ordered entity), then content, version (content first) and role.
// ErrNotFound for an entity the tenant lacks.
func (s *Store) Members(ctx context.Context, id EntityID, opts MemberOptions) (MemberPage, error) {
	page := MemberPage{Members: []Membership{}}
	if err := validateID(id); err != nil {
		return page, err
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	var after *memberCursor
	if opts.Cursor != "" {
		var c memberCursor
		b, err := base64.RawURLEncoding.DecodeString(opts.Cursor)
		if err == nil {
			err = json.Unmarshal(b, &c)
		}
		if err != nil {
			return page, fmt.Errorf("%w: cursor", ErrInvalid)
		}
		after = &c
	}
	const sortKey = `(coalesce(m.position, 2147483647), m.content_id, coalesce(m.version_id, ''), m.role)`
	err := s.read(ctx, func(q Querier) error {
		var exists bool
		if err := q.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s WHERE tenant_id = $1 AND id = $2)`, s.table("content_entities")), s.tenant, string(id)).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		args := []any{s.tenant, string(id), limit + 1}
		where := ""
		if after != nil {
			args = append(args, after.Position, after.Content, after.Version, after.Role)
			where = " AND " + sortKey + " > ($4, $5, $6, $7)"
		}
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT %s FROM %s m WHERE m.tenant_id = $1 AND m.entity_id = $2%s ORDER BY %s LIMIT $3`,
			membershipColumns, s.table("content_memberships"), where, strings.Trim(sortKey, "()")), args...)
		if err != nil {
			return err
		}
		page.Members, err = scanMemberships(rows)
		return err
	})
	if err != nil {
		return MemberPage{Members: []Membership{}}, err
	}
	if len(page.Members) > limit {
		page.Members = page.Members[:limit]
		last := page.Members[limit-1]
		c := memberCursor{Position: math.MaxInt32, Content: last.ContentID, Version: last.VersionID, Role: last.Role}
		if last.Position != nil {
			c.Position = *last.Position
		}
		b, _ := json.Marshal(c)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	return page, nil
}

// Reorder sets the member order of ordered entities: the listed members take
// positions 1..n in list order (every role of a member moves together), the
// others follow in their current order. A listed member the entity lacks is
// ErrNotFound.
func (s *Store) Reorder(ctx context.Context, orders []Order) error {
	if len(orders) == 0 {
		return nil
	}
	ids := make([]EntityID, len(orders))
	for i, o := range orders {
		ids[i] = o.EntityID
	}
	list, err := uniqueIDs(ids)
	if err != nil {
		return err
	}
	if len(list) != len(orders) {
		return fmt.Errorf("%w: a group is ordered twice", ErrInvalid)
	}
	return s.run(ctx, func(q Querier) error {
		locked, err := s.lock(ctx, q, list)
		if err != nil {
			return err
		}
		for _, g := range locked {
			if !s.cfg.EntityKinds[g.Kind].Ordered {
				return fmt.Errorf("%w: %s groups are not ordered", ErrInvalid, g.Kind)
			}
		}
		var rows []memberRow
		for _, o := range orders {
			res, err := q.Query(ctx, fmt.Sprintf(`SELECT %s FROM %s m WHERE m.tenant_id = $1 AND m.entity_id = $2
 ORDER BY coalesce(m.position, 2147483647), m.content_id, coalesce(m.version_id, ''), m.role`, membershipColumns, s.table("content_memberships")), s.tenant, string(o.EntityID))
			if err != nil {
				return err
			}
			current, err := scanMemberships(res)
			if err != nil {
				return err
			}
			rank := map[MemberRef]int{}
			for i, ref := range o.Members {
				if _, dup := rank[ref]; !dup {
					rank[ref] = i
				}
			}
			seen := map[MemberRef]bool{}
			for _, m := range current {
				seen[MemberRef{m.ContentID, m.VersionID}] = true
			}
			for ref := range rank {
				if !seen[ref] {
					return fmt.Errorf("%w: %s is not a member of %s", ErrNotFound, ref.ContentID, o.EntityID)
				}
			}
			sort.SliceStable(current, func(i, j int) bool {
				ri, iok := rank[MemberRef{current[i].ContentID, current[i].VersionID}]
				rj, jok := rank[MemberRef{current[j].ContentID, current[j].VersionID}]
				switch {
				case iok && jok:
					return ri < rj
				case iok != jok:
					return iok
				}
				return false
			})
			pos := map[MemberRef]int{}
			for _, m := range current {
				ref := MemberRef{m.ContentID, m.VersionID}
				p, ok := pos[ref]
				if !ok {
					p = len(pos) + 1
					pos[ref] = p
				}
				r := memberRow{EntityID: string(o.EntityID), ContentID: m.ContentID, Role: m.Role, Position: &p}
				if m.VersionID != "" {
					v := m.VersionID
					r.VersionID = &v
				}
				rows = append(rows, r)
			}
		}
		if len(rows) == 0 {
			return nil
		}
		data, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		_, err = q.Exec(ctx, fmt.Sprintf(`UPDATE %s m SET position = r.position
 FROM jsonb_to_recordset($2::jsonb) AS r(entity_id text, content_id text, version_id text, role text, position int)
 WHERE m.tenant_id = $1 AND m.entity_id = r.entity_id AND m.content_id = r.content_id AND m.version_id IS NOT DISTINCT FROM r.version_id AND m.role = r.role`,
			s.table("content_memberships")), s.tenant, data)
		return err
	})
}
