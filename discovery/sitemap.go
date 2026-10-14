package discovery

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/httpapi"
)

// SitemapPage is one page of a site's public pages of one kind, by id.
type SitemapPage struct {
	Entries []SitemapEntry `json:"entries"`
	// NextCursor pages on; empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// SitemapEntry is one public page: a content item or group.
type SitemapEntry struct {
	ID   string `json:"id"`
	Code string `json:"code"`
	// Path is the page in the first of the site's languages it serves;
	// Languages are its page in each, for hreflang alternates.
	Path      string         `json:"path"`
	LastMod   time.Time      `json:"lastmod"`
	Languages []SitemapAlias `json:"languages"`
}

// SitemapAlias is a page in one language.
type SitemapAlias struct {
	Language string `json:"language"`
	Path     string `json:"path"`
}

// SitemapOptions select one page of the feed.
type SitemapOptions struct {
	// Host picks the site scope.
	Host string
	Kind string
	// Cursor is the previous page's NextCursor.
	Cursor string
	// Limit defaults to 1000 and is capped at 10000.
	Limit int
}

var sitemapRoutes = []httpapi.Route[handler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/sitemap/contents", Resource: "discovery", Auth: httpapi.Open,
		Doc: "Pages through the site's public content pages of one kind, by id, for host sitemaps: live, listed and in the request's site scope, each with every language it serves.",
		Query: []httpapi.Param{httpapi.Text("kind", "the content kind; required"), httpapi.Text("cursor", "the previous page's next_cursor"),
			httpapi.Int("limit", "page size: 1000 by default, at most 10000")},
		Responses: []httpapi.Reply{httpapi.OK(SitemapPage{})}}, Serve: httpapi.H(handler.sitemapContents)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/sitemap/groups", Resource: "discovery", Auth: httpapi.Open,
		Doc: "Pages through the site's public group pages of one kind, by id, for host sitemaps: active groups holding content the site lists, each with the languages that content serves.",
		Query: []httpapi.Param{httpapi.Text("kind", "the group kind; required"), httpapi.Text("cursor", "the previous page's next_cursor"),
			httpapi.Int("limit", "page size: 1000 by default, at most 10000")},
		Responses: []httpapi.Reply{httpapi.OK(SitemapPage{})}}, Serve: httpapi.H(handler.sitemapGroups)},
}

func (h handler) sitemapOptions(r *http.Request) (SitemapOptions, error) {
	limit, err := intParam(r, "limit")
	q := r.URL.Query()
	return SitemapOptions{Host: r.Host, Kind: q.Get("kind"), Cursor: q.Get("cursor"), Limit: limit}, err
}

