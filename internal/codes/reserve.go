package codes

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
)

var codeRE = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{9}$`)
var letterRE = regexp.MustCompile(`[A-Z]`)

// ValidCode reports whether code is a content code contentkit_content_code
// can produce.
func ValidCode(code string) bool { return codeRE.MatchString(code) && letterRE.MatchString(code) }

// Reserve gives each key the code an import kept, in the caller's
// transaction, before Put slugs it: a key keeps a code it already has, so a
// different one is ErrConflict, as is a code another key holds.
func Reserve(ctx context.Context, q Querier, qs, tenant string, keep map[Key]string) error {
	if len(keep) == 0 {
		return nil
	}
	type row struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	rows := make([]row, 0, len(keep))
	for k, code := range keep {
		if !ValidCode(code) {
			return fmt.Errorf("%w: code %q", ErrInvalid, code)
		}
		rows = append(rows, row{k.Kind, k.ID, code})
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	input := `jsonb_to_recordset($2::jsonb) AS r(kind text, id text, code text)`
	if err := Drain(q.Query(ctx, fmt.Sprintf(`INSERT INTO %[1]s.content_codes (tenant_id, code, content_kind, content_id)
 SELECT $1, r.code, r.kind, r.id FROM %[2]s ON CONFLICT DO NOTHING RETURNING 1`, qs, input), tenant, string(data))); err != nil {
		return err
	}
	res, err := q.Query(ctx, fmt.Sprintf(`SELECT r.kind, r.id, r.code FROM %[2]s
 LEFT JOIN %[1]s.content_codes c ON c.tenant_id = $1 AND c.content_kind = r.kind AND c.content_id = r.id
 WHERE c.code IS DISTINCT FROM r.code LIMIT 1`, qs, input), tenant, string(data))
	if err != nil {
		return err
	}
	defer res.Close()
	if res.Next() {
		var r row
		if err := res.Scan(&r.Kind, &r.ID, &r.Code); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s %s cannot take code %s: it has another, or another record has it", ErrConflict, r.Kind, r.ID, r.Code)
	}
	return res.Err()
}

// Alias is an identifier of another system resolving to a registered key.
type Alias struct {
	Source  string
	Kind    string
	Key     string
	Locator string
	Target  Key
}

var aliasPartRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// ValidAlias checks an alias's source, identifier space and key.
func ValidAlias(source, kind, key string) error {
	if !aliasPartRE.MatchString(source) || !aliasPartRE.MatchString(kind) {
		return fmt.Errorf("%w: alias source %q / kind %q", ErrInvalid, source, kind)
	}
	if key == "" || len(key) > 512 {
		return fmt.Errorf("%w: alias key must be 1-512 bytes", ErrInvalid)
	}
	return nil
}

// PutAliases records aliases in the caller's transaction. An alias is
// written once: repeating it is a no-op, pointing it at another target (or
// locator) is ErrConflict, a target without a code ErrNotFound; roll back on
// any error.
func PutAliases(ctx context.Context, q Querier, qs, tenant string, aliases []Alias) error {
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
		if err := ValidAlias(a.Source, a.Kind, a.Key); err != nil {
			return err
		}
		rows[i] = row{Source: a.Source, Kind: a.Kind, Key: a.Key, CKind: a.Target.Kind, ID: a.Target.ID}
		if a.Locator != "" {
			rows[i].Locator = &a.Locator
		}
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	resolved := fmt.Sprintf(`SELECT r.source, r.legacy_kind, r.legacy_key, r.locator, c.code
 FROM jsonb_to_recordset($2::jsonb) AS r(source text, legacy_kind text, legacy_key text, locator text, kind text, id text)
 LEFT JOIN %[1]s.content_codes c ON c.tenant_id = $1 AND c.content_kind = r.kind AND c.content_id = r.id`, qs)
	count := func(sql string) (int64, error) {
		res, err := q.Query(ctx, sql, tenant, string(data))
		if err != nil {
			return 0, err
		}
		defer res.Close()
		var n int64
		if res.Next() {
			if err := res.Scan(&n); err != nil {
				return 0, err
			}
		}
		return n, res.Err()
	}
	// Refuse before writing: a target without a code, a batch that disagrees
	// with itself, or an alias that already names something else.
	missing, err := count(`SELECT count(*) FROM (` + resolved + `) r WHERE code IS NULL`)
	if err != nil {
		return err
	}
	if missing > 0 {
		return fmt.Errorf("%w: %d aliases name records without a code", ErrNotFound, missing)
	}
	clash := fmt.Sprintf(`WITH r AS (%[2]s)
SELECT (SELECT count(*) FROM (SELECT 1 FROM r GROUP BY source, legacy_kind, legacy_key HAVING count(DISTINCT (code, locator)) > 1) d)
 + (SELECT count(*) FROM r JOIN %[1]s.content_code_aliases a ON a.tenant_id = $1 AND a.source = r.source AND a.legacy_kind = r.legacy_kind
   AND a.legacy_key = r.legacy_key AND (a.code IS DISTINCT FROM r.code OR a.locator IS DISTINCT FROM r.locator))`, qs, resolved)
	conflicting, err := count(clash)
	if err != nil {
		return err
	}
	if conflicting > 0 {
		return fmt.Errorf("%w: %d aliases already name other records", ErrConflict, conflicting)
	}
	if err := Drain(q.Query(ctx, fmt.Sprintf(`INSERT INTO %[1]s.content_code_aliases (tenant_id, source, legacy_kind, legacy_key, code, locator)
 SELECT DISTINCT ON (source, legacy_kind, legacy_key) $1, source, legacy_kind, legacy_key, code, locator FROM (%[2]s) r
 ON CONFLICT (tenant_id, source, legacy_kind, legacy_key) DO NOTHING RETURNING 1`, qs, resolved), tenant, string(data))); err != nil {
		return err
	}
	// A concurrent import may have written the same alias meanwhile; a new
	// statement sees it under READ COMMITTED.
	if conflicting, err = count(clash); err != nil {
		return err
	}
	if conflicting > 0 {
		return fmt.Errorf("%w: %d aliases already name other records", ErrConflict, conflicting)
	}
	return nil
}
