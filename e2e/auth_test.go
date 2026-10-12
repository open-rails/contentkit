package e2e

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/authkit/auth"
	contracttest "github.com/open-rails/authkit/auth/authtest"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
)

// ContentKit's gates over a real AuthKit: anonymous without a credential,
// refused for one that fails, a person on user routes, staff by their
// permission in the root group (people and applications alike), each checked
// live, and every refusal with AuthKit's challenge.
func TestGatesWithAuthKit(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	staffUser, staff := w.person(w.staff)
	o, owner := w.person(iam.Role{})
	_, user := w.person(iam.Role{})
	w.owner = o.ID
	f := serve(t, w.engine(nil))
	item := "/gallery/" + cid(1)
	comment := content.CommentInput{Body: "hello"}

	signedOut, gone := w.person(iam.Role{})
	if _, err := w.ak.RevokeAccountSessions(ctx, iam.UserIdentity(signedOut.ID), signedOut.ID); err != nil {
		t.Fatal(err)
	}
	deletedUser, deleted := w.person(iam.Role{})
	if _, err := w.ak.DeleteUsers(ctx, iam.SystemIdentity(), []string{deletedUser.ID}); err != nil {
		t.Fatal(err)
	}
	bannedUser, banned := w.person(iam.Role{})
	if err := w.ak.Ban(ctx, iam.SystemIdentity(), bannedUser.ID, iam.Ban{Reason: "spam"}); err != nil {
		t.Fatal(err)
	}
	key, err := w.ak.CreateAPIKey(ctx, iam.SystemIdentity(), iam.RootGroup(), iam.NewAPIKey{Name: "moderation-bot", Role: w.staff})
	if err != nil {
		t.Fatal(err)
	}
	app := key.Secret
	stale := authtest.StaleSession(t, w.ak, staff)

	for _, tc := range []struct {
		name, bearer, method, target string
		body                         any
		status                       int
		challenge                    string
	}{
		{"open route, a forged credential is never read", owner + "x", "GET", "/config", nil, 200, ""},
		{"public route, anonymous", "", "GET", item + "/comments", nil, 200, ""},
		{"public route, signed in", user, "GET", item + "/comments", nil, 200, ""},
		{"public route, a forged credential", owner + "x", "GET", item + "/comments", nil, 401, `Bearer error="invalid_token"`},
		{"public route, signed out", gone, "GET", item + "/comments", nil, 401, `Bearer error="invalid_token"`},
		{"public route, a deleted account", deleted, "GET", item + "/comments", nil, 401, `Bearer error="invalid_token"`},
		{"public route, a banned account", banned, "GET", item + "/comments", nil, 401, `Bearer error="invalid_token"`},
		{"user route, anonymous", "", "POST", item + "/favorite", nil, 401, "Bearer"},
		{"user route, a person", user, "POST", item + "/favorite", nil, 200, ""},
		{"user route, an application", app, "POST", item + "/favorite", nil, 403, ""},
		{"notifications, anonymous", "", "GET", "/notifications/inbox", nil, 401, "Bearer"},
		{"notifications, a person", user, "GET", "/notifications/inbox", nil, 200, ""},
		{"notifications, an application", app, "GET", "/notifications/inbox", nil, 403, ""},
		{"an application comments", app, "POST", item + "/comments", comment, 403, ""},
		{"a person comments", user, "POST", item + "/comments", comment, 201, ""},
		{"staff route, anonymous", "", "GET", "/posts/admin", nil, 401, "Bearer"},
		{"staff route, without the permission", user, "GET", "/posts/admin", nil, 403, ""},
		{"staff route, staff", staff, "GET", "/posts/admin", nil, 200, ""},
		{"staff route, an application holding it", app, "GET", "/posts/admin", nil, 200, ""},
		{"staff route, a stale sign-in (no step-up)", stale, "GET", "/global-comment-bans", nil, 200, ""},
		{"taxonomy, without the permission", user, "GET", "/taxonomy/nodes", nil, 403, ""},
		{"taxonomy, an application holding it", app, "GET", "/taxonomy/nodes", nil, 200, ""},
		{"upload, an application", app, "POST", "/media/upload/abort", map[string]string{"ticket": "x"}, 403, ""},
		{"media read, a forged credential", owner + "x", "GET", "/media/gallery/" + cid(1), nil, 401, `Bearer error="invalid_token"`},
		{"media read, anonymous", "", "GET", "/media/gallery/" + cid(1), nil, 200, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.call(tc.bearer, tc.method, tc.target, tc.body)
			if rec.Code != tc.status || rec.Header().Get("WWW-Authenticate") != tc.challenge {
				t.Fatalf("%d %q, want %d %q: %s", rec.Code, rec.Header().Get("WWW-Authenticate"), tc.status, tc.challenge, rec.Body)
			}
			if tc.status >= 400 {
				want := map[int]string{401: `"unauthorized"`, 403: `"forbidden"`}[tc.status]
				if !strings.Contains(rec.Body.String(), want) {
					t.Fatalf("body %s, want code %s", rec.Body, want)
				}
			}
		})
	}

	// Moderation by a person and by an application both record their subject.
	var c content.Comment
	f.want(http.StatusCreated, owner, "POST", item+"/comments", comment, &c)
	f.want(http.StatusNoContent, app, "DELETE", "/comments/"+c.ID, nil, nil)
	f.want(http.StatusOK, staff, "POST", "/comments/"+c.ID+"/restore", nil, nil)

	// Permissions are checked live: a revoked role refuses the next request.
	authtest.RevokeRole(t, w.ak, iam.RootGroup(), iam.UserSubject(staffUser.ID), w.staff)
	f.want(http.StatusForbidden, staff, "GET", "/posts/admin", nil, nil)

	// An auth whose roles lack ContentKit's permissions fails the engine,
	// not every request; so does one naming no root scope.
	newRuntime := func(a auth.Authenticator) error {
		deps := w.deps()
		deps.Auth = a
		_, err := contentkit.New(ctx, w.config(), deps)
		return err
	}
	bare, _ := authtest.New(t)
	if err := newRuntime(bare.Authenticator()); err == nil || !strings.Contains(err.Error(), string(access.PostsWrite)) {
		t.Fatalf("roles without ContentKit's permissions: %v", err)
	}
	if err := newRuntime(unrooted{w.ak.Authenticator()}); err == nil || !strings.Contains(err.Error(), "root scope") {
		t.Fatalf("an Authenticator naming no root scope: %v", err)
	}
}

