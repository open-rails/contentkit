// Package codes writes the content-code registry (content_codes) for the
// packages that own content: contenturl for host content, taxonomy and content
// for ContentKit's own records. Codes are generated in Postgres
// (contentkit_content_code), so one generator serves runtime and migrations.
package codes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"golang.org/x/text/unicode/norm"
)

var (
	ErrInvalid  = errors.New("contenturl: invalid input")
	ErrNotFound = errors.New("contenturl: not found")
	ErrConflict = errors.New("contenturl: conflict")
)

// MaxSlug is the longest slug, in bytes (slugs are ASCII).
const MaxSlug = 80

var (
	slugRE     = regexp.MustCompile(`^([a-z0-9]+(-[a-z0-9]+)*)?$`)
	languageRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,15}$`)
)

var folds = map[rune]string{
	'ß': "ss", 'ẞ': "ss", 'æ': "ae", 'Æ': "ae", 'œ': "oe", 'Œ': "oe", 'ø': "o", 'Ø': "o",
	'đ': "d", 'Đ': "d", 'ð': "d", 'Ð': "d", 'ł': "l", 'Ł': "l", 'þ': "th", 'Þ': "th", 'ı': "i",
}

// Slugify turns text into a URL slug: NFKD, diacritics dropped, a few letters
// folded (ß -> ss), apostrophes removed, every other run of non [a-z0-9] one
// hyphen, at most MaxSlug bytes cut at a word boundary. Text without Latin
// letters or digits gives "".
func Slugify(s string) string {
	var b strings.Builder
	sep := false
	emit := func(r rune) {
		if sep && b.Len() > 0 {
			b.WriteByte('-')
		}
		sep = false
		b.WriteRune(r)
	}
	for _, r := range norm.NFKD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r), r == '\'', r == '‘', r == '’', r == 'ʼ':
			continue
		case folds[r] != "":
			for _, f := range folds[r] {
				emit(f)
			}
			continue
		}
		r = unicode.ToLower(r)
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			emit(r)
		} else {
			sep = true
		}
	}
	out := b.String()
	if len(out) > MaxSlug {
		if cut := strings.LastIndexByte(out[:MaxSlug+1], '-'); cut > 0 {
			out = out[:cut]
		} else {
			out = out[:MaxSlug]
		}
	}
	return strings.TrimRight(out, "-")
}

// ValidSlug reports whether s is a slug Slugify can produce ("" included).
func ValidSlug(s string) bool { return len(s) <= MaxSlug && slugRE.MatchString(s) }

// Language normalizes a slug language key.
func Language(l string) (string, error) {
	l = strings.ToLower(strings.TrimSpace(l))
	if !languageRE.MatchString(l) {
		return "", fmt.Errorf("%w: language %q", ErrInvalid, l)
	}
	return l, nil
}

// Rows is the row iterator the registry reads.
type Rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}

// Querier runs one statement. Every registry statement returns rows.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
}

// PgxQuerier is a pgx pool, connection or transaction.
type PgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type pgxQ struct{ q PgxQuerier }

func (p pgxQ) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	return p.q.Query(ctx, sql, args...)
}

// Pgx adapts a pgx pool, connection or transaction.
func Pgx(q PgxQuerier) Querier { return pgxQ{q} }

type sqlQ struct{ tx *sql.Tx }
type sqlRows struct{ *sql.Rows }

func (r sqlRows) Close() { _ = r.Rows.Close() }

func (s sqlQ) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	rows, err := s.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return sqlRows{rows}, nil
}

// SQL adapts a database/sql transaction (pgx stdlib driver).
func SQL(tx *sql.Tx) Querier { return sqlQ{tx} }

// Drain consumes and closes rows.
func Drain(rows Rows, err error) error {
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

// Key names content within a tenant.
type Key struct{ Kind, ID string }

// Entry is one content record's slugs. Slugs nil keeps the stored per-language
// slugs; a non-nil map replaces them.
type Entry struct {
	Key
	Slug  string
	Slugs map[string]string
}

type entryJSON struct {
	Kind  string            `json:"kind"`
	ID    string            `json:"id"`
	Slug  string            `json:"slug"`
	Slugs map[string]string `json:"slugs"`
}

const putAttempts = 8

// Put assigns a code to every entry without one and sets its slugs, in the
// caller's transaction; it returns every entry's code. qs is the quoted
// schema. Code collisions and, under READ COMMITTED, concurrent registrations
// of the same content are retried; stricter isolation levels report the
// latter as a serialization failure for the caller to retry.
func Put(ctx context.Context, q Querier, qs, tenant string, entries []Entry) (map[Key]string, error) {
	pending := make(map[Key]entryJSON, len(entries))
	for _, e := range entries {
		if e.Kind == "" || e.ID == "" || e.ID != strings.ToLower(e.ID) {
			return nil, fmt.Errorf("%w: content %q/%q", ErrInvalid, e.Kind, e.ID)
		}
		if !ValidSlug(e.Slug) {
			return nil, fmt.Errorf("%w: slug %q", ErrInvalid, e.Slug)
		}
		var slugs map[string]string
		if e.Slugs != nil {
			slugs = make(map[string]string, len(e.Slugs))
			for l, s := range e.Slugs {
				l, err := Language(l)
				if err != nil {
					return nil, err
				}
				if !ValidSlug(s) {
					return nil, fmt.Errorf("%w: slug %q", ErrInvalid, s)
				}
				if s != "" {
					slugs[l] = s
				}
			}
		}
		pending[e.Key] = entryJSON{Kind: e.Kind, ID: e.ID, Slug: e.Slug, Slugs: slugs}
	}
	out := make(map[Key]string, len(pending))
	stmt := fmt.Sprintf(`WITH r AS (SELECT * FROM jsonb_to_recordset($2::jsonb) AS r(kind text, id text, slug text, slugs jsonb)),
