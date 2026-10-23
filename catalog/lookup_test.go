package catalog

import (
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/open-rails/contentkit/access"
)

func TestVersionLookupPreservesSelectionAndAccess(t *testing.T) {
	f := newFixture(t, nil)
	in := gallery("One work", "en")
	text := in.Text["en"]
	text.Body = "The shared body"
	in.Text["en"] = text
	parent := f.create("staff", in)
	f.publish("staff", parent, nil)
	first := f.read(anon, "main.test", parent)[0]
	primary := first.Versions[0].ID
	ids, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{
		{ContentID: parent, VersionInput: VersionInput{Languages: []string{"ja"}, State: StatePublished, Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}}},
		{ContentID: parent, VersionInput: VersionInput{Languages: []string{"en"}, State: StateDraft}},
	})
	if err != nil {
		t.Fatal(err)
	}
	premium, draft := ids[0], ids[1]
	shared := f.entity("tag", "shared")
	edition := f.entity("tag", "edition")
	f.join(shared, parent, "")
	f.join(edition, parent, premium)
	f.claim("member", "premium")

	if got := f.read(anon, "main.test", parent)[0]; got.SelectedVersionID != primary || !got.Decision.Allowed || got.Body == "" {
		t.Fatalf("parent did not select its public default: %+v", got)
	}
	locked, err := f.s.LookupVersion(f.ctx, f.viewer(anon, "main.test", ViewerOptions{}), premium)
	if err != nil {
		t.Fatal(err)
	}
	if locked.ID != parent || locked.SelectedVersionID != premium || len(locked.Versions) != 1 || locked.Decision.Allowed || locked.Body != "" {
		t.Fatalf("the free sibling leaked access into the selected premium edition: %+v", locked)
	}
	opened, err := f.s.LookupVersion(f.ctx, f.viewer("member", "main.test", ViewerOptions{}), premium)
	if err != nil {
		t.Fatal(err)
	}
	if !opened.Decision.Allowed || opened.Body == "" || opened.Versions[0].ID != premium {
		t.Fatalf("the authorized edition was not selected: %+v", opened)
	}
	if !slices.ContainsFunc(opened.Entities, func(g EntitySummary) bool { return g.ID == shared }) ||
		!slices.ContainsFunc(opened.Versions[0].Entities, func(g EntitySummary) bool { return g.ID == edition }) {
		t.Fatalf("shared and edition entities were lost: %+v", opened)
	}
	for _, id := range []string{draft, parent, "not-an-id"} {
		if _, err := f.s.LookupVersion(f.ctx, f.viewer(anon, "main.test", ViewerOptions{}), id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("hidden/missing version %s fell back to a sibling: %v", id, err)
		}
	}
	if _, err := f.s.LookupVersion(f.ctx, f.viewer("member", "gay.test", ViewerOptions{}), premium); !errors.Is(err, ErrNotFound) {
		t.Fatalf("direct lookup bypassed the domain's required tag: %v", err)
	}
	foreign, err := New(f.ctx, Options{Pool: f.pool, Schema: f.schema, Tenant: "another-tenant", Kinds: config(), Auth: f.auth, Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(foreign.Close)
	foreignViewer, err := foreign.Viewer(access.WithMemo(f.ctx), ViewerOptions{Actor: f.as("member"), Host: "main.test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.LookupVersion(f.ctx, foreignViewer, premium); !errors.Is(err, ErrNotFound) {
		t.Fatalf("direct lookup crossed tenant ownership: %v", err)
	}
	var response Content
	f.want(http.StatusOK, anon, http.MethodGet, "/v1/versions/"+premium, nil, &response)
	if response.ID != parent || response.SelectedVersionID != premium || response.Decision.Allowed || response.Body != "" {
		t.Fatalf("HTTP lookup changed the selected permission: %+v", response)
	}
	f.want(http.StatusNotFound, anon, http.MethodGet, "/v1/versions/"+draft, nil, nil)
}
