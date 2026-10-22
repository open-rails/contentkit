package catalog

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
)

// Totals count content items, never versions: once in the kind and once per
// language any eligible version serves. Public is what an anonymous
// visitor's listing shows and opens; a tier counts everything its holders
// open. Group-gated, pay-per-view,
// draft, scheduled and deleted items never count; the site's scope and the
// default content filters apply.
func TestTotalsIntegration(t *testing.T) {
	f := newFixture(t, nil)
	yaoi, gore, studio := f.group("tag", "yaoi"), f.group("tag", "gore"), f.group("channel", "studio")
	f.sell(studio)
	premium := &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}
	item := func(in ContentInput, publish bool) string {
		id := f.create("staff", in)
		if publish {
			f.publish("staff", id, nil)
		}
		return id
	}
	with := func(in ContentInput, edit func(*ContentInput)) ContentInput {
		edit(&in)
		return in
	}
	enJa := item(gallery("Both", "en", "ja"), true)
	f.join(yaoi, enJa, "")
	twoEn := gallery("Two English", "en")
	twoEn.Versions = append(twoEn.Versions, VersionInput{Languages: []string{"en"}, Traits: &[]string{"colored"}})
	item(twoEn, true)
	item(gallery("Restricted", "en"), false)
	restricted := item(with(gallery("Restricted", "en"), func(c *ContentInput) { c.Restrictions = []string{"restricted"} }), true)
	_ = restricted
	item(with(gallery("Premium", "en"), func(c *ContentInput) { c.Access = premium }), true)
	item(with(gallery("Members", "ja"), func(c *ContentInput) { c.Access = premium }), true)
	item(gallery("Additional", "en"), true)
	item(with(gallery("Channel", "en"), func(c *ContentInput) { c.Access = &access.Rule{Level: access.GroupLevel, Group: studio} }), true)
	item(with(gallery("Pay per view", "en"), func(c *ContentInput) { c.Access = &access.Rule{Level: access.PPV} }), true)
	gory := item(gallery("Gory", "en"), true)
	f.join(gore, gory, "")
	deleted := item(gallery("Deleted", "en"), true)
	if err := f.s.DeleteForModeration(f.ctx, f.as("staff"), []string{deleted}); err != nil {
		t.Fatal(err)
	}
	scheduled := f.create("staff", gallery("Scheduled", "en"))
	f.publish("staff", scheduled, ptr(f.now().Add(time.Hour)))
	premiumJa := item(with(gallery("Premium version", "ja"), func(c *ContentInput) { c.Versions[0].Access = premium }), true)
	_ = premiumJa
	item(ContentInput{Kind: "blog", Text: map[string]Text{"en": {Title: "Post"}}}, true)

	total := func(public, prem int64, langs map[string][2]int64) Total {
		t := Total{Public: public, Tiers: map[string]int64{"premium": prem}, Languages: map[string]LanguageTotal{}}
		for l, n := range langs {
			t.Languages[l] = LanguageTotal{Public: n[0], Tiers: map[string]int64{"premium": n[1]}}
		}
		return t
	}
	for _, c := range []struct {
		name string
		q    TotalsQuery
		want map[string]Total
	}{
		{"main, every kind", TotalsQuery{}, map[string]Total{
			// public: both, two-english, restricted, additional; premium adds premium, members and the premium version.
			"gallery": total(4, 7, map[string][2]int64{"en": {4, 5}, "ja": {1, 3}}),
			"blog":    total(1, 1, map[string][2]int64{"en": {1, 1}}),
		}},
		{"the restricted site", TotalsQuery{Kinds: []string{"gallery"}, Domain: "moe"}, map[string]Total{
			"gallery": total(3, 6, map[string][2]int64{"en": {3, 4}, "ja": {1, 3}})}},
		{"the gay site", TotalsQuery{Kinds: []string{"gallery"}, Domain: "gay"}, map[string]Total{
			"gallery": total(1, 1, map[string][2]int64{"en": {1, 1}, "ja": {1, 1}})}},
		{"languages asked for, uncounted ones zero", TotalsQuery{Kinds: []string{"blog"}, Languages: []string{"ja"}}, map[string]Total{
			"blog": total(1, 1, map[string][2]int64{"ja": {0, 0}})}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := f.s.Totals(f.ctx, c.q)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Kinds, c.want) {
				t.Fatalf("totals %+v\n want %+v", got.Kinds, c.want)
			}
		})
	}
	beforeReorder, err := f.s.Totals(f.ctx, TotalsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(f.s.cfg.ContentFilters.Domains)
	afterReorder, err := f.s.Totals(f.ctx, TotalsQuery{})
	if err != nil || !reflect.DeepEqual(beforeReorder, afterReorder) {
		t.Fatalf("reordering domains changed default totals: before %+v, after %+v, error %v", beforeReorder, afterReorder, err)
	}
	for _, q := range []TotalsQuery{{Kinds: []string{"video"}}, {Languages: []string{"de"}}, {Domain: "nowhere"}} {
		if _, err := f.s.Totals(f.ctx, q); err == nil {
			t.Errorf("%+v accepted", q)
		}
	}

	// The open route takes the site from the host and lets any cache keep
	// the answer for ten minutes; the totals client reads it.
	req := httptest.NewRequest("GET", "/v1/catalog/totals?kinds=gallery&languages=en", nil)
	req.Host = "moe.test:8080"
	rec := httptest.NewRecorder()
	f.handler().ServeHTTP(rec, req)
	var body Totals
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "public, max-age=600" || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("route: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if want := total(3, 6, map[string][2]int64{"en": {3, 4}}); !reflect.DeepEqual(body.Kinds["gallery"], want) {
		t.Fatalf("route totals %+v, want %+v", body.Kinds["gallery"], want)
	}
	f.want(http.StatusBadRequest, anon, "GET", "/v1/catalog/totals?kinds=video", nil, nil)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	client, err := NewTotalsClient(srv.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	remote, err := client.Totals(f.ctx, TotalsQuery{Kinds: []string{"gallery", "blog"}, Languages: []string{"ja"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := total(4, 7, map[string][2]int64{"ja": {1, 3}}); !reflect.DeepEqual(remote.Kinds["gallery"], want) || remote.Kinds["blog"].Public != 1 {
		t.Fatalf("remote totals %+v", remote.Kinds)
	}
	if _, err := client.Totals(f.ctx, TotalsQuery{Kinds: []string{"gallery"}, Domain: "moe"}); err == nil {
		t.Fatal("the client took a site")
	}
}
