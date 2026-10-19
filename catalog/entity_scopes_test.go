package catalog

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/open-rails/contentkit/groups"
)

func TestContentEntityScopes(t *testing.T) {
	f := newFixture(t, nil)
	id := f.create("staff", gallery("Independent edition credits", "en", "ja"))
	initial := f.read("staff", "main.test", id)[0]
	a, b := initial.Versions[0].ID, initial.Versions[1].ID
	shared, legacy := f.group("series", "shared-series"), f.group("tag", "legacy-shared-credit")
	creditA, creditB := f.group("tag", "edition-a-credit"), f.group("tag", "edition-b-credit")
	internal := f.group("tag", "internal-edition-marker")
	hidden := true
	if _, err := f.groups.UpdateGroups(f.ctx, []groups.GroupUpdate{{ID: groups.GroupID(internal), Internal: &hidden}}); err != nil {
		t.Fatal(err)
	}
	f.join(shared, id, "")
	f.join(legacy, id, "")
	f.join(legacy, id, a) // The same entity can have both real membership scopes.
	f.join(creditA, id, a)
	f.join(creditB, id, b)
	f.join(internal, id, b)
	f.publish("staff", id, nil)

	assertScopes := func(t *testing.T, c *Content, wantVersions int) {
		t.Helper()
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Entities []GroupSummary `json:"entities"`
			Versions []struct {
				ID       string         `json:"id"`
				Entities []GroupSummary `json:"entities"`
			} `json:"versions"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		ids := func(gs []GroupSummary) []string {
			out := make([]string, len(gs))
			for i, g := range gs {
				out[i] = g.ID
			}
			slices.Sort(out)
			return out
		}
		if got := ids(wire.Entities); !slices.Equal(got, sorted(shared, legacy)) {
			t.Fatalf("parent entities include version credits: got %v, want shared %v", got, sorted(shared, legacy))
		}
		if len(wire.Versions) != wantVersions {
			t.Fatalf("got %d versions, want %d", len(wire.Versions), wantVersions)
		}
		for _, v := range wire.Versions {
			want := sorted(creditB)
			if v.ID == a {
				want = sorted(legacy, creditA)
			}
			if got := ids(v.Entities); !slices.Equal(got, want) {
				t.Fatalf("version %s credits: got %v, want %v", v.ID, got, want)
			}
		}
	}
	t.Run("public all versions", func(t *testing.T) {
		assertScopes(t, f.read(anon, "main.test", id)[0], 2)
	})
	t.Run("explicit version", func(t *testing.T) {
		got, err := f.s.contents(f.ctx, f.viewer(anon, "main.test", ViewerOptions{}), []string{id}, a)
		if err != nil {
			t.Fatal(err)
		}
		assertScopes(t, got[0], 1)
	})
}
