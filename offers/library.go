package offers

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
)

// LibraryQuery pages a viewer's library.
type LibraryQuery struct {
	// Language picks the titles; the configured languages follow.
	Language string
	Cursor   string // the previous page's next_cursor
	Limit    int    // 20 by default, at most 100
}

// LibraryItem is one content item the viewer bought.
type LibraryItem struct {
	ContentID string `json:"content_id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Language  string `json:"language,omitempty"`
	// Deleted content stays readable to its buyers.
	Deleted bool `json:"deleted"`
	// Whole: the content key, every version. Otherwise VersionIDs are the
	// versions bought.
	Whole      bool     `json:"whole"`
	VersionIDs []string `json:"version_ids"`
}

// LibraryPage is one page of a library, newest content first.
type LibraryPage struct {
	Items      []LibraryItem `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
	// Complete is false when the viewer holds more keys than one billing read
	// returns (access.HeldLimit); the library lists those it read.
	Complete bool `json:"complete"`
}

// Library lists the content actor holds a content or version key of: the
// keys under the tenant's content prefix, from the request's one billing
// read. Deleted content is listed too.
func (s *Service) Library(ctx context.Context, actor access.Actor, q LibraryQuery) (LibraryPage, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	q.Limit = min(q.Limit, 100)
	f, err := s.gate.Filter(ctx, actor)
	if err != nil {
		return LibraryPage{}, err
	}
	whole := map[string]bool{}
	versions := map[string][]string{}
	for _, c := range f.Contents() {
		whole[c] = true
	}
	for v, c := range f.Versions() {
		versions[c] = append(versions[c], v)
	}
	ids := make([]string, 0, len(whole)+len(versions))
	for c := range whole {
		ids = append(ids, c)
	}
	for c := range versions {
		if !whole[c] {
			ids = append(ids, c)
		}
	}
	page := LibraryPage{Items: []LibraryItem{}, Complete: f.Complete()}
	if len(ids) == 0 {
		return page, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id, c.kind, c.state = 'deleted' OR c.deleted_at IS NOT NULL, coalesce(t.title, ''), coalesce(t.language, '')
		FROM `+s.t.items+` c LEFT JOIN LATERAL (SELECT title, language FROM `+s.t.text+` x WHERE x.tenant_id = c.tenant_id AND x.content_id = c.id
			ORDER BY x.language = $3 DESC, array_position($4::text[], x.language), x.language LIMIT 1) t ON true
		WHERE c.tenant_id = $1 AND c.id = ANY($2) AND ($5 = '' OR c.id < $5)
		ORDER BY c.id DESC LIMIT $6`, s.tenant, ids, q.Language, s.languages(), q.Cursor, q.Limit+1)
	if err != nil {
		return LibraryPage{}, err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LibraryItem, error) {
		var it LibraryItem
		err := r.Scan(&it.ContentID, &it.Kind, &it.Deleted, &it.Title, &it.Language)
		it.Whole = whole[it.ContentID]
		it.VersionIDs = versions[it.ContentID]
		if it.Whole || it.VersionIDs == nil {
			it.VersionIDs = []string{}
		}
		slices.Sort(it.VersionIDs)
		return it, err
	})
	if err != nil {
		return LibraryPage{}, err
	}
	if len(items) > q.Limit {
		items = items[:q.Limit]
		page.NextCursor = items[q.Limit-1].ContentID
	}
	page.Items = items
	return page, nil
}
