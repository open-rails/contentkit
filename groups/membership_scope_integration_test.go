package groups

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/open-rails/contentkit/internal/testauth"
)

func TestScopedContentMembershipsPreserveOtherScopes(t *testing.T) {
	f := newFixture(t)
	f.content("gallery", 1, live("en"), live("es"))
	f.content("gallery", 2, live("en"))
	gs := f.create(GroupInput{Kind: "artist", Slug: "artist-before"}, GroupInput{Kind: "artist", Slug: "artist-after"},
		GroupInput{Kind: "tag", Slug: "shared-legacy-tag"}, GroupInput{Kind: "tag", Slug: "first-tag"},
		GroupInput{Kind: "tag", Slug: "second-tag"}, GroupInput{Kind: "tag", Slug: "replacement"},
		GroupInput{Kind: "voice_actor", Slug: "voice"})
	f.add(Membership{ContentID: cid(1), GroupID: gs[0].ID}, Membership{ContentID: cid(1), GroupID: gs[2].ID},
		Membership{ContentID: cid(1), VersionID: vid(1, 0), GroupID: gs[3].ID},
		Membership{ContentID: cid(1), VersionID: vid(1, 1), GroupID: gs[4].ID},
		Membership{ContentID: cid(1), VersionID: vid(1, 0), GroupID: gs[6].ID})
	set := func(scope string, kinds []string, ms ...Membership) error {
		return f.s.SetContentGroups(f.ctx, []ContentGroups{{ContentID: cid(1), VersionScope: &scope, Kinds: kinds, Memberships: ms}})
	}
	if err := set("", []string{"artist"}, Membership{GroupID: gs[1].ID}); err != nil {
		t.Fatal(err)
	}
	if err := set(vid(1, 0), []string{"tag"}, Membership{GroupID: gs[5].ID}); err != nil {
		t.Fatal(err)
	}
	if err := set(vid(1, 1), []string{"tag"}); err != nil {
		t.Fatal(err)
	}
	all, err := f.s.MembershipsOf(f.ctx, []string{cid(1)})
	if err != nil {
		t.Fatal(err)
	}
	want := map[memberKey]bool{
		{group: string(gs[1].ID), content: cid(1)}:                     true,
		{group: string(gs[2].ID), content: cid(1)}:                     true,
		{group: string(gs[5].ID), content: cid(1), version: vid(1, 0)}: true,
		{group: string(gs[6].ID), content: cid(1), version: vid(1, 0)}: true,
	}
	if len(all[cid(1)]) != len(want) {
		t.Fatalf("scoped replacement changed unrelated links: %+v", all[cid(1)])
	}
	for _, m := range all[cid(1)] {
		if !want[memberKey{group: string(m.GroupID), content: m.ContentID, version: m.VersionID, role: m.Role}] {
			t.Fatalf("unexpected membership %+v", m)
		}
	}
	before, _ := json.Marshal(all)
	for _, tc := range []struct {
		name, scope string
		kinds       []string
		memberships []Membership
		want        error
	}{
		{"shared rejects version kinds", "", []string{"tag"}, nil, ErrInvalid},
		{"version rejects shared kinds", vid(1, 0), []string{"artist"}, nil, ErrInvalid},
		{"shared rejects version ids", "", []string{"artist"}, []Membership{{GroupID: gs[1].ID, VersionID: vid(1, 0)}}, ErrInvalid},
		{"version rejects sibling ids", vid(1, 0), []string{"tag"}, []Membership{{GroupID: gs[3].ID, VersionID: vid(1, 1)}}, ErrInvalid},
		{"missing version even when clearing", vid(1, 9), []string{"tag"}, nil, ErrNotFound},
		{"foreign version even when clearing", vid(2, 0), []string{"tag"}, nil, ErrNotFound},
		{"invalid version", "invalid", []string{"tag"}, nil, ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := set(tc.scope, tc.kinds, tc.memberships...); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			after, err := f.s.MembershipsOf(f.ctx, []string{cid(1)})
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(after)
			if string(before) != string(body) {
				t.Fatal("refused write changed memberships")
			}
		})
	}
	f.exec(`UPDATE {s}.content_versions SET state='deleted' WHERE tenant_id=$1 AND id=$2`, tenant, vid(1, 1))
	if err := set(vid(1, 1), []string{"tag"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted version accepted: %v", err)
	}
	// Omitting kinds clears only the kinds owned by the requested scope.
	if err := set("", nil); err != nil {
		t.Fatal(err)
	}
	all, _ = f.s.MembershipsOf(f.ctx, []string{cid(1)})
	if len(all[cid(1)]) != 3 {
		t.Fatalf("shared default touched version kinds: %+v", all[cid(1)])
	}
	if err := set(vid(1, 0), nil); err != nil {
		t.Fatal(err)
	}
	all, _ = f.s.MembershipsOf(f.ctx, []string{cid(1)})
	if len(all[cid(1)]) != 1 || all[cid(1)][0].GroupID != gs[2].ID || all[cid(1)][0].VersionID != "" {
		t.Fatalf("version default touched shared link: %+v", all[cid(1)])
	}
	// A kind without version-level entities still accepts an empty scoped
	// replacement without accidentally defaulting to every shared kind.
	kind := f.s.cfg.Kinds["gallery"]
	kind.Groups = []string{"artist"}
	f.s.cfg.Kinds["gallery"] = kind
	if err := set(vid(1, 0), nil); err != nil {
		t.Fatalf("empty version scope: %v", err)
	}
}

