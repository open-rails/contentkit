package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// VersionReferenceOwner clears host links before versions are permanently
// erased. Calls use the catalog mutation's transaction with parent contents
// locked. An error rolls back the purge. Unpublish and soft deletion do not call
// this hook: hosts can retain their selection while publication is suspended.
type VersionReferenceOwner interface {
	PurgeVersions(ctx context.Context, q Querier, catalog string, versionIDs []string) error
}

// VersionReferences supplies storage-only joins and validation for host-owned
// links to exact catalog editions. It does not authorize host operations or
// grant reader access. The host must authorize its own edit before using it.
type VersionReferences struct {
	tenant, items, versions, facets string
}

func NewVersionReferences(schema, tenant string) (*VersionReferences, error) {
	if strings.TrimSpace(schema) == "" || strings.TrimSpace(tenant) == "" {
		return nil, fmt.Errorf("%w: schema and catalog are required", ErrInvalid)
	}
	table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	return &VersionReferences{tenant: tenant, items: table("content_items"), versions: table("content_versions"), facets: table("content_facets")}, nil
}

// PublicationSQL is a SELECT suitable for a host query's derived table. Join on
// BOTH catalog_key and version_id; version identity is catalog-scoped. The
// returned columns are catalog_key, version_id, content_id, kind, languages,
// published_at and published. Publication includes the parent, scheduling,
// moderation and soft deletion, but not a viewer's entitlement or preferences.
// It is evaluated at the database statement's time, before host pagination.
// Hosts must not write this relation or depend on its underlying table names.
func (r *VersionReferences) PublicationSQL() string {
	return `SELECT tenant_id AS catalog_key, version_id, content_id, kind, languages,
 live_from AS published_at, coalesce(NOT deleted AND live_from <= statement_timestamp(), false) AS published
 FROM ` + r.facets
}

// LockPublishedSQLTx validates and reserves a currently published edition of
// kind in language. It returns its owning content ID. Locks are parent first,
// edition second, and must precede any host row locks. This serializes selection
// with publication changes and permanent purge. It does not follow aliases.
// Use READ COMMITTED and roll back on error; tx is never committed here.
func (r *VersionReferences) LockPublishedSQLTx(ctx context.Context, tx *sql.Tx, versionID, kind, language string) (string, error) {
	if tx == nil || versionID == "" || kind == "" || language == "" {
		return "", fmt.Errorf("%w: transaction, edition, kind and language are required", ErrInvalid)
	}
	var isolation string
	if err := tx.QueryRowContext(ctx, "SHOW transaction_isolation").Scan(&isolation); err != nil {
		return "", err
	}
	if isolation != "read committed" {
		return "", fmt.Errorf("%w: edition references require READ COMMITTED", ErrInvalid)
	}
	var contentID string
	err := tx.QueryRowContext(ctx, `SELECT c.id FROM `+r.items+` c
 WHERE c.tenant_id=$1 AND EXISTS (SELECT 1 FROM `+r.versions+` v
 WHERE v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.id=$2) FOR SHARE OF c`, r.tenant, versionID).Scan(&contentID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	var locked string
	err = tx.QueryRowContext(ctx, `SELECT id FROM `+r.versions+`
 WHERE tenant_id=$1 AND content_id=$2 AND id=$3 FOR SHARE`, r.tenant, contentID, versionID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	// Read after waiting on both locks so a concurrent unpublish/purge cannot
	// pass using the earlier statement's snapshot.
	var eligible bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM (`+r.PublicationSQL()+`) p
 WHERE p.catalog_key=$1 AND p.version_id=$2 AND p.kind=$3 AND $4=ANY(p.languages) AND p.published)`,
		r.tenant, versionID, kind, language).Scan(&eligible)
	if err != nil {
		return "", err
	}
	if !eligible {
		return "", fmt.Errorf("%w: edition is not published in the requested kind and language", ErrConflict)
	}
	return contentID, nil
}
