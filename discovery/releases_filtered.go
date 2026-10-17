package discovery

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/groups"
)

// filteredReleases relaxes only optional preferences for a private selection.
// No omitted content IDs, metadata or covers leave this count path.
func (s *Service) filteredReleases(ctx context.Context, out *DailyReleases, filter Filter, visible *sel, visibleIDs []string, from, until, now time.Time) error {
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
		return nil
	}
	entities, err := s.o.Groups.Cards(ctx, ids, groups.CardOptions{Language: visible.display})
	if err != nil {
		return err
	}
	byID := make(map[string]groups.GroupCard, len(entities))
	for _, entity := range entities {
		byID[string(entity.ID)] = entity
	}
	entries := []ReleaseFilteredEntity{}
	for kind, ids := range hidden {
		if filter.Kind != "" && kind != filter.Kind {
			continue
		}
		for _, id := range ids {
			if entity, ok := byID[id]; ok {
				entries = append(entries, ReleaseFilteredEntity{ContentKind: kind, Entity: entity})
			}
		}
	}
	slices.SortFunc(entries, func(a, b ReleaseFilteredEntity) int {
		if n := strings.Compare(a.ContentKind, b.ContentKind); n != 0 {
			return n
		}
		if n := strings.Compare(strings.ToLower(a.Entity.Name), strings.ToLower(b.Entity.Name)); n != 0 {
			return n
		}
		return strings.Compare(string(a.Entity.ID), string(b.Entity.ID))
	})
	indices := make(map[string]int, len(out.Days))
	for i := range out.Days {
		out.Days[i].FilteredEntities = slices.Clone(entries)
		indices[out.Days[i].Date] = i
	}
	q, err := s.compileWithViewer(ctx, visible.reader, filter, visible.viewer.WithoutContentFilters())
	if err != nil {
		return err
	}
	// A visible matching edition already represents this content, even when a
	// different edition would be hidden by one of the optional filters.
	q.and("NOT (f.content_id = ANY(@r_visible::text[]))", pgx.NamedArgs{"r_visible": visibleIDs})
	got, err := s.releasePicks(ctx, q, from, until, now)
	if err != nil {
		return err
	}
	const batchSize = 256
	for offset := 0; offset < len(got); offset += batchSize {
		batch := got[offset:min(offset+batchSize, len(got))]
		picks := make([]pick, len(batch))
		dates := make(map[string]string, len(batch))
		for i, r := range batch {
			picks[i], dates[r.content] = r.pick, r.at.In(visible.loc).Format(time.DateOnly)
		}
		rows, err := s.cardRows(ctx, q, picks)
		if err != nil {
			return err
		}
		for _, row := range rows {
			i, ok := indices[dates[row.ID]]
			if !ok {
				continue
			}
			day := &out.Days[i]
			day.FilteredCount++
			for j := range day.FilteredEntities {
				entry := &day.FilteredEntities[j]
				if entry.ContentKind == row.Kind && slices.Contains(row.groupIDs, string(entry.Entity.ID)) {
					entry.Count++
				}
			}
		}
	}
	return nil
}
