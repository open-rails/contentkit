package groups

import (
	"context"
	"fmt"
	"sort"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/search"
)

// ref is the document and code reference of a group: its kind as content
// kind, its id as content id.
func (s *Store) ref(kind string, id GroupID) contentref.ContentRef {
	return contentref.New(s.tenant, kind, string(id))
}

// markDirty queues one keyword document per configured language for each
// group; groups that are not active have theirs deleted.
func (s *Store) markDirty(ctx context.Context, q Querier, groups []Group) error {
	marks := make([]search.DirtyMark, 0, len(groups)*len(s.cfg.Languages))
	for _, g := range groups {
		for _, lang := range s.cfg.Languages {
			marks = append(marks, search.DirtyMark{DocumentKey: search.DocumentKey{ContentRef: s.ref(g.Kind, g.ID), Language: lang},
				Deleted: g.State != StateActive, Reason: "groups"})
		}
	}
	return search.MarkDirty(ctx, q, s.schema, marks)
}

// Limits of search.UpsertKeywordDocuments.
const (
	maxAliases  = 64
	maxKeywords = 256
)

// IsKind reports whether kind is a configured group kind: the worker routes
// its documents here.
func (s *Store) IsKind(kind string) bool {
	_, ok := s.cfg.GroupKinds[kind]
	return ok
}

// BuildKeywordDocuments is the worker.BuildKeywordDocuments of group kinds:
// one document per active group and language, titled by that language's
// canonical name (falling back to English, then any language), with the
// language's aliases and every other name as aliases and overflow as
// keywords. Missing, merged and deleted groups yield no document.
func (s *Store) BuildKeywordDocuments(ctx context.Context, tenant, kind, language string, refs []contentref.ContentRef) ([]search.KeywordDocument, error) {
	if tenant != s.tenant {
		return nil, fmt.Errorf("%w: tenant %s", ErrInvalid, tenant)
	}
	if _, err := s.groupKind(kind); err != nil {
		return nil, err
	}
	language, err := normalizeLanguage(language)
	if err != nil {
		return nil, err
	}
	ids := make([]GroupID, 0, len(refs))
	for _, ref := range refs {
		if ref.TenantID != tenant || ref.ContentKind != kind || ref.Version() != "" {
			return nil, fmt.Errorf("%w: %s is not a %s group reference", ErrInvalid, ref, kind)
		}
		ids = append(ids, GroupID(ref.ContentID))
	}
	list, err := uniqueIDs(ids)
	if err != nil {
		return nil, err
	}
	out := []search.KeywordDocument{}
	if len(list) == 0 {
		return out, nil
	}
	err = s.read(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT id FROM %s WHERE tenant_id = $1 AND kind = $2 AND state = 'active' AND id = ANY($3::text[])`,
			s.table("content_groups")), s.tenant, kind, list)
		if err != nil {
			return err
		}
		active, err := collectStrings(rows)
		if err != nil || len(active) == 0 {
			return err
		}
		names, err := s.names(ctx, q, active)
		if err != nil {
			return err
		}
		sort.Strings(active)
		for _, id := range active {
			if doc := s.document(kind, GroupID(id), language, names[GroupID(id)]); doc.Title != "" {
				out = append(out, doc)
			}
		}
		return nil
	})
	return out, err
}

func (s *Store) document(kind string, id GroupID, language string, names []Name) search.KeywordDocument {
	doc := search.KeywordDocument{DocumentKey: search.DocumentKey{ContentRef: s.ref(kind, id), Language: language}}
	title := displayName(names, language, nil)
	if title.Name == "" {
		return doc
	}
	doc.Title = title.Name
	seen := map[string]struct{}{title.Name: {}}
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

// displayName is the canonical name in language, else in English, else in
// the first of fallback that has one, else in any language.
func displayName(names []Name, language string, fallback []string) Name {
	rank := func(l string) int {
		switch l {
		case language:
			return 0
		case "en":
			return 1
		}
		for i, f := range fallback {
			if f == l {
				return 2 + i
			}
		}
		return 2 + len(fallback)
	}
	var best Name
	bestRank := -1
	for _, n := range names {
		if n.Kind != NameCanonical {
			continue
		}
		if r := rank(n.Language); bestRank < 0 || r < bestRank {
			best, bestRank = n, r
		}
	}
	return best
}

// ListContent is the worker.ListContentPage of group kinds: active groups of
// the kind in id order.
func (s *Store) ListContent(ctx context.Context, tenant, kind, language, cursor string, limit int) ([]contentref.ContentRef, string, bool, error) {
	if tenant != s.tenant {
		return nil, "", false, fmt.Errorf("%w: tenant %s", ErrInvalid, tenant)
	}
	if _, err := s.groupKind(kind); err != nil {
		return nil, "", false, err
	}
	if limit <= 0 {
		limit = 1000
	}
	var refs []contentref.ContentRef
	err := s.read(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, fmt.Sprintf(`SELECT id FROM %s WHERE tenant_id = $1 AND kind = $2 AND state = 'active' AND id > $3 ORDER BY id LIMIT $4`,
			s.table("content_groups")), s.tenant, kind, cursor, limit)
		if err != nil {
			return err
		}
		ids, err := collectStrings(rows)
		for _, id := range ids {
			refs = append(refs, s.ref(kind, GroupID(id)))
		}
		return err
	})
	if err != nil {
		return nil, "", false, err
	}
	if len(refs) < limit {
		return refs, "", true, nil
	}
	return refs, refs[len(refs)-1].ContentID, false, nil
}