upd AS (
 UPDATE %[1]s.content_codes c SET slug = r.slug, slugs = coalesce(r.slugs, c.slugs), updated_at = now() FROM r
 WHERE c.tenant_id = $1 AND c.content_kind = r.kind AND c.content_id = r.id
 AND (c.slug <> r.slug OR (r.slugs IS NOT NULL AND c.slugs <> r.slugs))
 RETURNING c.code),
ins AS (
 INSERT INTO %[1]s.content_codes (tenant_id, code, content_kind, content_id, slug, slugs)
 SELECT $1, %[1]s.contentkit_content_code(), r.kind, r.id, r.slug, coalesce(r.slugs, '{}'::jsonb) FROM r
 WHERE NOT EXISTS (SELECT 1 FROM %[1]s.content_codes c WHERE c.tenant_id = $1 AND c.content_kind = r.kind AND c.content_id = r.id)
 ON CONFLICT DO NOTHING
 RETURNING content_kind, content_id, code)
SELECT content_kind, content_id, code FROM ins
UNION ALL
SELECT c.content_kind, c.content_id, c.code FROM %[1]s.content_codes c JOIN r ON c.tenant_id = $1 AND c.content_kind = r.kind AND c.content_id = r.id`, qs)
	for attempt := 0; len(pending) > 0; attempt++ {
		if attempt == putAttempts {
			return nil, fmt.Errorf("contenturl: %d content codes still unassigned after %d attempts", len(pending), putAttempts)
		}
		batch := make([]entryJSON, 0, len(pending))
		for _, e := range pending {
			batch = append(batch, e)
		}
		data, err := json.Marshal(batch)
		if err != nil {
			return nil, err
		}
		rows, err := q.Query(ctx, stmt, tenant, string(data))
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k Key
			var code string
			if err := rows.Scan(&k.Kind, &k.ID, &code); err != nil {
				rows.Close()
				return nil, err
			}
			out[k] = code
			delete(pending, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Merge makes from's code redirect to into's final code and re-points codes
// already redirected to from, so every redirect is one hop. ErrNotFound when
// either has no code; ErrConflict when into already redirects to from.
func Merge(ctx context.Context, q Querier, qs, tenant string, from, into Key) error {
	if from == into {
		return fmt.Errorf("%w: cannot merge content into itself", ErrInvalid)
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`SELECT content_kind, content_id, code, coalesce(merged_into, '') FROM %s.content_codes
 WHERE tenant_id = $1 AND ((content_kind = $2 AND content_id = $3) OR (content_kind = $4 AND content_id = $5))
 ORDER BY code FOR UPDATE`, qs), tenant, from.Kind, from.ID, into.Kind, into.ID)
	if err != nil {
		return err
	}
	type row struct{ code, merged string }
	found := map[Key]row{}
	for rows.Next() {
		var k Key
		var r row
		if err := rows.Scan(&k.Kind, &k.ID, &r.code, &r.merged); err != nil {
			rows.Close()
			return err
		}
		found[k] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	src, ok := found[from]
	if !ok {
		return fmt.Errorf("%w: no code for %s/%s", ErrNotFound, from.Kind, from.ID)
	}
	dst, ok := found[into]
	if !ok {
		return fmt.Errorf("%w: no code for %s/%s", ErrNotFound, into.Kind, into.ID)
	}
	target := dst.code
	if dst.merged != "" {
		target = dst.merged
	}
	if target == src.code {
		return fmt.Errorf("%w: %s/%s already redirects to %s/%s", ErrConflict, into.Kind, into.ID, from.Kind, from.ID)
	}
	return Drain(q.Query(ctx, fmt.Sprintf(`UPDATE %s.content_codes SET merged_into = $3, updated_at = now()
 WHERE tenant_id = $1 AND (code = $2 OR merged_into = $2) RETURNING code`, qs), tenant, src.code, target))
}
