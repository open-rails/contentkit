package discovery

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/groups"
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
	err = s.summaryRows(ctx, visible, sort, func(row cardRow) {
		visibleIDs[row.ID] = struct{}{}
		if !row.Access.Allowed && !row.Access.Unknown {
			switch row.Access.Level {
			case access.TierLevel, access.GroupLevel, access.PPV, access.GroupPPV:
				out.LockedCount++
			}
		}
	})
	if err != nil {
		return nil, err
	}
	out.Total = len(visibleIDs)
	hidden := visible.viewer.HiddenContentFilters()
	var ids []groups.GroupID
	for kind, entries := range hidden {
		if filter.Kind != "" && kind != filter.Kind {
			continue
		}
		for _, id := range entries {
			if !slices.Contains(ids, groups.GroupID(id)) {
				ids = append(ids, groups.GroupID(id))
			}
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	entities, err := s.o.Groups.Cards(ctx, ids, groups.CardOptions{Language: visible.display})
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
	err = s.summaryRows(ctx, relaxed, sort, func(row cardRow) {
		// A visible matching edition already represents this content, even
		// when a sibling edition has an optional hidden entity.
		if _, ok := visibleIDs[row.ID]; ok {
			return
		}
		out.FilteredCount++
		for i := range out.FilteredEntities {
			entry := &out.FilteredEntities[i]
			if slices.Contains(hidden[row.Kind], string(entry.Entity.ID)) && slices.Contains(row.groupIDs, string(entry.Entity.ID)) {
				entry.Count++
			}
		}
	})
	return out, err
}

func summaryCandidateSelection(q *sel, ids []string) *sel {
	if ids == nil {
		return q
	}
	return q.with("f.content_id = ANY(@summary_candidates::text[])", pgx.NamedArgs{"summary_candidates": ids})
}

// summaryRows does not fetch images or entity names. Each batch uses the same
// exact access settlement and locked-property redaction as visible cards.
// Finish reading picks before hydration, including with a one-connection pool.
func (s *Service) summaryRows(ctx context.Context, q *sel, sort order, visit func(cardRow)) error {
	args := maps.Clone(q.args)
	for key, value := range sort.args {
		args[key] = value
	}
	if name, _, _ := strings.Cut(sort.raw, ":"); name == SortPopular {
		q = q.with("("+sort.expr+") > 0", nil)
	}
	rows, err := s.o.Pool.Query(ctx, fmt.Sprintf(`SELECT DISTINCT ON (f.content_id) f.content_id, f.version_id
 FROM %s f %s WHERE %s ORDER BY f.content_id, %s`, s.t.facets, sort.join, q.predicate(), preference), args)
	if err != nil {
		return err
	}
	picks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pick, error) {
		var p pick
		err := row.Scan(&p.content, &p.version)
		return p, err
	})
	if err != nil {
		return err
	}
	const batchSize = 256
	for start := 0; start < len(picks); start += batchSize {
		batch, err := s.cardRows(ctx, q, picks[start:min(start+batchSize, len(picks))])
		if err != nil {
			return err
		}
		for _, row := range batch {
			visit(row)
		}
	}
	return nil
}
