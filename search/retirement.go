package search

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var legacySearchIndexes = []string{
	"content_search_documents_keyword_exact", "content_search_documents_keyword_fuzzy",
	"content_search_documents_keyword_native", "content_search_documents_name_prefix",
}

var currentSearchIndexes = []struct{ name, table string }{
	{"content_search_documents_search_exact", "content_search_documents"},
	{"content_search_documents_search_fuzzy", "content_search_documents"},
	{"content_search_documents_search_native", "content_search_documents"},
	{"content_search_documents_search_substring", "content_search_documents"},
	{"content_search_documents_names_prefix", "content_search_documents"},
	{"content_search_documents_names_substring", "content_search_documents"},
	{"content_text_management_prefix", "content_text"},
	{"content_text_management_substring", "content_text"},
	{"content_group_names_management_prefix", "content_group_names"},
	{"content_group_names_management_substring", "content_group_names"},
}

// RetireLegacyIndexes removes four superseded search indexes and their two
// wrappers after every legacy reader/writer has stopped. This is an explicit
// operator action, never a migration/startup step. Replacement indexes must be
// healthy; unexpected tracked SQL dependencies prevent any planned drops. The identity
// normalizer and all current search/management indexes remain untouched.
//
// Drops are concurrent and individually committed, so a canceled/failed pass
// is safe to retry. Use an operator pool with DDL authority and a bounded ctx.
func RetireLegacyIndexes(ctx context.Context, pool *pgxpool.Pool, schema string, legacyWritersStopped bool) error {
	if !legacyWritersStopped {
		return fmt.Errorf("search: stop legacy readers/writers before retiring search indexes")
	}
	if pool == nil {
		return fmt.Errorf("search: database is required")
	}
	schema = strings.TrimSpace(schema)
	qs, err := QuoteSchema(schema)
	if err != nil {
		return err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	// One connection also avoids deadlocking one-connection operator pools.
	var locked bool
	lock := "contentkit:retire-search:" + schema
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, lock).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("search: index retirement is already running for %s", schema)
	}
	defer func() {
		unlock, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlock, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, lock); err != nil {
			// Do not return a session lock to the pool after a canceled connection.
			_ = conn.Conn().Close(unlock)
		}
	}()
	for _, index := range currentSearchIndexes {
		var healthy bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_index
 WHERE indexrelid=to_regclass($1) AND indrelid=to_regclass($2) AND indisvalid AND indisready AND indislive)`,
			pgx.Identifier{schema, index.name}.Sanitize(), pgx.Identifier{schema, index.table}.Sanitize()).Scan(&healthy); err != nil {
			return err
		}
		if !healthy {
			return fmt.Errorf("search: replacement index %s.%s is missing or not ready", schema, index.name)
		}
	}
	oldNames := make([]string, len(legacySearchIndexes))
	for i, name := range legacySearchIndexes {
		oldNames[i] = pgx.Identifier{schema, name}.Sanitize()
	}
	wrappers := []string{"contentkit_keyword_terms", "contentkit_keyword_text"}
	for _, name := range wrappers {
		signature := qs + "." + name + "(text,text[],text[],text)"
		// Inspect PostgreSQL's actual dependency graph, including SQL-standard
		// RETURN bodies/views/generated expressions, rather than trusting Go refs.
		var dependency string
		err := conn.QueryRow(ctx, `SELECT pg_describe_object(d.classid,d.objid,d.objsubid) FROM pg_depend d
 WHERE d.refclassid='pg_proc'::regclass AND d.refobjid=to_regprocedure($1)
 AND NOT EXISTS(SELECT 1 FROM unnest($2::text[]) AS indexes(name)
 WHERE d.classid='pg_class'::regclass AND d.objid=to_regclass(indexes.name)) LIMIT 1`, signature, oldNames).Scan(&dependency)
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
		if dependency != "" {
			return fmt.Errorf("search: %s has an unexpected SQL dependency: %s", signature, dependency)
		}
	}
	for _, name := range oldNames {
		if _, err := conn.Exec(ctx, "DROP INDEX CONCURRENTLY IF EXISTS "+name); err != nil {
			return fmt.Errorf("search: retire %s: %w", name, err)
		}
	}
	for _, name := range wrappers {
		if _, err := conn.Exec(ctx, "DROP FUNCTION IF EXISTS "+qs+"."+name+"(text,text[],text[],text) RESTRICT"); err != nil {
			return fmt.Errorf("search: retire %s: %w", name, err)
		}
	}
	return nil
}
