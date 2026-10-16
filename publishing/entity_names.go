package publishing

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// entityNameSeparators deliberately ignores word separators only. Removing all
// punctuation would conflate distinct names such as C/C++ or AC/DC and ACDC.
// Case, accents and Unicode compatibility follow the existing catalog rules.
const entityNameSeparators = `[[:space:]_‐‑‒–—―-]+`

// namedEntityIDs matches both canonical names and registered aliases in one
// catalog, kind, language and parent scope. It returns up to two distinct IDs:
// two means ambiguity, never permission to guess or merge existing entities.
// Its expression is also the publishing-name lookup index in migration 0006.
func (s *Store) namedEntityIDs(ctx context.Context, tx pgx.Tx, kind, scopeID, language, name string) ([]string, error) {
	entities := pgx.Identifier{s.o.Schema, "content_groups"}.Sanitize()
	names := pgx.Identifier{s.o.Schema, "content_group_names"}.Sanitize()
	normalize := pgx.Identifier{s.o.Schema, "contentkit_keyword_normalize"}.Sanitize()
	query := fmt.Sprintf(`SELECT DISTINCT g.id FROM %s g JOIN %s n ON n.tenant_id=g.tenant_id AND n.group_id=g.id
WHERE g.tenant_id=$1 AND g.kind=$2 AND g.scope_id=$3 AND g.state='active' AND n.language=$4
  AND regexp_replace(n.normalized, '%s', '', 'g')=regexp_replace(%s($5), '%s', '', 'g')
ORDER BY g.id LIMIT 2`, entities, names, entityNameSeparators, normalize, entityNameSeparators)
	rows, err := tx.Query(ctx, query, s.o.Tenant, kind, scopeID, language, name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
