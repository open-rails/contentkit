package groups

import (
	"context"
	"errors"
	"testing"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/testauth"
)

type scopeCandidates map[string][]string

func (scopeCandidates) Provision(context.Context, []Scope) error { return nil }
func (scopeCandidates) Retire(context.Context, []string) error   { return nil }
func (s scopeCandidates) Holding(context.Context, string, []string) (map[string][]string, error) {
	return s, nil
}

type limitedScopeAuth struct {
	testauth.Auth
	err error
}

func (limitedScopeAuth) ResolveActor(_ context.Context, v auth.Verified) (string, error) {
	return v.Identity().Subject, nil
}
func (a *limitedScopeAuth) Authorize(_ context.Context, _ auth.Verified, scope, permission string) (bool, error) {
	return scope == "scope-one" && permission == access.GroupContentWrite("channel"), a.err
}

func TestScopedGroupMetadataRetainsCredentialCeilings(t *testing.T) {
	f := newFixture(t)
	channels := f.create(
		GroupInput{Kind: "channel", Slug: "one", OwnerUserID: "other-one", AuthScope: "scope-one"},
		GroupInput{Kind: "channel", Slug: "two", OwnerUserID: "other-two", AuthScope: "scope-two"},
	)
	f.s.scopes = scopeCandidates{
		"scope-one": access.GroupPermissions("channel"),
		"scope-two": access.GroupPermissions("channel"),
	}
	actor := testauth.Actor(access.Actor{ID: "reader"}, testauth.All{})
	limited := &limitedScopeAuth{}
	store := f.s.As(limited, actor)
	page, err := store.Mine(f.ctx, ListOptions{Kind: "channel"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Groups) != 1 || page.Groups[0].ID != channels[0].ID || !page.Groups[0].Writer || page.Groups[0].Editor || page.Groups[0].Moderator {
		t.Fatalf("directory grants escaped the request ceiling: %+v", page.Groups)
	}
	editors := store.contentEditors(f.ctx, ids(channels...))
	if !editors[string(channels[0].ID)] || editors[string(channels[1].ID)] {
		t.Fatalf("content editor metadata escaped the request ceiling: %+v", editors)
	}
	managed, err := f.s.ContentEditorGroups(f.ctx, limited, actor)
	if err != nil || len(managed) != 1 || managed[0] != channels[0].ID {
		t.Fatalf("management search groups escaped credential ceilings: %v %v", managed, err)
	}

	// A permission outage must not publish directory-only grants as usable.
	limited.err = auth.ErrUnavailable
	if _, err := f.s.ContentEditorGroups(f.ctx, limited, actor); !errors.Is(err, auth.ErrUnavailable) {
		t.Fatalf("management search permission outage did not fail closed: %v", err)
	}
	if _, err := store.Mine(f.ctx, ListOptions{Kind: "channel"}); !errors.Is(err, auth.ErrUnavailable) {
		t.Fatalf("permission outage did not fail closed: %v", err)
	}
	if editors := store.contentEditors(f.ctx, ids(channels...)); editors[string(channels[0].ID)] || editors[string(channels[1].ID)] {
		t.Fatalf("permission outage retained editor metadata: %+v", editors)
	}

	legacy, err := f.s.As(testauth.Auth{}, actor).Mine(f.ctx, ListOptions{Kind: "channel"})
	if err != nil || len(legacy.Groups) != 2 || !legacy.Groups[0].Editor || !legacy.Groups[1].Writer {
		t.Fatalf("legacy trusted directory contract changed: %+v %v", legacy.Groups, err)
	}
}
