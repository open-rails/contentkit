package discovery

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/entities"
)

// ListingSummary describes this query's selection before cursor pagination.
// Only optional entity preferences are relaxed to count filtered contents.
type ListingSummary struct {
	FilteredCount    int                     `json:"filtered_count"`
	FilteredEntities []ReleaseFilteredEntity `json:"filtered_entities"`
	LockedCount      int                     `json:"locked_count"`
	// Total is the contents the list pages through under its access mode.
	Total int `json:"total"`
	// Truncated makes counts lower bounds for bounded keyword or recommendation
	// candidate windows. Ordinary lists have exact selection counts.
	Truncated bool `json:"truncated,omitempty"`
	// unknown: some access went unsettled (billing unavailable).
	unknown bool
}

// summaryCandidates optionally bounds a keyword selection independently before
// and after preferences. Nil slices mean no candidate bound; empty non-nil
// slices intentionally select nothing.
type summaryCandidates struct {
	visible, relaxed []string
	truncated        bool
	selection        func(*sel) *sel
}

func (s *Service) listingSummary(ctx context.Context, filter Filter, selected *sel, sort order, candidates *summaryCandidates) (*ListingSummary, error) {
	out := &ListingSummary{FilteredEntities: []ReleaseFilteredEntity{}}
	reader := selected.reader
	visible, err := s.compileWithViewer(ctx, reader, filter, selected.viewer)
	if err != nil {
		return nil, err
	}
	if candidates != nil {
		if candidates.selection != nil {
			visible = candidates.selection(visible)
		}
		visible = summaryCandidateSelection(visible, candidates.visible)
		out.Truncated = candidates.truncated
	}
	visibleIDs := map[string]struct{}{}
	out.unknown, err = s.summaryRows(ctx, visible, sort, nil, func(row cardRow) {
		visibleIDs[row.ID] = struct{}{}
		if !row.Access.Allowed && !row.Access.Unknown {
			switch row.Access.Level {
			case access.TierLevel, access.EntityLevel, access.PPV, access.EntityPPV:
				out.LockedCount++
			}
		}
	})
	if err != nil {
		return nil, err
	}
	out.Total = len(visibleIDs)
	hidden := visible.viewer.HiddenContentFilters()
	var ids []entities.EntityID
	for kind, entries := range hidden {
		if filter.Kind != "" && kind != filter.Kind {
			continue
		}
		for _, id := range entries {
			if !slices.Contains(ids, entities.EntityID(id)) {
				ids = append(ids, entities.EntityID(id))
			}
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	entities, err := s.o.Entities.Cards(ctx, ids, entities.CardOptions{Language: visible.display})
	if err != nil {
		return nil, err
	}
	for _, entity := range entities {
		out.FilteredEntities = append(out.FilteredEntities, ReleaseFilteredEntity{Entity: entity})
	}
	slices.SortFunc(out.FilteredEntities, func(a, b ReleaseFilteredEntity) int {
		if n := strings.Compare(strings.ToLower(a.Entity.Name), strings.ToLower(b.Entity.Name)); n != 0 {
			return n
		}
		return strings.Compare(string(a.Entity.ID), string(b.Entity.ID))
	})
	relaxed, err := s.compileWithViewer(ctx, reader, filter, selected.viewer.WithoutContentFilters())
	if err != nil {
		return nil, err
	}
	if candidates != nil {
		if candidates.selection != nil {
			relaxed = candidates.selection(relaxed)
		}
		relaxed = summaryCandidateSelection(relaxed, candidates.relaxed)
	}
	var counted []string
	for _, entry := range out.FilteredEntities {
		counted = append(counted, string(entry.Entity.ID))
	}
	unknown, err := s.summaryRows(ctx, relaxed, sort, counted, func(row cardRow) {
		// A visible matching edition already represents this content, even
		// when a sibling edition has an optional hidden entity.
		if _, ok := visibleIDs[row.ID]; ok {
			return
		}
		out.FilteredCount++
		for i := range out.FilteredEntities {
			entry := &out.FilteredEntities[i]
			if slices.Contains(hidden[row.Kind], string(entry.Entity.ID)) && slices.Contains(row.entityIDs, string(entry.Entity.ID)) {
				entry.Count++
			}
		}
	})
	out.unknown = out.unknown || unknown
	return out, err
}

func summaryCandidateSelection(q *sel, ids []string) *sel {
	if ids == nil {
		return q
	}
	return q.with("f.content_id = ANY(@summary_candidates::text[])", pgx.NamedArgs{"summary_candidates": ids})
}

// summaryRows visits each selected content once, with the version its card
// would show, settled and redacted exactly as cards are. It reads only what
// the counts need, in one query: no titles, codes, media or offers.
// It also reports unresolved access, including rows withheld from the visitor.
// Rows carry, of their entities, those visit counts by and those that can open
// them.
func (s *Service) summaryRows(ctx context.Context, q *sel, sort order, counted []string, visit func(cardRow)) (bool, error) {
	args := maps.Clone(q.args)
	for key, value := range sort.args {
		args[key] = value
	}
	pickSQL := fmt.Sprintf(`SELECT DISTINCT ON (f.content_id) f.content_id, f.version_id FROM %s f WHERE %s ORDER BY f.content_id, %s`,
		s.t.facets, q.predicate(), preference)
	if name, _, _ := strings.Cut(sort.raw, ":"); name == SortPopular {
		// Popularity is per content: score each content's chosen edition once.
		pickSQL = fmt.Sprintf(`SELECT f.content_id, f.version_id FROM (
 SELECT DISTINCT ON (f.content_id) f.tenant_id, f.kind, f.content_id, f.version_id FROM %s f WHERE %s ORDER BY f.content_id, %s) f %s
WHERE (%s) > 0`, s.t.facets, q.predicate(), preference, sort.join, sort.expr)
	}
	// One read of the whole selection; access is then settled a batch at a time.
	args["summary_tenant"], args["summary_user"] = s.o.Tenant, person(q.reader.Actor)
	// Only a held entity opens a row; against complete holdings no other
	// entity changes a decision.
	entityIDs := "f.entity_ids"
	if filter, err := s.o.Gate.Filter(ctx, q.reader.Actor); err == nil && filter.Complete() {
		args["summary_entities"] = append(filter.Entities(), counted...)
		entityIDs = "ARRAY(SELECT e FROM unnest(f.entity_ids) e WHERE e = ANY(@summary_entities::text[]))"
	}
	rows, err := s.o.Pool.Query(ctx, fmt.Sprintf(`SELECT c.id, c.kind, c.access, v.access, v.id, %[6]s, coalesce(f.owner_entity_id, ''), f.published_at,
 @summary_user <> '' AND EXISTS (SELECT 1 FROM %[4]s cc WHERE cc.tenant_id = c.tenant_id AND cc.content_id = c.id AND cc.user_id = @summary_user)
 FROM (%[5]s) p
 JOIN %[3]s v ON v.tenant_id = @summary_tenant AND v.id = p.version_id
 JOIN %[1]s f ON f.tenant_id = v.tenant_id AND f.version_id = v.id
 JOIN %[2]s c ON c.tenant_id = f.tenant_id AND c.id = f.content_id`, s.t.facets, s.t.items, s.t.versions, s.t.creators, pickSQL, entityIDs), args)
	if err != nil {
		return false, err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cardRow, error) {
		var c cardRow
		err := r.Scan(&c.ID, &c.Kind, &c.contentRule, &c.override, &c.Version.ID, &c.entityIDs, &c.owner, &c.PublishedAt, &c.creator)
		return c, err
	})
	if err != nil {
		return false, err
	}
	const batchSize = 2048
	unknown := false
	for start := 0; start < len(all); start += batchSize {
		batch := all[start:min(start+batchSize, len(all))]
		if err := s.settle(ctx, q, batch); err != nil {
			return false, err
		}
		for _, row := range batch {
			unknown = unknown || row.Access.Unknown
		}
		batch = slices.DeleteFunc(batch, func(c cardRow) bool {
			return !c.Access.Allowed && q.reader.Mode == access.Hide
		})
		s.lock(batch)
		for _, row := range batch {
			visit(row)
		}
	}
	return unknown, nil
}
