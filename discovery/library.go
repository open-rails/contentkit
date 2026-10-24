package discovery

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/domainpolicy"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/kinds"
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
	// Whole reports a held content key. VersionIDs lists only the
	// currently eligible versions covered by the held entitlement.
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
// read. Deleted content is listed too, within the current domain's hard
// visibility boundary.
func (s *Service) Library(ctx context.Context, actor access.Actor, q LibraryQuery) (LibraryPage, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	q.Limit = min(q.Limit, 100)
	f, err := s.o.Gate.Filter(ctx, actor)
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
	policies, err := s.libraryDomain(ctx)
	if err != nil {
		return LibraryPage{}, err
	}
	hard := domainpolicy.Policies{}
	for kind, p := range policies {
		if p.MissingHard || p.MissingRequired || len(p.Required) > 0 || len(p.Blocked) > 0 {
			hard[kind] = p
		}
	}
	scope, args := hard.SQL("f", "library_domain")
	wholeIDs := make([]string, 0, len(whole))
	for id := range whole {
		wholeIDs = append(wholeIDs, id)
	}
	versionIDs := []string{}
	for _, ids := range versions {
		versionIDs = append(versionIDs, ids...)
	}
	editionTitles := []string{}
	for name, kind := range s.o.Kinds.Kinds {
		if kind.TitlePolicy == kinds.TitleOriginalAndVersion {
			editionTitles = append(editionTitles, name)
		}
	}
	args["tenant"], args["ids"], args["language"], args["languages"], args["cursor"], args["limit"] = s.o.Tenant, ids, q.Language, s.o.Kinds.Languages, q.Cursor, q.Limit+1
	args["whole"], args["versions"], args["edition_titles"] = wholeIDs, versionIDs, editionTitles
	args["now"] = s.o.Now()
	// Eligibility is evaluated for each granted version. An unowned sibling
	// may never qualify the parent and reveal a blocked purchased edition.
	// As in catalog direct reads, only a matured live_from permits disclosure;
	// a grant never publishes an unreleased edition. Deleted purchased editions
	// retain live_from and can still appear.
	rows, err := s.o.Pool.Query(ctx, `SELECT c.id,c.kind,c.state='deleted' OR c.deleted_at IS NOT NULL,
 CASE WHEN c.kind=ANY(@edition_titles::text[])
 THEN coalesce(nullif(ed.title,''),nullif(c.original_title,''),t.title,'') ELSE coalesce(t.title,'') END,
 CASE WHEN c.kind=ANY(@edition_titles::text[]) AND coalesce(ed.title,'')<>'' THEN ed.language ELSE coalesce(t.language,'') END,
 ed.ids
 FROM `+s.t.items+` c
 JOIN LATERAL (
  SELECT array_agg(v.id ORDER BY v.position,v.id) AS ids,
   (array_agg(vt.title ORDER BY coalesce(@language=ANY(v.languages),false) DESC,v.position,v.id) FILTER(WHERE coalesce(vt.title,'')<>''))[1] AS title,
   (array_agg(vt.language ORDER BY coalesce(@language=ANY(v.languages),false) DESC,v.position,v.id) FILTER(WHERE coalesce(vt.title,'')<>''))[1] AS language
  FROM `+s.t.versions+` v
  LEFT JOIN `+s.t.facets+` f ON f.tenant_id=v.tenant_id AND f.version_id=v.id
  LEFT JOIN LATERAL (SELECT title,language FROM `+s.t.versionText+` x WHERE x.tenant_id=v.tenant_id AND x.version_id=v.id
   ORDER BY x.language=@language DESC,array_position(v.languages,x.language),array_position(@languages::text[],x.language),x.language LIMIT 1) vt ON true
  WHERE v.tenant_id=c.tenant_id AND v.content_id=c.id
   AND (c.id=ANY(@whole::text[]) OR v.id=ANY(@versions::text[]))
   AND f.live_from <= @now::timestamptz
   AND `+scope+`
  HAVING count(*)>0
 ) ed ON true
 LEFT JOIN LATERAL(SELECT title,language FROM `+s.t.text+` x WHERE x.tenant_id=c.tenant_id AND x.content_id=c.id
  ORDER BY x.language=@language DESC,array_position(@languages::text[],x.language),x.language LIMIT 1)t ON true
 WHERE c.tenant_id=@tenant AND c.id=ANY(@ids::text[]) AND (@cursor='' OR c.id<@cursor)
 ORDER BY c.id DESC LIMIT @limit`, args)
	if err != nil {
		return LibraryPage{}, err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (LibraryItem, error) {
		var it LibraryItem
		err := r.Scan(&it.ContentID, &it.Kind, &it.Deleted, &it.Title, &it.Language, &it.VersionIDs)
		it.Whole = whole[it.ContentID]
		if it.VersionIDs == nil {
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

// libraryDomain resolves hard content boundaries for the current request.
func (s *Service) libraryDomain(ctx context.Context) (domainpolicy.Policies, error) {
	d, _ := s.o.Kinds.DomainFor(gate.Domain(ctx))
	return domainpolicy.Resolve(ctx, s.o.Pool, s.t.entities, s.o.Tenant, d, s.o.Kinds.ContentFilters.AppliesTo)
}
