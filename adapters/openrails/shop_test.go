package openrails_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/open-rails/openrails"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/contract"
	"github.com/open-rails/contentkit/internal/hub"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/offers"
)

// shop is a site over the world: entities, the catalog, discovery and offers
// wired as the engine wires them, each module's routes served under /v1 and
// held to the route catalog.
type shop struct {
	*world
	entities                 *entities.Store
	cat                      *catalog.Store
	staff                    access.Actor
	catalogAPI, discoveryAPI *httptest.Server
}

func newShop(t *testing.T, edits ...func(*openrails.Config)) *shop {
	t.Helper()
	ctx := t.Context()
	s := &shop{world: newWorld(t, edits...), staff: testauth.Actor(access.Actor{ID: "staff"}, catalogStaff{})}
	var err error
	if s.entities, err = entities.New(entities.Options{Pool: s.pool, Schema: s.schema, Tenant: tenant, Kinds: s.kinds,
		ContentChanged: func(ctx context.Context, q entities.Querier, ids []string) error {
			return s.cat.ContentChanged(ctx, q, ids)
		}}); err != nil {
		t.Fatal(err)
	}
	authn := testauth.Auth{Authz: catalogStaff{}}
	if s.cat, err = catalog.New(ctx, catalog.Options{Pool: s.pool, Schema: s.schema, Tenant: tenant, Kinds: s.kinds, Auth: authn, Gate: s.gate,
		Entities: s.entities, Offers: s.offers}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.cat.Close)
	h, err := hub.New(hub.Config{PG: s.pool, PGSchema: s.schema, Tenant: tenant, DefaultLanguage: "en"})
	if err != nil {
		t.Fatal(err)
	}
	disc, err := discovery.New(discovery.Options{Pool: s.pool, Schema: s.schema, Tenant: tenant, Kinds: s.kinds, Visibility: s.cat,
		Resolver: s.cat.Resolver(), Gate: s.gate, Entities: s.entities, Hub: h, Sales: s.offers})
	if err != nil {
		t.Fatal(err)
	}
	dh, err := disc.Handler(authn)
	if err != nil {
		t.Fatal(err)
	}
	s.catalogAPI, s.discoveryAPI = s.serve(s.cat.Handler()), s.serve(dh)
	return s
}

func (s *shop) serve(h http.Handler) *httptest.Server {
	checker, err := contract.NewChecker(os.DirFS("../.."))
	if err != nil {
		s.t.Fatal(err)
	}
	srv := httptest.NewServer(checker.Wrap("", http.StripPrefix("/v1", h), func(err error) { s.t.Error(err) }))
	s.t.Cleanup(srv.Close)
	return srv
}

// get reads path from srv as viewer, requires 200 and decodes into out; it
// returns the raw body.
func (s *shop) get(srv *httptest.Server, as access.Actor, path string, out any) string {
	s.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1"+path, nil)
	testauth.As(req, as)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("GET %s: %d %s", path, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		s.t.Fatal(err)
	}
	return string(raw)
}

// channel creates a channel owned by owner.
func (s *shop) channel(slug, name, owner string) string {
	s.t.Helper()
	made, err := s.entities.CreateEntities(s.t.Context(), []entities.EntityInput{{Kind: "channel", Slug: slug, OwnerUserID: owner,
		Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: name}}}})
	if err != nil {
		s.t.Fatal(err)
	}
	return string(made[0].ID)
}

// post publishes a post as staff and returns its id.
func (s *shop) post(in catalog.ContentInput) string {
	s.t.Helper()
	ctx := s.t.Context()
	in.Kind = "post"
	if in.Versions == nil {
		in.Versions = []catalog.VersionInput{{Languages: []string{"en"}}}
	}
	ids, err := s.cat.Create(ctx, s.staff, []catalog.ContentInput{in})
	if err != nil {
		s.t.Fatal(err)
	}
	if err := s.cat.Publish(ctx, s.staff, []catalog.Publish{{ContentID: ids[0]}}); err != nil {
		s.t.Fatal(err)
	}
	return ids[0]
}

// sell puts terms on target and syncs them to OpenRails.
func (s *shop) sell(target offers.Target, terms offers.Terms) {
	s.t.Helper()
	ctx := s.t.Context()
	if _, err := s.offers.Put(ctx, []offers.Put{{Target: target, Terms: terms}}); err != nil {
		s.t.Fatal(err)
	}
	if err := s.offers.Sync(ctx, []offers.Target{target}); err != nil {
		s.t.Fatal(err)
	}
}
