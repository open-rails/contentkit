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
	out.unknown, err = s.summaryRows(ctx, visible, sort, func(row cardRow) {
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
	unknown, err := s.summaryRows(ctx, relaxed, sort, func(row cardRow) {
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
func (s *Service) summaryRows(ctx context.Context, q *sel, sort order, visit func(cardRow)) (bool, error) {
	args := maps.Clone(q.args)
	for key, value := range sort.args {
		args[key] = value
	}
	sql := fmt.Sprintf(`SELECT DISTINCT ON (f.content_id) f.content_id, f.version_id FROM %s f WHERE %s ORDER BY f.content_id, %s`,
		s.t.facets, q.predicate(), preference)
	if name, _, _ := strings.Cut(sort.raw, ":"); name == SortPopular {
		// Popularity is per content: score each content's chosen edition once.
		sql = fmt.Sprintf(`SELECT f.content_id, f.version_id FROM (
 SELECT DISTINCT ON (f.content_id) f.tenant_id, f.kind, f.content_id, f.version_id FROM %s f WHERE %s ORDER BY f.content_id, %s) f %s
WHERE (%s) > 0`, s.t.facets, q.predicate(), preference, sort.join, sort.expr)
	}
	rows, err := s.o.Pool.Query(ctx, sql, args)
	if err != nil {
		return false, err
	}
	picks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pick, error) {
		var p pick
		return p, r.Scan(&p.content, &p.version)
	})
	if err != nil {
		return false, err
	}
	// Rows load and settle a batch at a time; only the picks span the selection.
	const batchSize = 2048
	lean := fmt.Sprintf(`SELECT c.id, c.kind, c.access, v.access, v.id, f.group_ids, f.listing, coalesce(f.owner_group_id, ''), f.published_at,
 $3 <> '' AND EXISTS (SELECT 1 FROM %[4]s cc WHERE cc.tenant_id = c.tenant_id AND cc.content_id = c.id AND cc.user_id = $3)
 FROM unnest($2::text[]) AS p(vid)
 JOIN %[1]s f ON f.tenant_id = $1 AND f.version_id = p.vid
 JOIN %[2]s c ON c.tenant_id = f.tenant_id AND c.id = f.content_id
 JOIN %[3]s v ON v.tenant_id = f.tenant_id AND v.id = f.version_id`, s.t.facets, s.t.items, s.t.versions, s.t.creators)
	user := person(q.reader.Actor)
	unknown := false
	for start := 0; start < len(picks); start += batchSize {
		vids := make([]string, 0, batchSize)
		for _, p := range picks[start:min(start+batchSize, len(picks))] {
			vids = append(vids, p.version)
		}
		rows, err := s.o.Pool.Query(ctx, lean, s.o.Tenant, vids, user)
		if err != nil {
			return false, err
		}
		batch, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cardRow, error) {
			var c cardRow
			err := r.Scan(&c.ID, &c.Kind, &c.contentRule, &c.override, &c.Version.ID, &c.groupIDs, &c.listing, &c.owner, &c.PublishedAt, &c.creator)
			return c, err
		})
		if err != nil {
			return false, err
		}
		if err := s.settle(ctx, q, batch); err != nil {
			return false, err
		}
		for _, row := range batch {
			unknown = unknown || row.Access.Unknown
		}
		batch = slices.DeleteFunc(batch, func(c cardRow) bool {
			return !c.Access.Allowed && (q.reader.Mode == access.Hide || c.listing == "members")
		})
		s.lock(batch)
		for _, row := range batch {
			visit(row)
		}
	}
	return unknown, nil
}
