// Package worker maintains one tenant's keyword documents: it drains the dirty
// queue, runs a bounded cursor backfill and delivers every published document
// to the optional DocumentSink.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
)

// ListContentPage enumerates a tenant's content of one kind and language for
// backfill: one bounded page from an opaque cursor, done when exhausted.
type ListContentPage func(ctx context.Context, tenant, contentKind, language, cursor string, limit int) (refs []contentref.ContentRef, nextCursor string, done bool, err error)

// BuildKeywordDocuments returns the current keyword document of every
// requested reference that exists in the given language. A missing reference
// means the content no longer exists and deletes its document. Return an
// error for failed or incomplete reads; never a partial result.
type BuildKeywordDocuments func(ctx context.Context, tenant, contentKind, language string, refs []contentref.ContentRef) ([]search.KeywordDocument, error)

// Options configures one tenant's worker.
type Options struct {
	// Pool needs at least two connections: SyncOnce reserves one transaction
	// while read-only host callbacks may query through the pool. Required.
	Pool   *pgxpool.Pool
	Schema string
	Tenant string

	// SupportedLanguages are the document languages backfill enumerates. Required.
	SupportedLanguages []string
	// ContentKinds are the kinds backfill enumerates. Dirty rows of any kind
	// are built through BuildKeywordDocuments.
	ContentKinds []string

	// Required.
	ListContent           ListContentPage
	BuildKeywordDocuments BuildKeywordDocuments

	// Sink receives every published or deleted document (see search.DocumentSink).
	Sink search.DocumentSink

	// Batch sizing (defaults are conservative).
	DirtyBatchSize   int
	BackfillPageSize int
	// Upper bound on how much cursor backfill work to do per SyncOnce.
	BackfillMaxPages int
}

func (o Options) withDefaults() Options {
	if o.DirtyBatchSize <= 0 {
		o.DirtyBatchSize = 250
	}
	if o.BackfillPageSize <= 0 {
		o.BackfillPageSize = 1000
	}
	if o.BackfillMaxPages <= 0 {
		o.BackfillMaxPages = 5
	}
	return o
}

type dirtyRow struct {
	search.DocumentKey
	IsDeleted bool
	Reason    string
	Revision  int64
}