// unrooted is an Authenticator that names no root scope.
type unrooted struct{ a auth.Authenticator }

func (u unrooted) Authenticate(r *http.Request) (auth.Verified, error) { return u.a.Authenticate(r) }

// The AuthKit a host passes conforms to authkit/auth with ContentKit's scope
// and permissions: exactly each permission in exactly the root group, live.
func TestAuthKitConformsWithContentKitPermissions(t *testing.T) {
	w := newWorld(t)
	staffUser, staff := w.person(w.staff)
	_, user := w.person(iam.Role{})
	gone, goneToken := w.person(w.staff)
	if _, err := w.ak.RevokeAccountSessions(t.Context(), iam.UserIdentity(gone.ID), gone.ID); err != nil {
		t.Fatal(err)
	}
	key, err := w.ak.CreateAPIKey(t.Context(), iam.SystemIdentity(), iam.RootGroup(), iam.NewAPIKey{Name: "bot", Role: w.staff})
	if err != nil {
		t.Fatal(err)
	}
	bearer := func(token string) func() *http.Request {
		return func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, prefix+"/posts/admin", nil).WithContext(context.Background())
			r.Header.Set("Authorization", "Bearer "+token)
			return r
		}
	}
	contracttest.Check(t, w.ak.Authenticator(), contracttest.Cases{
		Permissions: access.Names(access.Permissions()...),
		Staff:       bearer(staff),
		User:        bearer(user),
		Stale:       bearer(authtest.StaleSession(t, w.ak, staff)),
		Application: bearer(key.Secret),
		Refused:     map[string]func() *http.Request{"forged": bearer(user + "x"), "signed out": bearer(goneToken)},
		Revoke: func() {
			if _, err := w.ak.RevokeAccountSessions(context.Background(), iam.UserIdentity(staffUser.ID), staffUser.ID); err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		},
	})
}
