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
	// Pool is released before every host callback, so one connection
	// suffices. Required.
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

// documentIdentity compares version values, independent of host pointer allocation.
type documentIdentity struct {
	contentref.ContentKey
	language string
}

func identity(key search.DocumentKey) documentIdentity {
	return documentIdentity{key.ContentRef.Key(), key.Language}
}

type dirtyRow struct {
	search.DocumentKey
	IsDeleted bool
	Reason    string
	Revision  int64
}

// SyncOnce runs one tick: drain one dirty batch, then a bounded backfill step.
// No connection or transaction is held across host callbacks or sink calls:
// each write is a short transaction fenced by the dirty-queue revision, so
// overlapping ticks are safe (only duplicated work) and a stale build never
// overwrites a newer one. Sink calls run after the document commits and
// before its row is acknowledged.
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
	w := syncer{cfg: cfg, qs: qs}
	if err := w.dirty(ctx); err != nil {
		return err
	}
	return w.backfill(ctx)
}

type syncer struct {
	cfg Options
	qs  string
}

type buildGroup struct{ kind, language string }

// dirty publishes one batch: deletions first, then rebuilds grouped per kind
// and language for batch-shaped host callbacks.
func (w syncer) dirty(ctx context.Context) error {
	batch, err := w.readDirty(ctx)
	if err != nil {
		return err
	}
	var deletions []dirtyRow
	grouped := map[buildGroup][]dirtyRow{}
	var order []buildGroup
	for _, r := range batch {
		if r.IsDeleted {
			deletions = append(deletions, r)
			continue
		}
		g := buildGroup{r.ContentKind, r.Language}
		if _, ok := grouped[g]; !ok {
			order = append(order, g)
		}
		grouped[g] = append(grouped[g], r)
	}
	if err := w.publish(ctx, deletions, nil); err != nil {
		return err
	}
	for _, g := range order {
		members := grouped[g]
		refs := make([]contentref.ContentRef, 0, len(members))
		for _, r := range members {
			refs = append(refs, r.ContentRef)
		}
		built, err := w.cfg.BuildKeywordDocuments(ctx, w.cfg.Tenant, g.kind, g.language, refs)
		if err != nil {
			return err
		}
		byKey := make(map[documentIdentity]search.KeywordDocument, len(built))
		for _, doc := range built {
			if doc.TenantID != w.cfg.Tenant || doc.ContentKind != g.kind || doc.Language != g.language {
				return fmt.Errorf("worker: builder returned %s/%s outside the requested %s/%s", doc.ContentRef, doc.Language, g.kind, g.language)
			}
			byKey[identity(doc.DocumentKey)] = doc
		}
		if err := w.publish(ctx, members, byKey); err != nil {
			return err
		}
	}
	return nil
}

func (w syncer) readDirty(ctx context.Context) ([]dirtyRow, error) {
	rows, err := w.cfg.Pool.Query(ctx, fmt.Sprintf(`
		SELECT content_kind, content_id, content_version_id, language, is_deleted, reason, revision
		FROM %s.content_search_dirty
		WHERE tenant_id = $1
		ORDER BY updated_at ASC
		LIMIT $2
	`, w.qs), w.cfg.Tenant, w.cfg.DirtyBatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var batch []dirtyRow
	for rows.Next() {
		var (
			r       dirtyRow
			version string
		)
		if err := rows.Scan(&r.ContentKind, &r.ContentID, &version, &r.Language, &r.IsDeleted, &r.Reason, &r.Revision); err != nil {
			return nil, err
		}
		r.ContentRef = contentref.NewVersion(w.cfg.Tenant, r.ContentKind, r.ContentID, version)
		batch = append(batch, r)
	}
	return batch, rows.Err()
}

type queueRow struct {
	ContentKind      string `json:"content_kind"`
	ContentID        string `json:"content_id"`
	ContentVersionID string `json:"content_version_id"`
	Language         string `json:"language"`
	Revision         int64  `json:"revision"`
}

const queueColumns = `(content_kind text,content_id text,content_version_id text,language text,revision bigint)`

func queueRows(rows []dirtyRow) ([]byte, error) {
	out := make([]queueRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, queueRow{r.ContentKind, r.ContentID, r.Version(), r.Language, r.Revision})
	}
	return json.Marshal(out)
}

