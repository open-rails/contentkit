package urlstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/contentref"
	contenturl "github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/codes"
	"github.com/open-rails/contentkit/internal/search"
)

// Options configures one tenant's Store.
type Options struct {
	Pool   *pgxpool.Pool
	Schema string // the schema ContentKit's migrations were applied in
	Tenant string
}

// Store is one tenant's content-code registry.
type Store struct {
	pool   *pgxpool.Pool
	qs     string
	tenant string
}

// New validates the options and returns the store.
func New(opts Options) (*Store, error) {
	if opts.Pool == nil {
		return nil, fmt.Errorf("%w: Pool is required", contenturl.ErrInvalid)
	}
	qs, err := search.QuoteSchema(opts.Schema)
	if err != nil {
		return nil, fmt.Errorf("%w: Schema: %v", contenturl.ErrInvalid, err)
	}
	tenant := strings.TrimSpace(opts.Tenant)
	if tenant == "" {
		return nil, fmt.Errorf("%w: Tenant is required", contenturl.ErrInvalid)
	}
	return &Store{pool: opts.Pool, qs: qs, tenant: tenant}, nil
}

func (s *Store) q() codes.Querier { return codes.Pgx(s.pool) }

func (s *Store) ref(r contentref.ContentRef) (codes.Key, error) {
	if r.TenantID != s.tenant {
		return codes.Key{}, fmt.Errorf("%w: tenant %q, store is pinned to %q", contenturl.ErrInvalid, r.TenantID, s.tenant)
	}
	if err := r.Validate(); err != nil {
		return codes.Key{}, fmt.Errorf("%w: %w", contenturl.ErrInvalid, err)
	}
	if r.ContentVersionID != nil {
		return codes.Key{}, fmt.Errorf("%w: codes name works, not versions", contenturl.ErrInvalid)
	}
	return codes.Key{Kind: r.ContentKind, ID: r.ContentID}, nil
}

const linkColumns = `t.content_kind, t.content_id, t.code, t.slug, t.slugs::text, coalesce(t.merged_into, '')`

func (s *Store) scanLink(rows codes.Rows, extra ...any) (contenturl.Link, string, error) {
	var l contenturl.Link
	var slugs, merged string
	if err := rows.Scan(append([]any{&l.ContentKind, &l.ContentID, &l.Code, &l.Slug, &slugs, &merged}, extra...)...); err != nil {
		return contenturl.Link{}, "", err
	}
	l.TenantID = s.tenant
	if err := json.Unmarshal([]byte(slugs), &l.Slugs); err != nil {
		return contenturl.Link{}, "", err
	}
	if len(l.Slugs) == 0 {
		l.Slugs = nil
	}
	return l, merged, nil
}

