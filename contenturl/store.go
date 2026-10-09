package contenturl

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
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
	tx     codes.Querier
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

// WithTx returns a store that runs in tx; the caller commits.
func (s *Store) WithTx(tx pgx.Tx) *Store {
	c := *s
	c.tx = codes.Pgx(tx)
	return &c
}

// WithSQLTx returns a store that runs in a database/sql transaction opened
// on the pgx stdlib driver; the caller commits.
func (s *Store) WithSQLTx(tx *sql.Tx) *Store {
	c := *s
	c.tx = codes.SQL(tx)
	return &c
}

func (s *Store) q() codes.Querier {
	if s.tx != nil {
		return s.tx
	}
	return codes.Pgx(s.pool)
}

// write runs fn in the bound transaction or a new one.
func (s *Store) write(ctx context.Context, fn func(codes.Querier) error) error {
	if s.tx != nil {
		return fn(s.tx)
	}
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
// its slugs from the titles. Call it in the transaction that creates the
// content and again when a title changes; it is idempotent, so a backfill
// calls it over existing rows. The result is aligned with entries.
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
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Redirects are one hop; follow a longer chain the way Resolve does.
	for k, code := range further {
		l, err := s.Resolve(ctx, code)
		if err != nil {
			return nil, err
		}
		out[k] = l
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

// Resolve returns the link a code names, following a merge to the survivor.
// ErrNotFound when the tenant has no such code.
func (s *Store) Resolve(ctx context.Context, code Code) (Link, error) {
	if !code.Valid() {
		return Link{}, ErrInvalidCode
	}
	for hop := 0; hop < maxHops; hop++ {
		rows, err := s.q().Query(ctx, fmt.Sprintf(`SELECT %s FROM %s.content_codes c
 JOIN %[2]s.content_codes t ON t.tenant_id = c.tenant_id AND t.code = coalesce(c.merged_into, c.code)
 WHERE c.tenant_id = $1 AND c.code = $2`, linkColumns, s.qs), s.tenant, string(code))
		if err != nil {
			return Link{}, err
		}
		l, merged, found, err := s.one(rows)
		if err != nil {
			return Link{}, err
		}
		if !found {
			return Link{}, fmt.Errorf("%w: code %s", ErrNotFound, code)
		}
		if merged == "" {
			return l, nil
		}
		code = Code(merged)
	}
	return Link{}, fmt.Errorf("contenturl: code %s redirects more than %d times", code, maxHops)
}

func (s *Store) one(rows codes.Rows) (Link, string, bool, error) {
	defer rows.Close()
	if !rows.Next() {
		return Link{}, "", false, rows.Err()
	}
	l, merged, err := s.scanLink(rows)
	if err != nil {
		return Link{}, "", false, err
	}
	return l, merged, true, rows.Err()
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

var aliasPartRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// Alias is an identifier of another system that resolves to content: a
// legacy site's id, token or name. Source names the system
// ("doujins-legacy"), LegacyKind the identifier space ("folder", "tag-name",
// "video"), Key the identifier as the host normalizes it (stored and matched
// verbatim). Locator is an optional host position inside the content, such
// as a page. Many aliases may name one content record.
type Alias struct {
	Source     string
	LegacyKind string
	Key        string
	Locator    string
	contentref.ContentRef
}

// AliasMatch is what an alias resolves to.
type AliasMatch struct {
	Link
	Locator string
}

func validAlias(source, kind, key string) error {
	if !aliasPartRE.MatchString(source) || !aliasPartRE.MatchString(kind) {
		return fmt.Errorf("%w: alias source %q / kind %q", ErrInvalid, source, kind)
	}
	if key == "" || len(key) > 512 {
		return fmt.Errorf("%w: alias key must be 1-512 bytes", ErrInvalid)
	}
	return nil
}

// PutAliases records aliases, typically in the import transaction that
// writes their content. An alias is written once: repeating it is a no-op,
// pointing it at other content (or another locator) is ErrConflict and
// writes nothing. The content must be registered (ErrNotFound). Roll the
// transaction back on any error.
func (s *Store) PutAliases(ctx context.Context, aliases ...Alias) error {
	if len(aliases) == 0 {
		return nil
	}
	type row struct {
		Source  string  `json:"source"`
		Kind    string  `json:"legacy_kind"`
		Key     string  `json:"legacy_key"`
		Locator *string `json:"locator"`
		CKind   string  `json:"kind"`
		ID      string  `json:"id"`
	}
	rows := make([]row, len(aliases))
	for i, a := range aliases {
		if err := validAlias(a.Source, a.LegacyKind, a.Key); err != nil {
			return err
		}
		k, err := s.ref(a.ContentRef)
		if err != nil {
			return err
		}
		rows[i] = row{Source: a.Source, Kind: a.LegacyKind, Key: a.Key, CKind: k.Kind, ID: k.ID}
		if a.Locator != "" {
			rows[i].Locator = &a.Locator
		}
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	return s.write(ctx, func(q codes.Querier) error {
		input := `jsonb_to_recordset($2::jsonb) AS r(source text, legacy_kind text, legacy_key text, locator text, kind text, id text)`
		resolved := fmt.Sprintf(`SELECT r.source, r.legacy_kind, r.legacy_key, r.locator, c.code FROM %[2]s
 LEFT JOIN %[1]s.content_codes c ON c.tenant_id = $1 AND c.content_kind = r.kind AND c.content_id = r.id`, s.qs, input)
		// Refuse before writing: unregistered content, a batch that disagrees
		// with itself, or an alias that already names something else.
		var missing, conflicting int64
		if err := queryOne(ctx, q, fmt.Sprintf(`WITH r AS (%[2]s)
SELECT count(*) FILTER (WHERE code IS NULL),
 (SELECT count(*) FROM (SELECT 1 FROM r GROUP BY source, legacy_kind, legacy_key HAVING count(DISTINCT (code, locator)) > 1) d)
 + count(*) FILTER (WHERE EXISTS (SELECT 1 FROM %[1]s.content_code_aliases a WHERE a.tenant_id = $1 AND a.source = r.source
   AND a.legacy_kind = r.legacy_kind AND a.legacy_key = r.legacy_key AND (a.code IS DISTINCT FROM r.code OR a.locator IS DISTINCT FROM r.locator)))
FROM r`, s.qs, resolved), []any{s.tenant, string(data)}, &missing, &conflicting); err != nil {
			return err
		}
		if missing > 0 {
			return fmt.Errorf("%w: %d aliases name unregistered content", ErrNotFound, missing)
		}
		if conflicting > 0 {
			return fmt.Errorf("%w: %d aliases already name other content", ErrConflict, conflicting)
		}
		if err := codes.Drain(q.Query(ctx, fmt.Sprintf(`INSERT INTO %[1]s.content_code_aliases (tenant_id, source, legacy_kind, legacy_key, code, locator)
 SELECT DISTINCT ON (source, legacy_kind, legacy_key) $1, source, legacy_kind, legacy_key, code, locator FROM (%[2]s) r
 ON CONFLICT (tenant_id, source, legacy_kind, legacy_key) DO NOTHING RETURNING 1`, s.qs, resolved), s.tenant, string(data))); err != nil {
			return err
		}
		// A concurrent import may have written the same alias meanwhile: a new
		// statement sees it (READ COMMITTED; stricter levels fail the insert
		// with a serialization error instead). Roll back on this error.
		if err := queryOne(ctx, q, fmt.Sprintf(`SELECT count(*) FROM %[2]s
 JOIN %[1]s.content_codes c ON c.tenant_id = $1 AND c.content_kind = r.kind AND c.content_id = r.id
 WHERE NOT EXISTS (SELECT 1 FROM %[1]s.content_code_aliases a WHERE a.tenant_id = $1 AND a.source = r.source
  AND a.legacy_kind = r.legacy_kind AND a.legacy_key = r.legacy_key AND a.code = c.code AND a.locator IS NOT DISTINCT FROM r.locator)`, s.qs, input), []any{s.tenant, string(data)}, &conflicting); err != nil {
			return err
		}
		if conflicting > 0 {
			return fmt.Errorf("%w: %d aliases already name other content", ErrConflict, conflicting)
		}
		return nil
	})
}

func queryOne(ctx context.Context, q codes.Querier, sql string, args []any, dest ...any) error {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return fmt.Errorf("contenturl: no row")
	}
	if err := rows.Scan(dest...); err != nil {
		return err
	}
	return rows.Err()
}

// ResolveAlias returns what an alias names, following merges.
func (s *Store) ResolveAlias(ctx context.Context, source, legacyKind, key string) (AliasMatch, error) {
	if err := validAlias(source, legacyKind, key); err != nil {
		return AliasMatch{}, err
	}
	rows, err := s.q().Query(ctx, fmt.Sprintf(`SELECT code, coalesce(locator, '') FROM %s.content_code_aliases
 WHERE tenant_id = $1 AND source = $2 AND legacy_kind = $3 AND legacy_key = $4`, s.qs), s.tenant, source, legacyKind, key)
	if err != nil {
		return AliasMatch{}, err
	}
	var code, locator string
	found := rows.Next()
	if found {
		err = rows.Scan(&code, &locator)
	}
	rows.Close()
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return AliasMatch{}, err
	}
	if !found {
		return AliasMatch{}, fmt.Errorf("%w: alias %s/%s %q", ErrNotFound, source, legacyKind, key)
	}
	l, err := s.Resolve(ctx, Code(code))
	if err != nil {
		return AliasMatch{}, err
	}
	return AliasMatch{Link: l, Locator: locator}, nil
}
