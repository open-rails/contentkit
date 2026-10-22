package discovery

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/kinds"
)

func TestRegistrationPreservesGuestChoicesAcrossDomains(t *testing.T) {
	cfg := siteConfig()
	locked := false
	cfg.ContentFilters.Domains, cfg.ContentFilters.AppliesTo = []kinds.Domain{{Name: "main", Hosts: []string{"main.test"}, Filters: []kinds.ContentFilter{
		{Group: kinds.GroupRef{Kind: "tag", Slug: "big-breasts"}},
		{Group: kinds.GroupRef{Kind: "tag", Slug: "gore"}, Visible: false, UserToggle: &locked},
		{Group: kinds.GroupRef{Kind: "tag", Slug: "private"}, Visible: false},
	},
	}, {Name: "other", Hosts: []string{"other.test"}, Filters: []kinds.ContentFilter{
		{Group: kinds.GroupRef{Kind: "tag", Slug: "yaoi"}, Visible: true},
		{Group: kinds.GroupRef{Kind: "tag", Slug: "gore"}, Visible: true},
		{Group: kinds.GroupRef{Kind: "tag", Slug: "private"}, Visible: true},
	},
	},
	}, []string{"gallery", "video"}
	f := newFixtureConfig(t, nil, "", cfg)
	f.group("tag", "big-breasts", "yaoi", "gore", "private")
	internal := true
	if _, err := f.groups.UpdateGroups(f.ctx, []groups.GroupUpdate{{ID: groups.GroupID(f.ids["private"]), Internal: &internal}}); err != nil {
		t.Fatal(err)
	}
	for i, slug := range []string{"big-breasts", "yaoi", "gore", "private"} {
		f.seed(item{n: i + 1, titles: map[string]string{"en": "Story"}, age: time.Hour, groups: []string{slug}, versions: []ver{{langs: []string{"en"}}}})
	}
	input := ContentFiltersInitialization{AccountID: "registered", Filters: filterChoices(FilterSetting{EntityID: f.ids["big-breasts"], Visible: true}, FilterSetting{EntityID: f.ids["yaoi"], Visible: false}, FilterSetting{EntityID: f.ids["gore"], Visible: true}, FilterSetting{EntityID: f.ids["private"], Visible: true}, FilterSetting{EntityID: "obsolete", Visible: true})}
	var settings ContentFilters
	if status, _ := f.call(http.MethodPost, "main.test", "registered", "/me/content-filters/initialize", input, &settings); status != http.StatusOK {
		t.Fatalf("initialize: %d", status)
	}
	if len(settings.Filters) != 1 || !settings.Filters[0].Visible {
		t.Fatalf("current controls: %+v", settings)
	}
	want := map[string]string{"entity:" + f.ids["big-breasts"]: show, "entity:" + f.ids["yaoi"]: hide, "entity:" + f.ids["gore"]: show}
	got, _, err := f.svc.prefs(f.ctx, "registered")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("saved union: %v %v", got, err)
	}
	main, _ := f.list("main.test", "registered", "/contents?kind=gallery")
	if len(main) != 2 {
		t.Fatalf("hard/internal/draft rules weakened: %v", main)
	}
	other, _ := f.list("other.test", "registered", "/contents?kind=gallery")
	if len(other) != 3 {
		t.Fatalf("other-domain choices lost: %v", other)
	}
	// A delayed registration callback never overwrites preferences already saved.
	input.Filters = nil
	if status, _ := f.call(http.MethodPost, "main.test", "registered", "/me/content-filters/initialize", input, nil); status != http.StatusOK {
		t.Fatal(status)
	}
	got, _, err = f.svc.prefs(f.ctx, "registered")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("retry replaced union: %v %v", got, err)
	}
}
