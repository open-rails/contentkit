package entities

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/internal/codes"
)

// References maintains a host's links to catalog entities. Calls run with the
// affected entities locked, in the same transaction as the catalog mutation.
// Implementations must use q, not another connection, for all database work;
// an error aborts the mutation. Tenant is the store's catalog key.
// Reference-aware mutations require READ COMMITTED transactions so checks see
// host links committed while waiting for an entity's lock. Callers of Tx methods
// must roll back their transaction on error.
type References interface {
	// CheckUnavailable rejects deletion or public-to-internal reclassification
	// while host links still require the entities. Return ErrConflict to refuse.
	CheckUnavailable(ctx context.Context, q Querier, tenant string, refs []Ref) error
	// Merge moves From's host links to Into, collapsing duplicate pairs.
	Merge(ctx context.Context, q Querier, tenant string, from, into Ref) error
}

// LockReferencesTx validates active, non-internal entities of the exact requested
// kinds and locks them until tx ends. A host must call this before writing its
// own links, in that same transaction, to serialize with merges and deletion.
// Pass the complete selection together so locks are acquired in ID order.
// This is a storage contract, not authorization: callers authorize the host
// operation before starting tx. It does not follow merged IDs or create entities.
// tx must use READ COMMITTED isolation; roll it back on error.
func (s *Store) LockReferencesTx(ctx context.Context, tx pgx.Tx, refs []Ref) error {
	if tx == nil {
		return fmt.Errorf("%w: transaction is required", ErrInvalid)
	}
	if len(refs) == 0 {
		return nil
	}
	if err := checkReferenceTransaction(ctx, tx); err != nil {
		return err
	}
	return s.lockReferences(ctx, codes.Pgx(tx), refs)
}

// LockReferencesSQLTx is LockReferencesTx for a database/sql transaction using
// the pgx stdlib driver. Pass the underlying *sql.Tx when using an ORM. It never
// starts or ends a transaction; the caller must roll back on error.
func (s *Store) LockReferencesSQLTx(ctx context.Context, tx *sql.Tx, refs []Ref) error {
	if err := checkSQLReferenceTransaction(ctx, tx); err != nil {
		return err
	}
	if len(refs) == 0 {
		return nil
	}
	return s.lockReferences(ctx, codes.SQL(tx), refs)
}

// MatchExactNameSQLTx finds up to two distinct active, non-internal entities of
// kind within scope whose canonical name or alias equals name literally. Case,
// accents, whitespace and punctuation are significant. Empty language searches
// all languages. Zero matches means unresolved; two means ambiguous.
//
// This is a lookup, not a reservation of the name or authorization. It neither
// creates entities nor follows merged IDs. Before writing host links, pass the
// complete resolved selection to LockReferencesSQLTx in this same READ COMMITTED
// transaction. Keep the original text for unresolved or ambiguous names.
func (s *Store) MatchExactNameSQLTx(ctx context.Context, tx *sql.Tx, kind, scopeID, language, name string) ([]string, error) {
	if err := checkSQLReferenceTransaction(ctx, tx); err != nil {
		return nil, err
	}
	if _, err := s.entityKind(kind); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT DISTINCT g.id FROM %s g
 JOIN %s n ON n.tenant_id=g.tenant_id AND n.entity_id=g.id
 WHERE g.tenant_id=$1 AND g.kind=$2 AND g.scope_id=$3 AND g.state='active' AND NOT g.internal
   AND ($4='' OR n.language=$4) AND n.name COLLATE "C" = $5 COLLATE "C"
 ORDER BY g.id LIMIT 2`, s.table("content_entities"), s.table("content_entity_names")), s.tenant, kind, scopeID, language, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) lockReferences(ctx context.Context, q codes.Querier, refs []Ref) error {
	ids := make([]EntityID, len(refs))
	wanted := make(map[EntityID]string, len(refs))
	for i, ref := range refs {
		if _, err := s.entityKind(ref.Kind); err != nil {
			return err
		}
		if previous, ok := wanted[ref.ID]; ok && previous != ref.Kind {
			return fmt.Errorf("%w: group %s has conflicting kinds", ErrInvalid, ref.ID)
		}
		wanted[ref.ID] = ref.Kind
		ids[i] = ref.ID
	}
	list, err := uniqueIDs(ids)
	if err != nil {
		return err
	}
	rows, err := q.Query(ctx, `SELECT g.id, g.kind, g.state, g.internal FROM `+s.table("content_entities")+` g
 WHERE g.tenant_id = $1 AND g.id = ANY($2::text[]) ORDER BY g.id FOR SHARE`, s.tenant, list)
	if err != nil {
		return err
	}
	defer rows.Close()
	var locked []Entity
	for rows.Next() {
		var g Entity
		if err := rows.Scan(&g.ID, &g.Kind, &g.State, &g.Internal); err != nil {
			return err
		}
		locked = append(locked, g)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(locked) != len(list) {
		return ErrNotFound
	}
	for _, g := range locked {
		if g.Kind != wanted[g.ID] {
			return fmt.Errorf("%w: group %s is not a %s", ErrInvalid, g.ID, wanted[g.ID])
		}
		if g.State != StateActive || g.Internal {
			return fmt.Errorf("%w: group %s is unavailable", ErrConflict, g.ID)
		}
	}
	return nil
}

func checkSQLReferenceTransaction(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return fmt.Errorf("%w: transaction is required", ErrInvalid)
	}
	var isolation string
	if err := tx.QueryRowContext(ctx, "SHOW transaction_isolation").Scan(&isolation); err != nil {
		return err
	}
	if isolation != "read committed" {
		return fmt.Errorf("%w: host group references require READ COMMITTED isolation", ErrInvalid)
	}
	return nil
}

func checkReferenceTransaction(ctx context.Context, q Querier) error {
	var isolation string
	if err := q.QueryRow(ctx, "SHOW transaction_isolation").Scan(&isolation); err != nil {
		return err
	}
	if isolation != "read committed" {
		return fmt.Errorf("%w: host group references require READ COMMITTED isolation", ErrInvalid)
	}
	return nil
}
