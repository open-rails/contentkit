package pg

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// DocumentExecutor allows a pool or transaction to own lexical document writes.
type DocumentExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

const searchDocumentsTable = "search_documents"

// UpsertSearchDocuments adapts existing string builders to the canonical writer.
// The complete string is a title; new hosts should supply explicit fields using
// UpsertKeywordDocuments. Both writers replace aliases and keywords atomically.
func UpsertSearchDocuments(ctx context.Context, pool DocumentExecutor, schema string, entityType string, language string, docs map[string]string) error {
	structured := make(map[string]KeywordDocument, len(docs))
	for id, text := range docs {
		structured[id] = KeywordDocument{Title: text}
	}
	return UpsertKeywordDocuments(ctx, pool, schema, entityType, language, structured)
}

func DeleteSearchDocuments(ctx context.Context, pool DocumentExecutor, schema string, entityType string, entityID string, language string) error {
	return DeleteSearchDocumentsMany(ctx, pool, schema, entityType, []string{entityID}, language)
}

func DeleteSearchDocumentsMany(ctx context.Context, pool DocumentExecutor, schema string, entityType string, entityIDs []string, language string) error {
	if pool == nil {
		return fmt.Errorf("pool is required")
	}
	if strings.TrimSpace(schema) == "" {
		return fmt.Errorf("schema is required")
	}
	if strings.TrimSpace(entityType) == "" {
		return fmt.Errorf("entityType is required")
	}
	if strings.TrimSpace(language) == "" {
		return fmt.Errorf("language is required")
	}
	if len(entityIDs) == 0 {
		return nil
	}
	qs, err := quoteIdent(schema)
	if err != nil {
		return fmt.Errorf("invalid schema: %w", err)
	}
	q := fmt.Sprintf(`
		DELETE FROM %s.%s
		WHERE entity_type = $1 AND language = $2 AND entity_id = ANY($3::text[])
	`, qs, searchDocumentsTable)
	_, err = pool.Exec(ctx, q, entityType, language, entityIDs)
	return err
}