// publish writes the documents of the rows whose revision is still current
// (a row missing from built publishes an empty document, which deletes), then
// delivers them to the sink and acknowledges them. Locking the current rows
// holds back a concurrent host mark until the write commits, so its newer
// revision is always published after this one.
func (w syncer) publish(ctx context.Context, rows []dirtyRow, built map[documentIdentity]search.KeywordDocument) error {
	if len(rows) == 0 {
		return nil
	}
	data, err := queueRows(rows)
	if err != nil {
		return err
	}
	type publication struct {
		doc      search.KeywordDocument
		revision int64
	}
	var eligible []publication
	err = pgx.BeginFunc(ctx, w.cfg.Pool, func(tx pgx.Tx) error {
		current := map[documentIdentity]bool{}
		locked, err := tx.Query(ctx, fmt.Sprintf(`SELECT d.content_kind, d.content_id, d.content_version_id, d.language
 FROM %s.content_search_dirty d JOIN jsonb_to_recordset($2::jsonb) AS r%s
 ON d.content_kind=r.content_kind AND d.content_id=r.content_id AND d.content_version_id=r.content_version_id AND d.language=r.language AND d.revision=r.revision
 WHERE d.tenant_id=$1 ORDER BY 1,2,3,4 FOR UPDATE OF d`, w.qs, queueColumns), w.cfg.Tenant, data)
		if err != nil {
			return err
		}
		defer locked.Close()
		for locked.Next() {
			var kind, id, version, language string
			if err := locked.Scan(&kind, &id, &version, &language); err != nil {
				return err
			}
			current[identity(search.DocumentKey{ContentRef: contentref.NewVersion(w.cfg.Tenant, kind, id, version), Language: language})] = true
		}
		if err := locked.Err(); err != nil {
			return err
		}
		docs := make([]search.KeywordDocument, 0, len(current))
		for _, r := range rows {
			if !current[identity(r.DocumentKey)] {
				continue
			}
			doc, ok := built[identity(r.DocumentKey)]
			if !ok || r.IsDeleted {
				doc = search.KeywordDocument{DocumentKey: r.DocumentKey}
			}
			docs = append(docs, doc)
			eligible = append(eligible, publication{doc, r.Revision})
		}
		return search.UpsertKeywordDocuments(ctx, tx, w.cfg.Schema, docs)
	})
	if err != nil {
		return err
	}
	var acked, retry []dirtyRow
	for _, p := range eligible {
		r := dirtyRow{DocumentKey: p.doc.DocumentKey, Revision: p.revision}
		if w.deliver(ctx, p.doc, p.revision) {
			acked = append(acked, r)
		} else {
			retry = append(retry, r)
		}
	}
	return w.ack(ctx, acked, retry)
}

// deliver reports whether the sink accepted the document (or there is none).
func (w syncer) deliver(ctx context.Context, doc search.KeywordDocument, revision int64) bool {
	if w.cfg.Sink == nil {
		return true
	}
	if strings.TrimSpace(doc.Title) == "" {
		return w.cfg.Sink.Delete(ctx, doc.DocumentKey, revision) == nil
	}
	return w.cfg.Sink.Upsert(ctx, search.PublishedDocument{KeywordDocument: doc, Version: revision}) == nil
}

