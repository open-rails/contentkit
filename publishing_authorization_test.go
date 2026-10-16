package contentkit

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/gate"
)

func TestPublishingAuthorityRecognizesOnlyInprocessHost(t *testing.T) {
	e := &Engine{cfg: Config{Tenant: "one"}}
	authority := publishingAuthority{e}
	stored, err := authority.Capture(t.Context(), gate.Host())
	if err != nil {
		t.Fatal(err)
	}
	actor, err := authority.Resume(t.Context(), stored)
	if err != nil || !gate.IsHost(actor) {
		t.Fatalf("host resume: %v %v", actor, err)
	}
	if _, err := (publishingAuthority{&Engine{cfg: Config{Tenant: "two"}}}).Resume(t.Context(), stored); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("resumed another app's delegation: %v", err)
	}
	tenant := NewTenantID()
	first := publishingAuthority{&Engine{cfg: Config{Tenant: "same"}, scope: &Scope{TenantID: tenant, AppID: NewAppID()}}}
	firstGrant, err := first.Capture(t.Context(), gate.Host())
	if err != nil {
		t.Fatal(err)
	}
	sibling := publishingAuthority{&Engine{cfg: Config{Tenant: "same"}, scope: &Scope{TenantID: tenant, AppID: NewAppID()}}}
	if _, err := sibling.Resume(t.Context(), firstGrant); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("host delegation crossed canonical app scope: %v", err)
	}
	fake := access.Actor{ID: gate.HostSubject, Kind: auth.SubjectApplication, Verified: publishingFakeHost{}}
	if _, err := authority.Capture(t.Context(), fake); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("host-looking external identity captured: %v", err)
	}
	invalid, _ := json.Marshal(publishingDelegation{App: authority.scopeKey(), Host: true, Credential: json.RawMessage(`{"key":"external"}`)})
	if _, err := authority.Resume(t.Context(), invalid); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("mixed host and external credential accepted: %v", err)
	}
}

type publishingFakeHost struct{}

func (publishingFakeHost) Identity() auth.Identity {
	return auth.Identity{Subject: gate.HostSubject, SubjectKind: auth.SubjectApplication}
}

func TestPublishingRoutesRequireMedia(t *testing.T) {
	selected, err := ParseRouteGroups([]string{"publishing"})
	if err != nil || selected != PublishingRoutes || AllRoutes&PublishingRoutes == 0 {
		t.Fatalf("publishing route selection: %v %v", selected, err)
	}
	e := &Engine{}
	if e.RouteGroups()&PublishingRoutes != 0 {
		t.Fatal("engine without media advertised publishing routes")
	}
	if _, err := e.Routes(Routes{Groups: PublishingRoutes}); err == nil {
		t.Fatal("engine without media mounted publishing routes")
	}
}
