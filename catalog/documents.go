package catalog

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/search"
)

// Content items are keyword documents of their own: one per item and
// language it has text in (no version), built by the search worker
// from that text (its last approved text while an edit awaits review) with
// the names of the item's timeless entities as keywords. Dated curation uses
// runtime entity filters; it does not enter clock-dependent keyword text. Drafts and deleted items have
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
 AND (EXISTS (SELECT 1 FROM `+s.t.text+` t WHERE t.tenant_id = c.tenant_id AND t.content_id = c.id AND t.language = $3)
 OR ($6 AND (coalesce(c.original_title,'')<>'' OR EXISTS (
 SELECT 1 FROM `+s.t.versions+` v JOIN `+s.t.versionText+` t ON t.tenant_id=v.tenant_id AND t.version_id=v.id
 WHERE v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.state='published' AND (v.publish_at IS NULL OR v.publish_at<=clock_timestamp()) AND t.title<>''))))
 ORDER BY c.id LIMIT $5`,
		s.tenant, kind, language, cursor, limit+1, s.cfg.Kinds[kind].TitlePolicy == kinds.TitleOriginalAndVersion)
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
	rows, err := s.pool.Query(ctx, `SELECT c.id, c.moderation, c.published_snapshot -> $3, t.title, t.aliases, coalesce(c.original_title,''),
 ARRAY(SELECT vt.title FROM `+s.t.versions+` v JOIN `+s.t.versionText+` vt ON vt.tenant_id=v.tenant_id AND vt.version_id=v.id
 WHERE v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.state='published' AND (v.publish_at IS NULL OR v.publish_at<=clock_timestamp())
 AND vt.language=ANY(v.languages) AND vt.title<>'' ORDER BY (vt.language=$3) DESC,v.position,vt.language),
 ARRAY(SELECT coalesce((SELECT n.name FROM `+s.t.entityNames+` n WHERE n.tenant_id = g.tenant_id AND n.entity_id = g.id AND n.kind = 'name'
   ORDER BY n.language = $3 DESC, n.language = $5 DESC, n.language LIMIT 1), g.slug)
  FROM `+s.t.entities+` g WHERE g.tenant_id = c.tenant_id AND g.state = 'active' AND NOT g.internal AND g.id = ANY(`+s.qs+`.content_keyword_entity_ids(c.tenant_id,c.id)) ORDER BY g.kind, g.slug)
 FROM `+s.t.items+` c LEFT JOIN `+s.t.text+` t ON t.tenant_id = c.tenant_id AND t.content_id = c.id AND t.language = $3
 WHERE c.tenant_id = $1 AND c.id = ANY($2) AND c.kind = $4 AND c.state = 'published'`, s.tenant, ids, language, kind, s.defaultLanguage())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []search.KeywordDocument
	for rows.Next() {
		var id, moderation, original string
		var snapshot []byte
		var title *string
		var aliases, keywords, editionTitles []string
		if err := rows.Scan(&id, &moderation, &snapshot, &title, &aliases, &original, &editionTitles, &keywords); err != nil {
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
		case moderation == "approved" && (title != nil || s.cfg.Kinds[kind].TitlePolicy == kinds.TitleOriginalAndVersion):
			if title != nil {
				doc.Title = *title
			}
			doc.Aliases = aliases
			if s.cfg.Kinds[kind].TitlePolicy == kinds.TitleOriginalAndVersion {
				if original != "" {
					doc.Aliases = append(doc.Aliases, original)
				}
				if doc.Title != "" {
					doc.Aliases = append(doc.Aliases, doc.Title)
				}
				doc.Aliases = append(doc.Aliases, editionTitles...)
				if len(editionTitles) > 0 {
					doc.Title = editionTitles[0]
				} else if original != "" {
					doc.Title = original
				}
				if doc.Title == "" {
					continue
				}
			}
		default:
			continue
		}
		if s.cfg.Kinds[kind].TitlePolicy == kinds.TitleOriginalAndVersion {
			aliases := []string{}
			for _, alias := range doc.Aliases {
				if strings.TrimSpace(alias) != "" && alias != doc.Title && !slices.Contains(aliases, alias) {
					aliases = append(aliases, alias)
				}
			}
			doc.Aliases = aliases
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
