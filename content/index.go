package content

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
)

// Posts are ContentKit's own keyword documents: every post write queues the
// post's DocumentKey (kind "post", the post's language) in the keyword
// schema's dirty queue inside the write transaction, and the worker rebuilds
// it through KeywordDocuments. A post without a language is not a document.

func (p *posts) markDirty(ctx context.Context, tx pgx.Tx, id, language string, deleted bool) error {
	if p.rt.searchSchema == "" || language == "" {
		return nil
	}
	return search.MarkDirty(ctx, tx, p.rt.searchSchema, []search.DirtyMark{{
		DocumentKey: search.DocumentKey{ContentRef: p.rt.Ref(KindPost, id), Language: language},
		Deleted:     deleted,
		Reason:      "post",
	}})
}

// KeywordDocuments returns the current keyword document of every published
// post in refs for language (the worker's BuildKeywordDocuments shape for kind
// "post"). A draft, unpublished, deleted or other-language post yields no
// document, which deletes its stale index entry; so does a held or rejected one.
func (rt *Runtime) KeywordDocuments(ctx context.Context, tenant, kind, language string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
	if tenant != rt.tenant || kind != KindPost {
		return nil, fmt.Errorf("content: KeywordDocuments serves %s/%s, not %s/%s", rt.tenant, KindPost, tenant, kind)
	}
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		if err := rt.checkRef(r); err != nil {
			return nil, err
		}
		ids = append(ids, r.ContentID)
	}
	rows, err := rt.store.pool.Query(ctx, `SELECT id, title, slug FROM `+rt.store.t.posts+`
		WHERE tenant_id = $1 AND language = $2 AND id = ANY($3) AND deleted_at IS NULL AND is_draft = false
		AND moderation = 'approved' AND (live_at IS NULL OR live_at <= now())`, rt.tenant, language, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []search.KeywordDocument
	for rows.Next() {
		var id, title string
		var slug *string
		if err := rows.Scan(&id, &title, &slug); err != nil {
			return nil, err
		}
		doc := search.KeywordDocument{DocumentKey: search.DocumentKey{ContentRef: rt.Ref(KindPost, id), Language: language}, Title: title}
		if slug != nil && *slug != "" {
			doc.Aliases = []string{*slug}
		}
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

// ListContent enumerates the tenant's published posts of language in id order
// (the worker's ListContentPage shape for kind "post"); cursor is the last id.
func (rt *Runtime) ListContent(ctx context.Context, tenant, kind, language, cursor string, limit int) ([]contentref.ContentRef, string, bool, error) {
	if tenant != rt.tenant || kind != KindPost {
		return nil, "", false, fmt.Errorf("content: ListContent serves %s/%s, not %s/%s", rt.tenant, KindPost, tenant, kind)
	}
	if limit <= 0 {
		limit = 1000
	}
	rows, err := rt.store.pool.Query(ctx, `SELECT id FROM `+rt.store.t.posts+`
		WHERE tenant_id = $1 AND language = $2 AND id > $3 AND deleted_at IS NULL AND is_draft = false
		AND moderation = 'approved' AND (live_at IS NULL OR live_at <= now()) ORDER BY id LIMIT $4`, rt.tenant, language, cursor, limit+1)
	if err != nil {
		return nil, "", false, err
	}
	defer rows.Close()
	var refs []contentref.ContentRef
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, "", false, err
		}
		refs = append(refs, rt.Ref(KindPost, id))
	}
	if err := rows.Err(); err != nil {
		return nil, "", false, err
	}
	done := len(refs) <= limit
	if !done {
		refs = refs[:limit]
	}
	next := cursor
	if len(refs) > 0 {
		next = refs[len(refs)-1].ContentID
	}
	return refs, next, done, nil
}
