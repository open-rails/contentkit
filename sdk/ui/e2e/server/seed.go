package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/search"
)

// seedVersion is one version as the catalog writes it.
type seedVersion struct {
	Languages []string        `json:"languages"`
	Traits    []string        `json:"traits"`
	Access    json.RawMessage `json:"access"` // an override rule
	Pages     int             `json:"pages"`
	Future    bool            `json:"future"` // live in an hour
}

// seedContent is one content item as the catalog writes it, with its
// facets, memberships and keyword documents.
type seedContent struct {
	Kind         string            `json:"kind"`
	Titles       map[string]string `json:"titles"`
	Access       json.RawMessage   `json:"access"` // public by default
	Listing      string            `json:"listing"`
	Restrictions []string          `json:"restrictions"`
	Attributes   map[string]any    `json:"attributes"`
	AgeHours     float64           `json:"age_hours"` // published this long ago; default 1
	Favorites    int               `json:"favorites"`
	Versions     []seedVersion     `json:"versions"`
	Groups       []string          `json:"groups"` // group ids, content-level
}

// createContent writes contents through the contract tables, as the
// catalog's routes will: [seedContent] → [{id, code, versions}].
func (h *harness) createContent(w http.ResponseWriter, r *http.Request) {
	var in []seedContent
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	type made struct {
		ID       string   `json:"id"`
		Code     string   `json:"code"`
		Versions []string `json:"versions"`
	}
	out := make([]made, 0, len(in))
	q := pgx.Identifier{schema}.Sanitize()
	batch := &pgx.Batch{}
	var docs []search.KeywordDocument
	var ms []groups.Membership
	for _, c := range in {
		if c.Kind == "" || len(c.Versions) == 0 {
			fail(w, http.StatusBadRequest, fmt.Errorf("a content needs a kind and versions"))
			return
		}
		rule := c.Access
		if len(rule) == 0 {
			rule = json.RawMessage(`{"level":"public"}`)
		}
		if c.Listing == "" {
			c.Listing = "listed"
		}
		if c.AgeHours == 0 {
			c.AgeHours = 1
		}
		attrs, _ := json.Marshal(c.Attributes)
		if c.Attributes == nil {
			attrs = []byte("{}")
		}
		published := time.Now().UTC().Add(-time.Duration(c.AgeHours * float64(time.Hour)))
		m := made{ID: contentref.NewID(), Code: code()}
		batch.Queue(`INSERT INTO `+q+`.content_items (tenant_id, id, kind, state, published_at, listing, access, restrictions, attributes)
 VALUES ($1, $2, $3, 'published', $4, $5, $6, $7, $8)`, tenant, m.ID, c.Kind, published, c.Listing, string(rule), nonNil(c.Restrictions), string(attrs))
		main := ""
		for lang, title := range c.Titles {
			s := slugOf(title)
			if lang == "en" || main == "" {
				main = s
			}
			batch.Queue(`INSERT INTO `+q+`.content_text (tenant_id, content_id, language, title, slug) VALUES ($1, $2, $3, $4, $5)`, tenant, m.ID, lang, title, s)
			docs = append(docs, search.KeywordDocument{DocumentKey: search.DocumentKey{ContentRef: contentref.New(tenant, c.Kind, m.ID), Language: lang}, Title: title})
		}
		batch.Queue(`INSERT INTO `+q+`.content_codes (tenant_id, code, content_kind, content_id, slug) VALUES ($1, $2, $3, $4, $5)`, tenant, m.Code, c.Kind, m.ID, main)
		for i, v := range c.Versions {
			vr := rule
			if len(v.Access) > 0 {
				vr = v.Access
			}
			var parsed access.Rule
			if err := json.Unmarshal(vr, &parsed); err != nil {
				fail(w, http.StatusBadRequest, err)
				return
			}
			fc := parsed.Facet()
			live := published
			if v.Future {
				live = time.Now().UTC().Add(time.Hour)
			}
			var pages *int
			if v.Pages > 0 {
				pages = &v.Pages
			}
			var override *string
			if len(v.Access) > 0 {
				s := string(v.Access)
				override = &s
			}
			id := contentref.NewID()
			m.Versions = append(m.Versions, id)
			batch.Queue(`INSERT INTO `+q+`.content_versions (tenant_id, id, content_id, position, languages, traits, state, published_at, access, pages)
 VALUES ($1, $2, $3, $4, $5, $6, 'published', $7, $8, $9)`, tenant, id, m.ID, i, v.Languages, nonNil(v.Traits), published, override, pages)
			batch.Queue(`INSERT INTO `+q+`.content_facets (tenant_id, content_id, version_id, kind, live_from, listing, languages, access_level,
 access_tiers, access_group_id, access_free_after, restrictions, traits, position, published_at, attributes, favorites)
 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
				tenant, m.ID, id, c.Kind, live, c.Listing, v.Languages, string(fc.Level), fc.Tiers, fc.GroupID, fc.FreeAfter,
				nonNil(c.Restrictions), nonNil(v.Traits), i, published, string(attrs), c.Favorites)
		}
		for _, g := range c.Groups {
			ms = append(ms, groups.Membership{GroupID: groups.GroupID(g), ContentID: m.ID})
		}
		out = append(out, m)
	}
	if err := h.pool.SendBatch(r.Context(), batch).Close(); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if len(ms) > 0 {
		if err := h.groups.AddMemberships(r.Context(), ms); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
	}
	if err := search.UpsertKeywordDocuments(r.Context(), h.pool, schema, docs); err != nil {
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

// code is a fresh content code: nine of Crockford's alphabet, led by a letter.
func code() string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	b[0] = alphabet[10+int(b[0])%22]
	return string(b)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugOf is a title's ASCII slug, as codes store it.
func slugOf(title string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > 80 {
		s = strings.Trim(s[:80], "-")
	}
	return s
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
