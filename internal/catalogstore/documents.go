package catalogstore

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/search"
	"github.com/open-rails/contentkit/kinds"
)

func (s *Store) DocumentKinds() []string { return slices.Sorted(maps.Keys(s.cfg.Kinds)) }

// ListDocuments pages edition references for the existing search backfill.
func (s *Store) ListDocuments(ctx context.Context, kind, language, cursor string, limit int) ([]contentref.ContentRef, string, bool, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT v.content_id,v.id FROM `+s.t.versions+` v
 JOIN `+s.t.items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id
 LEFT JOIN `+s.t.versionText+` t ON t.tenant_id=v.tenant_id AND t.version_id=v.id AND t.language=$3
 WHERE c.tenant_id=$1 AND c.kind=$2 AND c.state='published' AND v.state='published' AND v.released
 AND v.id>$4 AND $3=ANY(v.languages) AND (coalesce(t.title,'')<>'' OR ($6 AND coalesce(v.original_title,'')<>''))
 ORDER BY v.id LIMIT $5`, s.tenant, kind, language, cursor, limit+1, s.cfg.Kinds[kind].TitlePolicy == kinds.TitleOriginalAndVersion)
	if err != nil {
		return nil, "", false, err
	}
	defer rows.Close()
	refs := []contentref.ContentRef{}
	for rows.Next() {
		var id, version string
		if err := rows.Scan(&id, &version); err != nil {
			return nil, "", false, err
		}
		refs = append(refs, s.Ref(kind, id).WithVersion(version))
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
		next = refs[len(refs)-1].Version()
	}
	return refs, next, done, nil
}

// BuildDocuments returns exactly the requested editions. A missing, unpublished
// or nonmatching-language edition deletes its own document, not another version.
func (s *Store) BuildDocuments(ctx context.Context, kind, language string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
	var ids, versions []string
	for _, ref := range refs {
		if ref.TenantID == s.tenant && ref.ContentKind == kind && ref.Version() != "" {
			ids = append(ids, ref.ContentID)
			versions = append(versions, ref.Version())
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id,v.id,coalesce(t.title,''),coalesce(t.aliases,'{}'::text[]),coalesce(v.original_title,''),
 ARRAY(SELECT k.name FROM `+s.t.entities+` g CROSS JOIN LATERAL (
  SELECT n.name,n.kind='alias' AS alias,n.language FROM `+s.t.entityNames+` n
  WHERE n.tenant_id=g.tenant_id AND n.entity_id=g.id AND (n.kind='alias' OR n.language IN($3,$5))
  UNION ALL SELECT coalesce((SELECT n.name FROM `+s.t.entityNames+` n WHERE n.tenant_id=g.tenant_id AND n.entity_id=g.id AND n.kind='name' ORDER BY n.language LIMIT 1),g.slug),false,''
  WHERE NOT EXISTS(SELECT 1 FROM `+s.t.entityNames+` n WHERE n.tenant_id=g.tenant_id AND n.entity_id=g.id AND n.kind='name' AND n.language IN($3,$5))) k
 WHERE g.tenant_id=c.tenant_id AND g.state='active' AND NOT g.internal AND g.id=ANY(f.entity_ids)
 ORDER BY k.alias,g.kind,g.slug,k.language=$3 DESC,k.name)
 FROM unnest($2::text[],$6::text[]) wanted(content_id,version_id)
 JOIN `+s.t.items+` c ON c.tenant_id=$1 AND c.id=wanted.content_id
 JOIN `+s.t.versions+` v ON v.tenant_id=c.tenant_id AND v.content_id=c.id AND v.id=wanted.version_id
 JOIN `+s.t.facets+` f ON f.tenant_id=v.tenant_id AND f.version_id=v.id
 LEFT JOIN `+s.t.versionText+` t ON t.tenant_id=v.tenant_id AND t.version_id=v.id AND t.language=$3
 WHERE c.kind=$4 AND c.state='published' AND v.state='published' AND v.released AND $3=ANY(v.languages)`, s.tenant, ids, language, kind, s.defaultLanguage(), versions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []search.KeywordDocument
	for rows.Next() {
		var id, version, title, original string
		var aliases, keywords []string
		if err := rows.Scan(&id, &version, &title, &aliases, &original, &keywords); err != nil {
			return nil, err
		}
		if s.cfg.Kinds[kind].TitlePolicy == kinds.TitleOriginalAndVersion {
			if title == "" {
				title = original
			} else if original != "" {
				aliases = append(aliases, original)
			}
		}
		if title == "" {
			continue
		}
		aliases = uniqueKeywords(aliases)
		aliases = slices.DeleteFunc(aliases, func(alias string) bool { return strings.TrimSpace(alias) == "" || alias == title })
		if len(aliases) > 64 {
			aliases = aliases[:64]
		}
		keywords = uniqueKeywords(keywords)
		if len(keywords) > 256 {
			keywords = keywords[:256]
		}
		docs = append(docs, search.KeywordDocument{DocumentKey: search.DocumentKey{ContentRef: s.Ref(kind, id).WithVersion(version), Language: language}, Title: title, Aliases: aliases, Keywords: keywords})
	}
	return docs, rows.Err()
}

// uniqueKeywords drops repeated keywords, ignoring case, keeping the first.
func uniqueKeywords(keywords []string) []string {
	seen := make(map[string]bool, len(keywords))
	out := keywords[:0]
	for _, k := range keywords {
		if key := strings.ToLower(k); !seen[key] {
			seen[key] = true
			out = append(out, k)
		}
	}
	return out
}