// ack removes delivered rows and requeues failed deliveries under a new
// revision; either applies only while the published revision is current.
func (w syncer) ack(ctx context.Context, acked, retry []dirtyRow) error {
	match := `d.tenant_id=$1 AND d.content_kind=r.content_kind AND d.content_id=r.content_id AND d.content_version_id=r.content_version_id AND d.language=r.language AND d.revision=r.revision`
	if len(acked) > 0 {
		data, err := queueRows(acked)
		if err != nil {
			return err
		}
		if _, err := w.cfg.Pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s.content_search_dirty d USING jsonb_to_recordset($2::jsonb) AS r%s WHERE %s`, w.qs, queueColumns, match), w.cfg.Tenant, data); err != nil {
			return err
		}
	}
	if len(retry) > 0 {
		data, err := queueRows(retry)
		if err != nil {
			return err
		}
		if _, err := w.cfg.Pool.Exec(ctx, fmt.Sprintf(`UPDATE %s.content_search_dirty d SET reason='sink_retry', updated_at=now() FROM jsonb_to_recordset($2::jsonb) AS r%s WHERE %s`, w.qs, queueColumns, match), w.cfg.Tenant, data); err != nil {
			return err
		}
	}
	return nil
}

// backfill lists a bounded number of pages and queues them through the
// revision-fenced dirty queue. Each page commits with a compare-and-set of
// its cursor, so an overlapping tick never rewinds or re-queues a page.
func (w syncer) backfill(ctx context.Context) error {
	cfg := w.cfg
	if cfg.BackfillMaxPages <= 0 || cfg.BackfillPageSize <= 0 {
		return nil
	}
	pagesDone := 0
	for _, kind := range cfg.ContentKinds {
		for _, lang := range cfg.SupportedLanguages {
			if pagesDone >= cfg.BackfillMaxPages {
				return nil
			}
			if strings.TrimSpace(lang) == "" || strings.TrimSpace(kind) == "" {
				continue
			}
			cursor, state, err := w.backfillState(ctx, kind, lang)
			if err != nil {
				return err
			}
			if state == "done" {
				continue
			}
			where := `tenant_id = $1 AND content_kind = $2 AND language = $3 AND cursor = $4 AND state = $5`
			refs, nextCursor, done, err := cfg.ListContent(ctx, cfg.Tenant, kind, lang, cursor, cfg.BackfillPageSize)
			if err != nil {
				_, _ = cfg.Pool.Exec(ctx, fmt.Sprintf(`UPDATE %s.content_search_backfill SET last_error = $6, state = 'failed', updated_at = now() WHERE %s`, w.qs, where),
					cfg.Tenant, kind, lang, cursor, state, err.Error())
				return err
			}
			rows := make([]map[string]string, 0, len(refs))
			for _, ref := range refs {
				if ref.TenantID != cfg.Tenant || ref.ContentKind != kind {
					return fmt.Errorf("worker: ListContent returned %s outside the requested %s/%s", ref, cfg.Tenant, kind)
				}
				rows = append(rows, map[string]string{"content_id": ref.ContentID, "content_version_id": ref.Version()})
			}
			data, err := json.Marshal(rows)
			if err != nil {
				return err
			}
			next := "running"
			if done {
				next = "done"
			}
			err = pgx.BeginFunc(ctx, cfg.Pool, func(tx pgx.Tx) error {
				tag, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.content_search_backfill SET cursor = $6, state = $7, last_error = NULL, updated_at = now() WHERE %s`, w.qs, where),
					cfg.Tenant, kind, lang, cursor, state, nextCursor, next)
				if err != nil || tag.RowsAffected() == 0 {
					return err // Another tick advanced this cursor.
				}
				// A pending row keeps its revision.
				_, err = tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.content_search_dirty(tenant_id,content_kind,content_id,content_version_id,language,reason)
 SELECT $1, $2, r.content_id, r.content_version_id, $3, 'backfill' FROM jsonb_to_recordset($4::jsonb) AS r(content_id text, content_version_id text)
 ON CONFLICT(tenant_id,content_kind,content_id,content_version_id,language) DO NOTHING`, w.qs), cfg.Tenant, kind, lang, data)
				return err
			})
			if err != nil {
				return err
			}
			pagesDone++
		}
	}
	return nil
}

func (w syncer) backfillState(ctx context.Context, kind, language string) (cursor, state string, err error) {
	err = w.cfg.Pool.QueryRow(ctx, fmt.Sprintf(`WITH ins AS (
 INSERT INTO %[1]s.content_search_backfill (tenant_id, content_kind, language) VALUES ($1, $2, $3)
 ON CONFLICT (tenant_id, content_kind, language) DO NOTHING RETURNING cursor, state)
 SELECT cursor, state FROM ins UNION ALL
 SELECT cursor, state FROM %[1]s.content_search_backfill WHERE tenant_id = $1 AND content_kind = $2 AND language = $3`, w.qs), w.cfg.Tenant, kind, language).Scan(&cursor, &state)
	return cursor, state, err
}