func (h handler) sitemapContents(w http.ResponseWriter, r *http.Request) {
	o, err := h.sitemapOptions(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	page, err := h.s.SitemapContents(r.Context(), o)
	h.reply(w, r, page, err)
}

func (h handler) sitemapGroups(w http.ResponseWriter, r *http.Request) {
	o, err := h.sitemapOptions(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	page, err := h.s.SitemapGroups(r.Context(), o)
	h.reply(w, r, page, err)
}

// sitemapRow is one page as its query reads it.
type sitemapRow struct {
	id, code, slug string
	slugs          map[string]string
	lastMod        time.Time
	languages      []string
}

// SitemapContents pages the site's public pages of a content kind.
func (s *Service) SitemapContents(ctx context.Context, o SitemapOptions) (SitemapPage, error) {
	k, ok := s.o.Kinds.Kinds[o.Kind]
	if !ok || k.Route == "" {
		return SitemapPage{Entries: []SitemapEntry{}}, fmt.Errorf("%w: kind %q is no content kind with pages", ErrInvalid, o.Kind)
	}
	return s.sitemap(ctx, o, k.Route, func(pred string) string {
		return fmt.Sprintf(`SELECT c.id, coalesce(cd.code, ''), coalesce(cd.slug, ''), coalesce(cd.slugs, '{}'), c.updated_at, l.languages
FROM %[1]s c
CROSS JOIN LATERAL (SELECT array_agg(DISTINCT x ORDER BY x) AS languages FROM %[2]s f CROSS JOIN LATERAL unnest(f.languages) x
 WHERE f.tenant_id = c.tenant_id AND f.content_id = c.id AND %[4]s) l
JOIN %[3]s cd ON cd.tenant_id = c.tenant_id AND cd.content_kind = c.kind AND cd.content_id = c.id
WHERE c.tenant_id = @d_tenant AND c.kind = @d_kind AND c.id > @d_after AND l.languages IS NOT NULL
ORDER BY c.id LIMIT @d_limit`, s.t.items, s.t.facets, s.t.codes, pred)
	})
}

// SitemapGroups pages the site's public pages of a group kind: active
// groups holding content the site lists.
func (s *Service) SitemapGroups(ctx context.Context, o SitemapOptions) (SitemapPage, error) {
	k, ok := s.o.Kinds.GroupKinds[o.Kind]
	if !ok || k.Route == "" {
		return SitemapPage{Entries: []SitemapEntry{}}, fmt.Errorf("%w: kind %q is no group kind with pages", ErrInvalid, o.Kind)
	}
	return s.sitemap(ctx, o, k.Route, func(pred string) string {
		return fmt.Sprintf(`SELECT g.id, coalesce(cd.code, ''), coalesce(cd.slug, g.slug), coalesce(cd.slugs, '{}'), g.updated_at, l.languages
FROM %[1]s g
CROSS JOIN LATERAL (SELECT ARRAY(SELECT x.l FROM unnest(@d_langs::text[]) AS x(l) WHERE EXISTS (SELECT 1 FROM %[2]s f
 WHERE f.tenant_id = g.tenant_id AND f.group_ids @> ARRAY[g.id] AND f.languages @> ARRAY[x.l] AND %[4]s)) AS languages) l
JOIN %[3]s cd ON cd.tenant_id = g.tenant_id AND cd.content_kind = g.kind AND cd.content_id = g.id
WHERE g.tenant_id = @d_tenant AND g.kind = @d_kind AND g.state = 'active' AND g.id > @d_after AND cardinality(l.languages) > 0
ORDER BY g.id LIMIT @d_limit`, s.t.groups, s.t.facets, s.t.codes, pred)
	})
}

// sitemap runs one page of a feed's query, which selects sitemapRows under
// the site's indexable predicate over f, and builds each page's paths.
func (s *Service) sitemap(ctx context.Context, o SitemapOptions, route string, query func(pred string) string) (SitemapPage, error) {
	page := SitemapPage{Entries: []SitemapEntry{}}
	limit := o.Limit
	switch {
	case limit == 0:
		limit = 1000
	case limit > 10000:
		limit = 10000
	}
	after, err := decodeSitemapCursor(o.Cursor)
	if err != nil {
		return page, err
	}
	v, err := s.o.Visibility.Viewer(ctx, catalog.ViewerOptions{Actor: access.Actor{Anonymous: true}, Host: o.Host})
	if err != nil {
		return page, err
	}
	pred, args := v.Indexable("f")
	args["d_tenant"], args["d_kind"], args["d_after"], args["d_limit"], args["d_langs"] = s.o.Tenant, o.Kind, after, limit+1, s.languages()
	rows, err := s.o.Pool.Query(ctx, query(pred), args)
	if err != nil {
		return page, err
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (sitemapRow, error) {
		var x sitemapRow
		var slugs []byte
		if err := r.Scan(&x.id, &x.code, &x.slug, &slugs, &x.lastMod, &x.languages); err != nil {
			return x, err
		}
		return x, json.Unmarshal(slugs, &x.slugs)
	})
	if err != nil {
		return page, err
	}
	langs := s.languages()
	for i, x := range got {
		if i == limit {
			page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(got[limit-1].id))
			break
		}
		link := contenturl.Link{Code: contenturl.Code(x.code), Slug: x.slug, Slugs: x.slugs}
		e := SitemapEntry{ID: x.id, Code: x.code, LastMod: x.lastMod.UTC(), Languages: []SitemapAlias{}}
		for _, l := range langs {
			if slices.Contains(x.languages, l) {
				e.Languages = append(e.Languages, SitemapAlias{Language: l, Path: contenturl.Path(langs, l, route, link.Code, link.SlugFor(l))})
			}
		}
		if len(e.Languages) == 0 {
			continue
		}
		e.Path = e.Languages[0].Path
		page.Entries = append(page.Entries, e)
	}
	return page, nil
}

func decodeSitemapCursor(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) == 0 {
		return "", fmt.Errorf("%w: cursor", ErrInvalid)
	}
	return string(b), nil
}
