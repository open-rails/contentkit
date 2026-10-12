package main

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	kindcfg "github.com/open-rails/contentkit/kinds"
)

// contentModel is the site's kinds config: content kinds joining every group
// kind, which between them declare roles, attributes, order, parents, scopes,
// windows and version-level memberships.
func contentModel() kindcfg.Config {
	grouped := kindcfg.Kind{Presentation: "reader", Versions: kindcfg.VersionRules{Languages: "one"},
		Groups: []string{"tag", "artist", "series", "character", "voice_actor", "rail"}}
	gallery, album := grouped, grouped
	gallery.Route, album.Route = "g", "album"
	return kindcfg.Config{Languages: []string{"en", "ja"},
		Kinds: map[string]kindcfg.Kind{"gallery": gallery, "album": album},
		GroupKinds: map[string]kindcfg.GroupKind{
			"tag": {Route: "tag", VersionLevel: true},
			"artist": {Route: "artists", Roles: []string{"publisher"}, Attributes: map[string]kindcfg.Attribute{
				"artist_type": {Type: "enum", Values: []string{"artist", "circle", "company"}},
				"active":      {Type: "bool"}, "works": {Type: "int"}, "debut": {Type: "time"}, "link": {Type: "string"}}},
			"series":      {Route: "series", Ordered: true, Parent: "series"},
			"character":   {Route: "characters", ScopedBy: "series"},
			"voice_actor": {Route: "voice-actors", VersionLevel: true},
			"rail":        {Ordered: true, Windows: true},
		}}
}

// kinds serves the content model as GET /v1/kinds will, for the UI's
// fallback until that route exists.
func (h *harness) kinds(w http.ResponseWriter, _ *http.Request) {
	c := contentModel()
	reply(w, http.StatusOK, map[string]any{"languages": c.Languages, "tiers": c.Tiers, "kinds": c.Kinds, "group_kinds": c.GroupKinds, "content_filters": c.ContentFilters})
}

// createContent writes published content with live, listed versions and
// their facets, as the catalog lane's routes will: {kind, versions:
// [{languages, favorites?}]} → {id, versions}.
func (h *harness) createContent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind     string `json:"kind"`
		Versions []struct {
			Languages []string `json:"languages"`
			Favorites int      `json:"favorites"`
		} `json:"versions"`
	}
	if err := decode(r, &in); err != nil || in.Kind == "" || len(in.Versions) == 0 {
		fail(w, http.StatusBadRequest, err)
		return
	}
	out := struct {
		ID       string   `json:"id"`
		Versions []string `json:"versions"`
	}{ID: contentref.NewID()}
	q := pgx.Identifier{schema}.Sanitize()
	live := time.Now().Add(-time.Hour)
	batch := &pgx.Batch{}
	batch.Queue(`INSERT INTO `+q+`.content_items (tenant_id, id, kind, state, published_at) VALUES ($1, $2, $3, 'published', $4)`, tenant, out.ID, in.Kind, live)
	for i, v := range in.Versions {
		vid := contentref.NewID()
		out.Versions = append(out.Versions, vid)
		batch.Queue(`INSERT INTO `+q+`.content_versions (tenant_id, id, content_id, position, languages, state, published_at) VALUES ($1, $2, $3, $4, $5, 'published', $6)`,
			tenant, vid, out.ID, i, v.Languages, live)
		batch.Queue(`INSERT INTO `+q+`.content_facets (tenant_id, content_id, version_id, kind, live_from, listing, languages, access_level, favorites, published_at)
 VALUES ($1, $2, $3, $4, $5, 'listed', $6, 'public', $7, $5)`, tenant, out.ID, vid, in.Kind, live, v.Languages, v.Favorites)
	}
	if err := h.pool.SendBatch(r.Context(), batch).Close(); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	reply(w, http.StatusCreated, out)
}

// recount runs the groups' count and automatic cover job now.
func (h *harness) recount(w http.ResponseWriter, r *http.Request) {
	if err := h.groups.Recount(r.Context()); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