// links returns the resolved link of each registered key. A merged record
// resolves to its survivor.
func (s *Store) links(ctx context.Context, keys []codes.Key) (map[codes.Key]contenturl.Link, error) {
	out := make(map[codes.Key]contenturl.Link, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	kinds, ids := make([]string, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		kinds[i], ids[i] = k.Kind, k.ID
	}
	rows, err := s.q().Query(ctx, fmt.Sprintf(`SELECT %s, c.content_kind, c.content_id
 FROM unnest($2::text[], $3::text[]) AS r(kind, id)
 JOIN %[2]s.content_codes c ON c.tenant_id = $1 AND c.content_kind = r.kind AND c.content_id = r.id
 JOIN %[2]s.content_codes t ON t.tenant_id = $1 AND t.code = coalesce(c.merged_into, c.code)`, linkColumns, s.qs), s.tenant, kinds, ids)
	if err != nil {
		return nil, err
	}
	further := map[codes.Key]contenturl.Code{}
	var chained []contenturl.Code
	for rows.Next() {
		var k codes.Key
		l, merged, err := s.scanLink(rows, &k.Kind, &k.ID)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out[k] = l
		if merged != "" {
			further[k] = contenturl.Code(merged)
			chained = append(chained, contenturl.Code(merged))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(chained) == 0 {
		return out, err
	}
	// Redirects are one hop; follow a longer chain the way Resolve does.
	survivors, err := s.Resolve(ctx, chained)
	if err != nil {
		return nil, err
	}
	for k, code := range further {
		if l, ok := survivors[code]; ok {
			out[k] = l
		}
	}
	return out, nil
}

// Links returns the link of each registered reference, keyed by
// ref.Key(); unregistered references are absent. Use it to render lists.
func (s *Store) Links(ctx context.Context, refs []contentref.ContentRef) (map[contentref.ContentKey]contenturl.Link, error) {
	keys := make([]codes.Key, 0, len(refs))
	for _, r := range refs {
		k, err := s.ref(r.Content())
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	got, err := s.links(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make(map[contentref.ContentKey]contenturl.Link, len(got))
	for _, r := range refs {
		if l, ok := got[codes.Key{Kind: r.ContentKind, ID: r.ContentID}]; ok {
			out[r.Key()] = l
		}
	}
	return out, nil
}

const maxHops = 4

// Resolve returns the link each code names, following merges to the
// survivor; a code the tenant does not have is absent. A code not in
// canonical form is ErrInvalidCode.
func (s *Store) Resolve(ctx context.Context, cs []contenturl.Code) (map[contenturl.Code]contenturl.Link, error) {
	out := make(map[contenturl.Code]contenturl.Link, len(cs))
	// at maps each code to look up to the asked codes leading to it.
	at := map[contenturl.Code][]contenturl.Code{}
	for _, c := range cs {
		if !c.Valid() {
			return nil, contenturl.ErrInvalidCode
		}
		at[c] = []contenturl.Code{c}
	}
	for hop := 0; len(at) > 0; hop++ {
		if hop == maxHops {
			return nil, fmt.Errorf("contenturl: a code redirects more than %d times", maxHops)
		}
		pending := make([]string, 0, len(at))
		for c := range at {
			pending = append(pending, string(c))
		}
		rows, err := s.q().Query(ctx, fmt.Sprintf(`SELECT %s, c.code FROM %s.content_codes c
 JOIN %[2]s.content_codes t ON t.tenant_id = c.tenant_id AND t.code = coalesce(c.merged_into, c.code)
 WHERE c.tenant_id = $1 AND c.code = ANY($2)`, linkColumns, s.qs), s.tenant, pending)
		if err != nil {
			return nil, err
		}
		next := map[contenturl.Code][]contenturl.Code{}
		for rows.Next() {
			var from string
			l, merged, err := s.scanLink(rows, &from)
			if err != nil {
				rows.Close()
				return nil, err
			}
			for _, asked := range at[contenturl.Code(from)] {
				if merged == "" {
					out[asked] = l
				} else {
					next[contenturl.Code(merged)] = append(next[contenturl.Code(merged)], asked)
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		at = next
	}
	return out, nil
}

// Versions answers the id of each key's version while it is live (published,
// past its publication time, its content live), as readers open it; keys
// naming none are left out.
func (s *Store) Versions(ctx context.Context, keys []contenturl.VersionKey) (map[contenturl.VersionKey]string, error) {
	out := make(map[contenturl.VersionKey]string, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	contents, cs := make([]string, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		contents[i], cs[i] = k.ContentID, string(k.Code)
	}
	rows, err := s.q().Query(ctx, fmt.Sprintf(`SELECT v.content_id, v.code, v.id FROM unnest($2::text[], $3::text[]) AS k(content_id, code)
JOIN %[1]s.content_versions v ON v.tenant_id = $1 AND v.content_id = k.content_id AND v.code = k.code
JOIN %[1]s.content_facets f ON f.tenant_id = v.tenant_id AND f.version_id = v.id
WHERE NOT f.deleted AND f.live_from <= now()`, s.qs), s.tenant, contents, cs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k contenturl.VersionKey
		var code, id string
		if err := rows.Scan(&k.ContentID, &code, &id); err != nil {
			return nil, err
		}
		k.Code = contenturl.VersionCode(code)
		out[k] = id
	}
	return out, rows.Err()
}

// ResolveAliases returns what each alias names, following merges, in keys'
// order: nil for one the tenant does not have.
func (s *Store) ResolveAliases(ctx context.Context, keys []contenturl.AliasKey) ([]*contenturl.AliasMatch, error) {
	out := make([]*contenturl.AliasMatch, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	sources, kinds, ks := make([]string, len(keys)), make([]string, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		if err := codes.ValidAlias(k.Source, k.Kind, k.Key); err != nil {
			return nil, err
		}
		sources[i], kinds[i], ks[i] = k.Source, k.Kind, k.Key
	}
	rows, err := s.q().Query(ctx, fmt.Sprintf(`SELECT r.i, a.code, coalesce(a.locator, '')
 FROM unnest($2::text[], $3::text[], $4::text[]) WITH ORDINALITY AS r(source, kind, key, i)
 JOIN %s.content_code_aliases a ON a.tenant_id = $1 AND a.source = r.source AND a.legacy_kind = r.kind AND a.legacy_key = r.key`, s.qs),
		s.tenant, sources, kinds, ks)
	if err != nil {
		return nil, err
	}
	type hit struct {
		code    contenturl.Code
		locator string
	}
	hits := map[int]hit{}
	var found []contenturl.Code
	for rows.Next() {
		var i int
		var code string
		var h hit
		if err := rows.Scan(&i, &code, &h.locator); err != nil {
			rows.Close()
			return nil, err
		}
		h.code = contenturl.Code(code)
		hits[i-1] = h
		found = append(found, h.code)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(found) == 0 {
		return out, err
	}
	links, err := s.Resolve(ctx, found)
	if err != nil {
		return nil, err
	}
	for i, h := range hits {
		if l, ok := links[h.code]; ok {
			out[i] = &contenturl.AliasMatch{Link: l, Locator: h.locator}
		}
	}
	return out, nil
}
