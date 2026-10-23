package catalog

import (
	"strings"
	"testing"

	"github.com/open-rails/contentkit/entities"
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

	saga := f.entity("series", "saga")
	f.join(saga, id, "")
	if got := path(id); got != "/saga/night-before" {
		t.Fatalf("joined saga: %q", got)
	}
	// Renaming the series re-slugs every member's path.
	if err := f.entities.SetNames(f.ctx, []entities.EntityNames{{ID: entities.EntityID(saga), Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Grand Saga"}}}}); err != nil {
		t.Fatal(err)
	}
	if got := path(id); got != "/grand-saga/night-before" {
		t.Fatalf("renamed series: %q", got)
	}
	// Of several unordered series, the first slug names the path.
	alpha := f.entity("series", "alpha")
	f.join(alpha, id, "")
	if got := path(id); got != "/alpha/night-before" {
		t.Fatalf("two series: %q", got)
	}
	if _, err := f.entities.DeleteEntities(f.ctx, []entities.EntityID{entities.EntityID(alpha)}); err != nil {
		t.Fatal(err)
	}
	if got := path(id); got != "/grand-saga/night-before" {
		t.Fatalf("a deleted series leaves the path: %q", got)
	}
	if err := f.entities.RemoveMemberships(f.ctx, []entities.Membership{{EntityID: entities.EntityID(saga), ContentID: id}}); err != nil {
		t.Fatal(err)
	}
	if got := path(id); got != "/night-before" {
		t.Fatalf("left every series: %q", got)
	}

	// A title without Latin letters or digits leaves the series alone.
	ja := f.create("staff", ContentInput{Kind: "gallery", Text: map[string]Text{"ja": {Title: "こんにちは"}}, Versions: []VersionInput{{Languages: []string{"ja"}}}})
	f.publish("staff", ja, nil)
	f.join(f.entity("series", "beta"), ja, "")
	if got := path(ja); got != "/beta" {
		t.Fatalf("CJK title in a series: %q", got)
	}
}

func TestRefreshPathsBackfillsEntityPaths(t *testing.T) {
	f := newFixture(t, nil)
	id := f.create("staff", gallery("Night Before", "en"))
	f.publish("staff", id, nil)
	f.join(f.entity("series", "saga"), id, "")
	// A path stored before its kind named a path entity.
	if _, err := f.pool.Exec(f.ctx, `UPDATE `+f.table("content_codes")+` SET slug = 'night-before', slugs = '{"en":"night-before"}' WHERE tenant_id = $1 AND content_id = $2`, tenant, id); err != nil {
		t.Fatal(err)
	}
	if got := f.read("staff", "main.test", id)[0].Path; !strings.HasSuffix(got, "/night-before") || strings.Contains(got, "saga") {
		t.Fatalf("stale path %q", got)
	}
	if n, err := RefreshPaths(f.ctx, f.pool, f.schema, "", config()); err != nil || n != 1 {
		t.Fatalf("RefreshPaths = %d, %v", n, err)
	}
	if got := f.read("staff", "main.test", id)[0].Path; !strings.HasSuffix(got, "/saga/night-before") {
		t.Fatalf("refreshed path %q", got)
	}
	if n, err := f.s.RefreshPaths(f.ctx); err != nil || n != 1 {
		t.Fatalf("idempotent RefreshPaths = %d, %v", n, err)
	}
}
