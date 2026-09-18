package taxonomy

import (
	"context"
	"fmt"
	"sort"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
	"github.com/open-rails/contentkit/worker"
)

type nodeKey struct {
	kind string
	id   TaxonomyID
}

// ref is the document reference of a node: its kind as content kind, its id
// as content id, no version.
func (s *Store) ref(k nodeKey) contentref.ContentRef {
	return contentref.New(s.tenant, k.kind, string(k.id))
}

// markDirty queues one document per configured language for each node
// through the keyword dirty queue; deleted marks remove the documents.
func (s *Store) markDirty(ctx context.Context, q querier, nodes []nodeKey, deleted bool) error {
	marks := make([]search.DirtyMark, 0, len(nodes)*len(s.languages))
	for _, n := range nodes {
		for _, lang := range s.languages {
			marks = append(marks, search.DirtyMark{DocumentKey: search.DocumentKey{ContentRef: s.ref(n), Language: lang}, Deleted: deleted, Reason: "taxonomy"})
		}
	}
	return search.MarkDirty(ctx, q, s.schema, marks)
}

// Limits of search.UpsertKeywordDocuments.
const (
	maxAliases  = 64
	maxKeywords = 256
)

// BuildKeywordDocuments is the worker.BuildKeywordDocuments of taxonomy
// kinds: one document per active node and language titled by that language's
// canonical name (falling back to English, then any language), with the
// language's aliases and every other name as aliases and overflow as
// keywords. Missing, merged and deleted nodes yield no document.
func (s *Store) BuildKeywordDocuments(ctx context.Context, tenant, kind, language string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
	if tenant != s.tenant {
		return nil, fmt.Errorf("%w: tenant %s", ErrInvalid, tenant)
	}
	if err := s.requireKind(kind); err != nil {
		return nil, err
	}
	language, err := normalizeLanguage(language)
	if err != nil {
		return nil, err
	}
	ids := make([]TaxonomyID, 0, len(refs))
	for _, ref := range refs {
		if ref.TenantID != tenant || ref.ContentKind != kind || ref.Version() != "" {
			return nil, fmt.Errorf("%w: %s is not a %s node reference", ErrInvalid, ref, kind)
		}
		ids = append(ids, TaxonomyID(ref.ContentID))
	}
	list, err := uniqueIDs(ids)
	if err != nil {
		return nil, err
	}
	out := []search.KeywordDocument{}
	if len(list) == 0 {
		return out, nil
	}
	err = s.read(ctx, func(q querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT n.taxonomy_id FROM %s n WHERE n.tenant_id=$1 AND n.kind=$2 AND n.state='active' AND n.taxonomy_id=ANY($3::text[])`, s.table("content_nodes")), s.tenant, kind, list)
		if err != nil {
			return err
		}
		defer rows.Close()
		var active []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			active = append(active, id)
		}
		if err := rows.Err(); err != nil || len(active) == 0 {
			return err
		}
		rows.Close()
		names, err := s.names(ctx, q, active)
		if err != nil {
			return err
		}
		sort.Strings(active)
		for _, id := range active {
			doc := s.document(nodeKey{kind, TaxonomyID(id)}, language, names[TaxonomyID(id)])
			if doc.Title != "" {
				out = append(out, doc)
			}
		}
		return nil
	})
	return out, err
}

func (s *Store) document(k nodeKey, language string, names []Name) search.KeywordDocument {
	doc := search.KeywordDocument{DocumentKey: search.DocumentKey{ContentRef: s.ref(k), Language: language}}
	var title string
	titleRank := 4
	for _, n := range names {
		if n.Kind != NameCanonical {
			continue
		}
		rank := 3
		switch n.Language {
		case language:
			rank = 1
		case "en":
			rank = 2
		}
		if rank < titleRank {
			title, titleRank = n.Name, rank
		}
	}
	if title == "" {
		return doc
	}
	doc.Title = title
	seen := map[string]struct{}{title: {}}
	add := func(n Name) {
		if _, dup := seen[n.Name]; dup {
			return
		}
		seen[n.Name] = struct{}{}
		if len(doc.Aliases) < maxAliases {
			doc.Aliases = append(doc.Aliases, n.Name)
		} else if len(doc.Keywords) < maxKeywords {
			doc.Keywords = append(doc.Keywords, n.Name)
		}
	}
	// Same-language aliases first, then other languages' canonical names, then their aliases.
	for _, n := range names {
		if n.Language == language {
			add(n)
		}
	}
	for _, n := range names {
		if n.Language != language && n.Kind == NameCanonical {
			add(n)
		}
	}
	for _, n := range names {
		if n.Language != language && n.Kind != NameCanonical {
			add(n)
		}
	}
	return doc
}

// ListContent is the worker.ListContentPage of taxonomy kinds: active nodes
// of the kind in taxonomy_id order.
func (s *Store) ListContent(ctx context.Context, tenant, kind, language, cursor string, limit int) ([]contentref.ContentRef, string, bool, error) {
	if tenant != s.tenant {
		return nil, "", false, fmt.Errorf("%w: tenant %s", ErrInvalid, tenant)
	}
	if err := s.requireKind(kind); err != nil {
		return nil, "", false, err
	}
	if limit <= 0 {
		limit = 1000
	}
	var refs []contentref.ContentRef
	err := s.read(ctx, func(q querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT taxonomy_id FROM %s WHERE tenant_id=$1 AND kind=$2 AND state='active' AND taxonomy_id>$3 ORDER BY taxonomy_id LIMIT $4`, s.table("content_nodes")), s.tenant, kind, cursor, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			refs = append(refs, s.ref(nodeKey{kind, TaxonomyID(id)}))
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", false, err
	}
	if len(refs) < limit {
		return refs, "", true, nil
	}
	return refs, refs[len(refs)-1].ContentID, false, nil
}

// Builder returns a worker builder that serves this store's kinds and
// delegates every other kind to next (the host's content builder).
func (s *Store) Builder(next worker.BuildKeywordDocuments) worker.BuildKeywordDocuments {
	return func(ctx context.Context, tenant, kind, language string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
		if s.isKind(kind) {
			return s.BuildKeywordDocuments(ctx, tenant, kind, language, refs)
		}
		if next == nil {
			return nil, fmt.Errorf("taxonomy: no builder for content kind %q", kind)
		}
		return next(ctx, tenant, kind, language, refs)
	}
}

// Lister returns a worker lister that enumerates this store's kinds for
// backfill and delegates every other kind to next.
func (s *Store) Lister(next worker.ListContentPage) worker.ListContentPage {
	return func(ctx context.Context, tenant, kind, language, cursor string, limit int) ([]contentref.ContentRef, string, bool, error) {
		if s.isKind(kind) {
			return s.ListContent(ctx, tenant, kind, language, cursor, limit)
		}
		if next == nil {
			return nil, "", false, fmt.Errorf("taxonomy: no lister for content kind %q", kind)
		}
		return next(ctx, tenant, kind, language, cursor, limit)
	}
}