// SyncOnce runs one tick: drain the dirty queue, then a bounded backfill step.
// One writer per schema and tenant: a competing tick returns without work.
// Documents, queue acknowledgements and sink deliveries share one transaction.
func SyncOnce(ctx context.Context, opts Options) error {
	cfg := opts.withDefaults()
	if cfg.Pool == nil {
		return fmt.Errorf("worker: pool is required")
	}
	if strings.TrimSpace(cfg.Schema) == "" || strings.TrimSpace(cfg.Tenant) == "" {
		return fmt.Errorf("worker: schema and tenant are required")
	}
	if len(cfg.SupportedLanguages) == 0 {
		return fmt.Errorf("worker: SupportedLanguages is required")
	}
	if cfg.ListContent == nil || cfg.BuildKeywordDocuments == nil {
		return fmt.Errorf("worker: ListContent and BuildKeywordDocuments are required")
	}
	qs, err := search.QuoteSchema(cfg.Schema)
	if err != nil {
		return err
	}
	// One writer per schema/tenant, covering dirty work AND backfills. The
	// transaction owns all document writes and acknowledgements: losing its
	// connection cannot leave an old callback able to overwrite a newer writer.
	// Host callbacks may read through Pool, so reserve another slot.
	if cfg.Pool.Config().MaxConns < 2 {
		return fmt.Errorf("worker: SyncOnce requires at least two pool connections")
	}
	tx, err := cfg.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var acquired bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`, LockKey(cfg.Schema, cfg.Tenant)).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return nil // Another tick owns this schema/tenant; retry next tick.
	}

	// 1) Drain dirty queue (fast path).
	batch, retry, err := processDirtyOnce(ctx, tx, qs, cfg)
	if err != nil {
		return err
	}

	// 2) Bounded backfill tick (slow path).
	if err := backfillOnce(ctx, tx, qs, cfg); err != nil {
		return err
	}

	// Acknowledge only after every callback has finished; waiting on a host's
	// concurrent UPSERT cannot hold queue locks while callbacks need the pool.
	// A row whose sink delivery failed stays queued under a new revision.
	for _, r := range batch {
		var err error
		if _, ok := retry[r.DocumentKey]; ok {
			_, err = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.content_search_dirty SET reason='sink_retry', updated_at=now() WHERE tenant_id=$1 AND content_kind=$2 AND content_id=$3 AND content_version_id=$4 AND language=$5 AND revision=$6`, qs),
				r.TenantID, r.ContentKind, r.ContentID, r.Version(), r.Language, r.Revision)
		} else {
			_, err = tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.content_search_dirty WHERE tenant_id=$1 AND content_kind=$2 AND content_id=$3 AND content_version_id=$4 AND language=$5 AND revision=$6`, qs),
				r.TenantID, r.ContentKind, r.ContentID, r.Version(), r.Language, r.Revision)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// LockKey is the advisory-lock text of one schema/tenant writer.
func LockKey(schema, tenant string) string { return "contentkit:sync:" + schema + ":" + tenant }

type buildGroup struct{ kind, language string }

// processDirtyOnce builds and publishes one batch of dirty rows and returns
// the batch plus the keys whose sink delivery failed.
func processDirtyOnce(ctx context.Context, tx pgx.Tx, qs string, cfg Options) ([]dirtyRow, map[search.DocumentKey]struct{}, error) {
	rows, err := tx.Query(ctx, fmt.Sprintf(`
		SELECT content_kind, content_id, content_version_id, language, is_deleted, reason, revision
		FROM %s.content_search_dirty
		WHERE tenant_id = $1
		ORDER BY updated_at ASC
		LIMIT $2
	`, qs), cfg.Tenant, cfg.DirtyBatchSize)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var batch []dirtyRow
	for rows.Next() {
		var (
			r       dirtyRow
			version string
		)
		if err := rows.Scan(&r.ContentKind, &r.ContentID, &version, &r.Language, &r.IsDeleted, &r.Reason, &r.Revision); err != nil {
			return nil, nil, err
		}
		r.ContentRef = contentref.NewVersion(cfg.Tenant, r.ContentKind, r.ContentID, version)
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows.Close()
	retry := map[search.DocumentKey]struct{}{}
	if len(batch) == 0 {
		return nil, retry, nil
	}
	deliver := func(doc search.KeywordDocument, revision int64) {
		if cfg.Sink == nil {
			return
		}
		var err error
		if doc.Title == "" {
			err = cfg.Sink.Delete(ctx, doc.DocumentKey)
		} else {
			err = cfg.Sink.Upsert(ctx, search.PublishedDocument{KeywordDocument: doc, Version: revision})
		}
		if err != nil {
			retry[doc.DocumentKey] = struct{}{}
		}
	}

	// Deletions first.
	var removed []search.DocumentKey
	for _, r := range batch {
		if !r.IsDeleted {
			continue
		}
		current, err := dirtyRevisionCurrent(ctx, tx, qs, r)
		if err != nil {
			return nil, nil, err
		}
		if current {
			removed = append(removed, r.DocumentKey)
		}
	}
	if err := search.DeleteKeywordDocuments(ctx, tx, cfg.Schema, removed); err != nil {
		return nil, nil, err
	}
	for _, key := range removed {
		deliver(search.KeywordDocument{DocumentKey: key}, 0)
	}

	// Rebuilds, grouped per kind and language for batch-shaped host callbacks.
	grouped := map[buildGroup][]dirtyRow{}
	var order []buildGroup
	for _, r := range batch {
		if r.IsDeleted {
			continue
		}
		g := buildGroup{r.ContentKind, r.Language}
		if _, ok := grouped[g]; !ok {
			order = append(order, g)
		}
		grouped[g] = append(grouped[g], r)
	}
	for _, g := range order {
		members := grouped[g]
		refs := make([]contentref.ContentRef, 0, len(members))
		for _, r := range members {
			refs = append(refs, r.ContentRef)
		}
		built, err := cfg.BuildKeywordDocuments(ctx, cfg.Tenant, g.kind, g.language, refs)
		if err != nil {
			return nil, nil, err
		}
		byKey := make(map[search.DocumentKey]search.KeywordDocument, len(built))
		for _, doc := range built {
			if doc.TenantID != cfg.Tenant || doc.ContentKind != g.kind || doc.Language != g.language {
				return nil, nil, fmt.Errorf("worker: builder returned %s/%s outside the requested %s/%s", doc.ContentRef, doc.Language, g.kind, g.language)
			}
			byKey[doc.DocumentKey] = doc
		}
		// Recheck generations after building. Changed or unsolicited keys are
		// not published; their latest queue entry remains for the next tick.
		// Missing requested keys mean the content no longer exists; an empty
		// document deletes any stale indexed record.
		type publish struct {
			doc      search.KeywordDocument
			revision int64
		}
		var eligible []publish
		for _, r := range members {
			current, err := dirtyRevisionCurrent(ctx, tx, qs, r)
			if err != nil {
				return nil, nil, err
			}
			if !current {
				continue
			}
			doc, ok := byKey[r.DocumentKey]
			if !ok {
				doc = search.KeywordDocument{DocumentKey: r.DocumentKey}
			}
			eligible = append(eligible, publish{doc, r.Revision})
		}
		docs := make([]search.KeywordDocument, 0, len(eligible))
		for _, p := range eligible {
			docs = append(docs, p.doc)
		}
		if err := search.UpsertKeywordDocuments(ctx, tx, cfg.Schema, docs); err != nil {
			return nil, nil, err
		}
		for _, p := range eligible {
			deliver(p.doc, p.revision)
		}
	}
	return batch, retry, nil
}

func backfillOnce(ctx context.Context, tx pgx.Tx, qs string, cfg Options) (retErr error) {
	if cfg.BackfillMaxPages <= 0 || cfg.BackfillPageSize <= 0 {
		return nil
	}
	pagesDone := 0
	type request struct {
		kind, language string
		refs           []contentref.ContentRef
	}
	var pending []request
	// Queue writes happen after all listing callbacks, avoiding pool starvation
	// from host UPSERTs waiting for our newly inserted queue rows.
	defer func() {
		if retErr != nil {
			return
		}
		for _, req := range pending {
			rows := make([]map[string]string, 0, len(req.refs))
			for _, ref := range req.refs {
				if ref.TenantID != cfg.Tenant || ref.ContentKind != req.kind {
					retErr = fmt.Errorf("worker: ListContent returned %s outside the requested %s/%s", ref, cfg.Tenant, req.kind)
					return
				}
				rows = append(rows, map[string]string{"content_id": ref.ContentID, "content_version_id": ref.Version()})
			}
			data, err := json.Marshal(rows)
			if err != nil {
				retErr = err
				return
			}
			// Backfill requests work through the same generation-fenced queue;
			// a pending row keeps its revision.
			if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.content_search_dirty(tenant_id,content_kind,content_id,content_version_id,language,reason)
 SELECT $1, $2, r.content_id, r.content_version_id, $3, 'backfill' FROM jsonb_to_recordset($4::jsonb) AS r(content_id text, content_version_id text)
 ON CONFLICT(tenant_id,content_kind,content_id,content_version_id,language) DO NOTHING`, qs), cfg.Tenant, req.kind, req.language, data); err != nil {
				retErr = err
				return
			}
		}
	}()

	for _, kind := range cfg.ContentKinds {
		for _, lang := range cfg.SupportedLanguages {
			if pagesDone >= cfg.BackfillMaxPages {
				return nil
			}
			if strings.TrimSpace(lang) == "" || strings.TrimSpace(kind) == "" {
				continue
			}
			cursor, state, err := backfillState(ctx, tx, qs, cfg.Tenant, kind, lang)
			if err != nil {
				return err
			}
			if state == "done" {
				continue
			}
			refs, nextCursor, done, err := cfg.ListContent(ctx, cfg.Tenant, kind, lang, cursor, cfg.BackfillPageSize)
			if err != nil {
				_, _ = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.content_search_backfill SET last_error = $4, state = 'failed', updated_at = now()
 WHERE tenant_id = $1 AND content_kind = $2 AND language = $3`, qs), cfg.Tenant, kind, lang, err.Error())
				return err
			}
			if len(refs) > 0 {
				pending = append(pending, request{kind, lang, refs})
			}
			next := "running"
			if done {
				next = "done"
			}
			if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.content_search_backfill SET cursor = $4, state = $5, last_error = NULL, updated_at = now()
 WHERE tenant_id = $1 AND content_kind = $2 AND language = $3`, qs), cfg.Tenant, kind, lang, nextCursor, next); err != nil {
				return err
			}
			pagesDone++
		}
	}
	return nil
}

func backfillState(ctx context.Context, tx pgx.Tx, qs, tenant, kind, language string) (cursor, state string, err error) {
	if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.content_search_backfill (tenant_id, content_kind, language) VALUES ($1, $2, $3)
 ON CONFLICT (tenant_id, content_kind, language) DO NOTHING`, qs), tenant, kind, language); err != nil {
		return "", "", err
	}
	if err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT cursor, state FROM %s.content_search_backfill WHERE tenant_id = $1 AND content_kind = $2 AND language = $3`, qs), tenant, kind, language).Scan(&cursor, &state); err != nil {
		return "", "", err
	}
	return cursor, state, nil
}

func dirtyRevisionCurrent(ctx context.Context, tx pgx.Tx, qs string, r dirtyRow) (bool, error) {
	var current bool
	err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s.content_search_dirty WHERE tenant_id=$1 AND content_kind=$2 AND content_id=$3 AND content_version_id=$4 AND language=$5 AND revision=$6)`, qs),
		r.TenantID, r.ContentKind, r.ContentID, r.Version(), r.Language, r.Revision).Scan(&current)
	return current, err
}
