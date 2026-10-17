package catalog

import (
	"strings"
	"testing"

	"github.com/open-rails/contentkit/groups"
)

func TestDecorativePathsFollowSeriesAndTitle(t *testing.T) {
	f := newFixture(t, nil)
	path := func(id string) string {
		t.Helper()
		got := f.read("staff", "main.test", id)
		if len(got) != 1 || got[0].Code == "" {
			t.Fatalf("read %s: %+v", id, got)
		}
		return strings.TrimPrefix(got[0].Path, "/en/g/"+got[0].Code)
	}
	id := f.create("staff", gallery("Night Before", "en"))
	f.publish("staff", id, nil)
	if got := path(id); got != "/night-before" {
		t.Fatalf("without a series: %q", got)
	}

	saga, alpha := f.group("series", "saga"), f.group("series", "alpha")
	one, two := 1, 2
	if err := f.groups.AddMemberships(f.ctx, []groups.Membership{{GroupID: groups.GroupID(saga), ContentID: id, Position: &two}}); err != nil {
		t.Fatal(err)
	}
	if got := path(id); got != "/saga/night-before" {
		t.Fatalf("joined saga: %q", got)
	}
	// The first membership by position wins; without positions, the slug order would pick alpha.
	if err := f.groups.AddMemberships(f.ctx, []groups.Membership{{GroupID: groups.GroupID(alpha), ContentID: id, Position: &two},
		{GroupID: groups.GroupID(saga), ContentID: id, Position: &one}}); err != nil {
		t.Fatal(err)
	}
	if got := path(id); got != "/saga/night-before" {
		t.Fatalf("saga first by position: %q", got)
	}

	// Renaming the series re-slugs every member's path.
	if err := f.groups.SetNames(f.ctx, []groups.GroupNames{{ID: groups.GroupID(saga), Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Grand Saga"}}}}); err != nil {
		t.Fatal(err)
	}
	if got := path(id); got != "/grand-saga/night-before" {
		t.Fatalf("renamed series: %q", got)
	}
	if _, err := f.groups.DeleteGroups(f.ctx, []groups.GroupID{groups.GroupID(saga)}); err != nil {
		t.Fatal(err)
	}
	if got := path(id); got != "/alpha/night-before" {
		t.Fatalf("a deleted series leaves the path: %q", got)
	}
	if err := f.groups.RemoveMemberships(f.ctx, []groups.Membership{{GroupID: groups.GroupID(alpha), ContentID: id}}); err != nil {
		t.Fatal(err)
	}
	if got := path(id); got != "/night-before" {
		t.Fatalf("left every series: %q", got)
	}

	// A title without Latin letters or digits leaves the series alone.
	ja := f.create("staff", ContentInput{Kind: "gallery", Text: map[string]Text{"ja": {Title: "こんにちは"}}, Versions: []VersionInput{{Languages: []string{"ja"}}}})
	f.publish("staff", ja, nil)
	f.join(alpha, ja, "")
	if got := path(ja); got != "/alpha" {
		t.Fatalf("CJK title in a series: %q", got)
	}
}
