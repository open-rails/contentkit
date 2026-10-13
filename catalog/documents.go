package catalog

import (
	"context"
	"encoding/json"
	"maps"
	"slices"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
)

// Content items are keyword documents of their own: one per item and
// language it has text in (no version), built by the search worker
// from that text (its last approved text while an edit awaits review) with
// the names of the item's groups as keywords. Drafts and deleted items have
// none; whom a document shows to is the visibility rule's, at query time
// (Viewer.Eligibility).

// DocumentKinds are the content kinds the catalog documents.
func (s *Store) DocumentKinds() []string { return slices.Sorted(maps.Keys(s.cfg.Kinds)) }

// ListDocuments pages the published items of kind with text in language, in
// id order, for backfill.
func (s *Store) ListDocuments(ctx context.Context, kind, language, cursor string, limit int) ([]contentref.ContentRef, string, bool, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id FROM `+s.t.items+` c WHERE c.tenant_id = $1 AND c.kind = $2 AND c.state = 'published' AND c.id > $4
 AND EXISTS (SELECT 1 FROM `+s.t.text+` t WHERE t.tenant_id = c.tenant_id AND t.content_id = c.id AND t.language = $3) ORDER BY c.id LIMIT $5`,
		s.tenant, kind, language, cursor, limit+1)
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
		refs = append(refs, s.Ref(kind, id))
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

// BuildDocuments returns the current document of each published item of
// refs with text in language; the others get none, which deletes theirs.
func (s *Store) BuildDocuments(ctx context.Context, kind, language string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
	var ids []string
	for _, r := range refs {
		if r.TenantID == s.tenant && r.ContentKind == kind && r.Version() == "" {
			ids = append(ids, r.ContentID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id, c.moderation, c.published_snapshot -> $3, t.title, t.aliases,
 ARRAY(SELECT coalesce((SELECT n.name FROM `+s.t.groupNames+` n WHERE n.tenant_id = g.tenant_id AND n.group_id = g.id AND n.kind = 'name'
   ORDER BY n.language = $3 DESC, n.language = $5 DESC, n.language LIMIT 1), g.slug)
  FROM `+s.t.groups+` g WHERE g.tenant_id = c.tenant_id AND g.state = 'active' AND g.id IN (
   SELECT unnest(f.group_ids) FROM `+s.t.facets+` f WHERE f.tenant_id = c.tenant_id AND f.content_id = c.id) ORDER BY g.kind, g.slug)
 FROM `+s.t.items+` c LEFT JOIN `+s.t.text+` t ON t.tenant_id = c.tenant_id AND t.content_id = c.id AND t.language = $3
 WHERE c.tenant_id = $1 AND c.id = ANY($2) AND c.kind = $4 AND c.state = 'published'`, s.tenant, ids, language, kind, s.defaultLanguage())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []search.KeywordDocument
	for rows.Next() {
		var id, moderation string
		var snapshot []byte
		var title *string
		var aliases, keywords []string
		if err := rows.Scan(&id, &moderation, &snapshot, &title, &aliases, &keywords); err != nil {
			return nil, err
		}
		doc := search.KeywordDocument{DocumentKey: search.DocumentKey{ContentRef: s.Ref(kind, id), Language: language}, Keywords: keywords}
		switch {
		case moderation != "approved" && snapshot != nil:
			var t Text
			if err := json.Unmarshal(snapshot, &t); err != nil {
				return nil, err
			}
			doc.Title, doc.Aliases = t.Title, t.Aliases
		case moderation == "approved" && title != nil:
			doc.Title, doc.Aliases = *title, aliases
		default:
			continue
		}
		if len(doc.Aliases) > 64 {
			doc.Aliases = doc.Aliases[:64]
		}
		if len(doc.Keywords) > 256 {
			doc.Keywords = doc.Keywords[:256]
		}
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}