func TestScopedContentMembershipsConcurrentVersionsAndNames(t *testing.T) {
	f := newFixture(t)
	f.content("gallery", 1, live("en"), live("es"))
	actor := f.s.cfg.GroupKinds["voice_actor"]
	actor.Creatable = true
	f.s.cfg.GroupKinds["voice_actor"] = actor
	artist := f.create(GroupInput{Kind: "artist", Slug: "shared-artist"})[0]
	f.add(Membership{ContentID: cid(1), GroupID: artist.ID})
	start := make(chan struct{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			version := vid(1, i)
			errs[i] = f.s.SetContentGroups(f.ctx, []ContentGroups{{ContentID: cid(1), VersionScope: &version,
				Kinds: []string{"voice_actor"}, Create: []NewEntity{{Kind: "voice_actor", Name: "Same Actor", Language: "en"}}}})
		}(i)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	all, err := f.s.MembershipsOf(f.ctx, []string{cid(1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(all[cid(1)]) != 3 {
		t.Fatalf("concurrent sets lost a scope: %+v", all[cid(1)])
	}
	var actorID GroupID
	seen := map[string]bool{}
	for _, m := range all[cid(1)] {
		if m.GroupID == artist.ID {
			if m.VersionID != "" {
				t.Fatal("shared artist moved to a version")
			}
			continue
		}
		if actorID != "" && actorID != m.GroupID {
			t.Fatal("concurrent named creation duplicated the entity")
		}
		actorID = m.GroupID
		seen[m.VersionID] = true
	}
	if len(seen) != 2 || !seen[vid(1, 0)] || !seen[vid(1, 1)] {
		t.Fatalf("new names attached outside their versions: %+v", all[cid(1)])
	}
}

func TestScopedContentMembershipsHTTP(t *testing.T) {
	f := newFixture(t)
	f.content("gallery", 1, live("en"), live("es"))
	f.content("gallery", 2, live("en"))
	gs := f.create(GroupInput{Kind: "artist", Slug: "shared"}, GroupInput{Kind: "voice_actor", Slug: "actor"})
	h, err := f.s.Handler(testauth.Auth{Authz: testauth.All{}})
	if err != nil {
		t.Fatal(err)
	}
	call := func(path, body, bearer string, status int) ContentMemberships {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != status {
			t.Fatalf("PUT %s: %d %s, want %d", path, rec.Code, rec.Body, status)
		}
		var out ContentMemberships
		if status == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	shared := "/contents/" + cid(1) + "/entities/shared"
	version := "/contents/" + cid(1) + "/versions/" + vid(1, 0) + "/entities"
	call(shared, `{"memberships":[{"group_id":"`+string(gs[0].ID)+`"}]}`, "user:u1", http.StatusOK)
	out := call(version, `{"memberships":[{"group_id":"`+string(gs[1].ID)+`"}]}`, "user:u1", http.StatusOK)
	if len(out.Memberships) != 2 || !slices.ContainsFunc(out.Memberships, func(m Membership) bool { return m.GroupID == gs[1].ID && m.VersionID == vid(1, 0) }) {
		t.Fatalf("route scope not applied: %+v", out.Memberships)
	}
	call(shared, `{"memberships":[]}`, "", http.StatusUnauthorized)
	call(version, `{"memberships":[]}`, "", http.StatusUnauthorized)
	call("/contents/"+cid(1)+"/versions/"+vid(2, 0)+"/entities", `{"memberships":[]}`, "user:u1", http.StatusNotFound)
	// The wire cannot override the route-implied scope or inject it into the
	// unchanged legacy endpoint.
	for _, path := range []string{shared, version, "/contents/" + cid(1) + "/entities"} {
		call(path, `{"memberships":[],"version_scope":"`+vid(1, 1)+`"}`, "user:u1", http.StatusBadRequest)
	}
	call(shared, `{"kinds":["voice_actor"],"memberships":[]}`, "user:u1", http.StatusBadRequest)
	call(version, `{"kinds":["artist"],"memberships":[]}`, "user:u1", http.StatusBadRequest)
}
