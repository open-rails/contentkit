package contenturl

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/codes"
	"github.com/open-rails/contentkit/search"
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
		return nil, fmt.Errorf("%w: Pool is required", ErrInvalid)
	}
	qs, err := search.QuoteSchema(opts.Schema)
	if err != nil {
		return nil, fmt.Errorf("%w: Schema: %v", ErrInvalid, err)
	}
	tenant := strings.TrimSpace(opts.Tenant)
	if tenant == "" {
		return nil, fmt.Errorf("%w: Tenant is required", ErrInvalid)
	}
	return &Store{pool: opts.Pool, qs: qs, tenant: tenant}, nil
}

// Tenant is the tenant the store is pinned to.
func (s *Store) Tenant() string { return s.tenant }

func (s *Store) q() codes.Querier { return codes.Pgx(s.pool) }

// write runs fn in a transaction.
func (s *Store) write(ctx context.Context, fn func(codes.Querier) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := fn(codes.Pgx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) ref(r contentref.ContentRef) (codes.Key, error) {
	if r.TenantID != s.tenant {
		return codes.Key{}, fmt.Errorf("%w: tenant %q, store is pinned to %q", ErrInvalid, r.TenantID, s.tenant)
	}
	if err := r.Validate(); err != nil {
		return codes.Key{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if r.ContentVersionID != nil {
		return codes.Key{}, fmt.Errorf("%w: codes name works, not versions", ErrInvalid)
	}
	return codes.Key{Kind: r.ContentKind, ID: r.ContentID}, nil
}

// Entry is one content record to register. Title (any text) becomes the
// default slug through Slugify; Titles, keyed by language, the localized
// slugs. Titles nil keeps the stored localized slugs; an empty map clears them.
type Entry struct {
	contentref.ContentRef
	Title  string
	Titles map[string]string
}

// Put gives every entry a code if it has none (codes never change) and sets
// its slugs from the titles; it is idempotent. The result is aligned with
// entries.
func (s *Store) Put(ctx context.Context, entries ...Entry) ([]Link, error) {
	in := make([]codes.Entry, len(entries))
	for i, e := range entries {
		k, err := s.ref(e.ContentRef)
		if err != nil {
			return nil, err
		}
		in[i] = codes.Entry{Key: k, Slug: Slugify(e.Title)}
		if e.Titles != nil {
			in[i].Slugs = make(map[string]string, len(e.Titles))
			for l, t := range e.Titles {
				in[i].Slugs[l] = Slugify(t)
			}
		}
	}
	var got map[codes.Key]string
	err := s.write(ctx, func(q codes.Querier) (err error) {
		got, err = codes.Put(ctx, q, s.qs, s.tenant, in)
		return err
	})
	if err != nil {
		return nil, err
	}
	keys := make([]codes.Key, len(in))
	for i, e := range in {
		keys[i] = e.Key
	}
	links, err := s.links(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make([]Link, len(in))
	for i, e := range in {
		l, ok := links[e.Key]
		if !ok || got[e.Key] == "" {
			return nil, fmt.Errorf("contenturl: %s/%s has no code after Put", e.Kind, e.ID)
		}
		out[i] = l
	}
	return out, nil
}

const linkColumns = `t.content_kind, t.content_id, t.code, t.slug, t.slugs::text, coalesce(t.merged_into, '')`

func (s *Store) scanLink(rows codes.Rows, extra ...any) (Link, string, error) {
	var l Link
	var slugs, merged string
	if err := rows.Scan(append([]any{&l.ContentKind, &l.ContentID, &l.Code, &l.Slug, &slugs, &merged}, extra...)...); err != nil {
		return Link{}, "", err
	}
	l.TenantID = s.tenant
	if err := json.Unmarshal([]byte(slugs), &l.Slugs); err != nil {
		return Link{}, "", err
	}
	if len(l.Slugs) == 0 {
		l.Slugs = nil
	}
	return l, merged, nil
}

// links returns the resolved link of each registered key. A merged record
// resolves to its survivor.
func (s *Store) links(ctx context.Context, keys []codes.Key) (map[codes.Key]Link, error) {
	out := make(map[codes.Key]Link, len(keys))
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
	further := map[codes.Key]Code{}
	var chained []Code
	for rows.Next() {
		var k codes.Key
		l, merged, err := s.scanLink(rows, &k.Kind, &k.ID)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out[k] = l
		if merged != "" {
			further[k] = Code(merged)
			chained = append(chained, Code(merged))
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
func (s *Store) Links(ctx context.Context, refs []contentref.ContentRef) (map[contentref.ContentKey]Link, error) {
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
	out := make(map[contentref.ContentKey]Link, len(got))
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
func (s *Store) Resolve(ctx context.Context, cs []Code) (map[Code]Link, error) {
	out := make(map[Code]Link, len(cs))
	// at maps each code to look up to the asked codes leading to it.
	at := map[Code][]Code{}
	for _, c := range cs {
		if !c.Valid() {
			return nil, ErrInvalidCode
		}
		at[c] = []Code{c}
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
		next := map[Code][]Code{}
		for rows.Next() {
			var from string
			l, merged, err := s.scanLink(rows, &from)
			if err != nil {
				rows.Close()
				return nil, err
			}
			for _, asked := range at[Code(from)] {
				if merged == "" {
					out[asked] = l
				} else {
					next[Code(merged)] = append(next[Code(merged)], asked)
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

// VersionKey names a version by its content and version code.
type VersionKey struct {
	ContentID string
	Code      VersionCode
}

// Versions answers the id of each key's version while it is live (published,
// past its publication time, its content live), as readers open it; keys
// naming none are left out.
func (s *Store) Versions(ctx context.Context, keys []VersionKey) (map[VersionKey]string, error) {
	out := make(map[VersionKey]string, len(keys))
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
		var k VersionKey
		var code, id string
		if err := rows.Scan(&k.ContentID, &code, &id); err != nil {
			return nil, err
		}
		k.Code = VersionCode(code)
		out[k] = id
	}
	return out, rows.Err()
}

// Merge makes from's code redirect to into's: both stay valid, from's resolves
// to into. Use it when duplicate content is folded into one record. Both
// must be registered (ErrNotFound); a merge back into from is ErrConflict.
func (s *Store) Merge(ctx context.Context, from, into contentref.ContentRef) error {
	f, err := s.ref(from)
	if err != nil {
		return err
	}
	t, err := s.ref(into)
	if err != nil {
		return err
	}
	return s.write(ctx, func(q codes.Querier) error { return codes.Merge(ctx, q, s.qs, s.tenant, f, t) })
}

// AliasKey names another system's identifier: the system (an import
// batch's source), the identifier's space and the identifier.
type AliasKey struct {
	Source string `json:"source"`
	Kind   string `json:"kind"`
	Key    string `json:"key"`
}

// AliasMatch is what an alias resolves to.
type AliasMatch struct {
	Link
	Locator string
}

// ResolveAliases returns what each alias names, following merges, in keys'
// order: nil for one the tenant does not have.
func (s *Store) ResolveAliases(ctx context.Context, keys []AliasKey) ([]*AliasMatch, error) {
	out := make([]*AliasMatch, len(keys))
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
		code    Code
		locator string
	}
	hits := map[int]hit{}
	var found []Code
	for rows.Next() {
		var i int
		var code string
		var h hit
		if err := rows.Scan(&i, &code, &h.locator); err != nil {
			rows.Close()
			return nil, err
		}
		h.code = Code(code)
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
			out[i] = &AliasMatch{Link: l, Locator: h.locator}
		}
	}
	return out, nil
}
