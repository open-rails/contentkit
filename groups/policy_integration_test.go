package groups

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
)

func TestHardPolicyBindingsProtectEntityMutations(t *testing.T) {
	for _, mode := range []string{"implicit-internal", "fixed-hidden", "required"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			made := f.create(
				GroupInput{Kind: "tag", Slug: "bound", Internal: true, Names: []Name{canonical("en", "Original")}},
				GroupInput{Kind: "tag", Slug: "survivor", Internal: true},
				GroupInput{Kind: "tag", Slug: "unrelated", Names: []Name{canonical("en", "Unrelated")}},
			)
			bound, survivor, unrelated := made[0], made[1], made[2]
			domain := kinds.Domain{Name: "main", Hosts: []string{"restricted.test"}}
			ref := kinds.GroupRef{Kind: bound.Kind, Slug: bound.Slug}
			if mode == "required" {
				domain.RequireEntities = map[string][]kinds.GroupRef{"gallery": {ref}}
			} else {
				filter := kinds.ContentFilter{Group: ref, Visible: false}
				if mode == "fixed-hidden" {
					filter.UserToggle = ptr(false)
				}
				domain.Filters = []kinds.ContentFilter{filter}
			}
			f.s.cfg.ContentFilters = kinds.ContentFilters{Domains: []kinds.Domain{domain}, AppliesTo: []string{"gallery"}}
			check := func(err error) {
				t.Helper()
				var binding *policyBindingError
				if !errors.Is(err, ErrConflict) || !errors.As(err, &binding) || binding.entity != bound.ID || binding.domain != domain.Name {
					t.Fatalf("expected typed policy conflict, got %v", err)
				}
			}
			_, err := f.s.UpdateGroups(f.ctx, []GroupUpdate{{ID: bound.ID, Slug: ptr("renamed")}})
			check(err)
			_, err = f.s.DeleteGroups(f.ctx, []GroupID{bound.ID})
			check(err)
			_, err = f.s.Merge(f.ctx, []Merge{{From: bound.ID, Into: survivor.ID}})
			check(err)
			// A rejected batch must leave otherwise-valid presentation edits intact.
			_, err = f.s.UpdateGroups(f.ctx, []GroupUpdate{{ID: unrelated.ID, Names: []Name{canonical("en", "Should roll back")}}, {ID: bound.ID, Slug: ptr("renamed")}})
			check(err)
			for _, change := range []string{"slug", "state", "kind", "unmark"} {
				if change == "unmark" && mode != "implicit-internal" {
					continue
				}
				t.Run("import-"+change, func(t *testing.T) {
					in := ImportGroup{ID: bound.ID, Kind: bound.Kind, Slug: bound.Slug}
					switch change {
					case "slug":
						in.Slug = "renamed"
					case "state":
						in.State = StateDeleted
					case "kind":
						in.Kind = "artist"
					case "unmark":
						in.Internal = ptr(false)
					}
					tx, err := f.pool.Begin(f.ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(f.ctx)
					check(f.s.ImportGroups(f.ctx, tx, []ImportGroup{in}, false))
				})
			}
			if mode == "implicit-internal" {
				_, err = f.s.UpdateGroups(f.ctx, []GroupUpdate{{ID: bound.ID, Internal: ptr(false)}})
				check(err)
			} else {
				if _, err := f.s.UpdateGroups(f.ctx, []GroupUpdate{{ID: bound.ID, Internal: ptr(false)}}); err != nil {
					t.Fatalf("unchanged fixed/required policy blocked unmark: %v", err)
				}
			}
			if _, err := f.s.UpdateGroups(f.ctx, []GroupUpdate{{ID: bound.ID, Names: []Name{canonical("en", "New display name")}}}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.UpdateGroups(f.ctx, []GroupUpdate{{ID: unrelated.ID, Slug: ptr("unrelated-renamed")}}); err != nil {
				t.Fatal(err)
			}
			got, err := f.s.ManagementDetail(f.ctx, unrelated.ID, "en")
			if err != nil || got.Name != "Unrelated" {
				t.Fatalf("rejected batch changed unrelated names: %+v, %v", got, err)
			}
			// The configuration owner can remove or repoint the rule first.
			f.s.cfg.ContentFilters.Domains = nil
			if _, err := f.s.DeleteGroups(f.ctx, []GroupID{bound.ID}); err != nil {
				t.Fatalf("removed policy still blocked mutation: %v", err)
			}
		})
	}
}

func TestHardPolicyBindingConflictsAreActionableOverHTTP(t *testing.T) {
	f := newFixture(t)
	bound := f.create(GroupInput{Kind: "tag", Slug: "restricted", Internal: true})[0]
	f.s.cfg.ContentFilters.Domains, f.s.cfg.ContentFilters.AppliesTo = []kinds.Domain{{Name: "main", Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "restricted"}, Visible: false}}}}, []string{"gallery"}
	h, err := f.s.Handler(testauth.Auth{Authz: testauth.All{}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodDelete, "/entities/"+string(bound.ID), nil)
	r.Header.Set("Authorization", "Bearer user:staff")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"conflict"`) || !strings.Contains(rec.Body.String(), "update the policy first") {
		t.Fatalf("policy failure was not actionable: %d %s", rec.Code, rec.Body)
	}
}

func TestInactiveInternalPolicyEvidenceCannotBeRenamedAway(t *testing.T) {
	f := newFixture(t)
	bound := f.create(GroupInput{Kind: "tag", Slug: "restricted", Internal: true})[0]
	f.s.cfg.ContentFilters = kinds.ContentFilters{Domains: []kinds.Domain{{Name: "main", Filters: []kinds.ContentFilter{{Group: kinds.GroupRef{Kind: "tag", Slug: "restricted"}, Visible: false}}}}, AppliesTo: []string{"gallery"}}
	// Simulate an entity deleted by an older binary before mutation guards.
	f.exec("UPDATE {s}.content_groups SET state='deleted' WHERE id=$1", bound.ID)
	if _, err := f.s.UpdateGroups(f.ctx, []GroupUpdate{{ID: bound.ID, Slug: ptr("renamed")}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("inactive marker lost its policy identity: %v", err)
	}
	if _, err := f.s.UpdateGroups(f.ctx, []GroupUpdate{{ID: bound.ID, Internal: ptr(false)}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("inactive marker lost internal evidence: %v", err)
	}
	if _, err := f.s.UpdateGroups(f.ctx, []GroupUpdate{{ID: bound.ID, State: ptr(StateActive)}}); err != nil {
		t.Fatalf("safe restore should be allowed: %v", err)
	}
}
