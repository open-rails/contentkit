package gate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/helpers/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/httpapi"
)

var scope = auth.Scope{Authority: "https://auth.test", ID: "root"}

const moderate = string(access.CommentsModerate)

// provider answers Authenticate with err, else v; v counts its Can calls.
type provider struct {
	v   auth.Verified
	err error
}

func (provider) RootScope(context.Context) (auth.Scope, error) { return scope, nil }

// unrooted names no root scope.
type unrooted struct{ v auth.Verified }

func (u unrooted) Authenticate(*http.Request) (auth.Verified, error) { return u.v, nil }

// rootDown cannot resolve its root scope.
type rootDown struct{ provider }

func (rootDown) RootScope(context.Context) (auth.Scope, error) {
	return auth.Scope{}, errors.New("db down")
}

func (p provider) Authenticate(*http.Request) (auth.Verified, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.v, nil
}

type panics struct{}

func (panics) Authenticate(*http.Request) (auth.Verified, error) { panic("boom") }

type verified struct {
	id    auth.Identity
	holds map[string]bool
	err   error
	calls *int
}

func (v verified) Identity() auth.Identity { return v.id }

func (v verified) Can(_ context.Context, s auth.Scope, perm string) (bool, error) {
	*v.calls++
	return s == scope && v.holds[perm], v.err
}

func person(id string, holds ...string) verified {
	h := map[string]bool{}
	for _, p := range holds {
		h[p] = true
	}
	return verified{id: auth.Identity{Issuer: "https://auth.test", Subject: id, SubjectKind: auth.SubjectUser,
		Invoker: auth.Invoker{Issuer: "https://auth.test", ID: id}}, holds: h, calls: new(int)}
}

type reply struct {
	status    int
	code      string
	challenge string
	actor     access.Actor
}

// serve runs one request with Authorization header bearer ("" for none)
// through a route of tier, the handler checking perm once more.
func serve(t *testing.T, a auth.Authenticator, tier httpapi.Tier, bearer string) reply {
	t.Helper()
	g := &Gate{Auth: a}
	spec := httpapi.Spec{Method: "GET", Path: "/x", Auth: tier}
	if tier == httpapi.Staff {
		spec.Perm = moderate
	}
	var got access.Actor
	h := g.Guard(spec, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = Actor(r.Context())
		if tier == httpapi.Staff {
			if ok, err := got.Can(r.Context(), scope, moderate); !ok || err != nil {
				t.Errorf("the handler's own check: %v %v", ok, err)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	r := httptest.NewRequest("GET", "/x", nil)
	r.RemoteAddr = "203.0.113.7:4444"
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var body struct{ Code string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return reply{rec.Code, body.Code, rec.Header().Get("WWW-Authenticate"), got}
}

func TestGate(t *testing.T) {
	unauth := provider{err: auth.ErrUnauthenticated}
	staff := person("s1", moderate)
	app := person("app1", moderate)
	app.id.SubjectKind = auth.SubjectApplication
	onBehalf := person("app1")
	onBehalf.id.SubjectKind, onBehalf.id.Invoker = auth.SubjectApplication, auth.Invoker{Issuer: "https://other.test", ID: "u9"}
	expired := person("s1", moderate)
	expired.err = errors.Join(auth.ErrUnauthenticated, auth.ErrExpired)
	down := person("s1", moderate)
	down.err = errors.New("directory down")
	nobody := person("")
	for _, tc := range []struct {
		name      string
		a         auth.Authenticator
		tier      httpapi.Tier
		bearer    string
		status    int
		code      string
		challenge string
	}{
		{"open never authenticates", panics{}, httpapi.Open, "x", 204, "", ""},
		{"public, no credential: anonymous", unauth, httpapi.Public, "", 204, "", ""},
		{"public, a failing credential", unauth, httpapi.Public, "bad", 401, "unauthorized", `Bearer error="invalid_token"`},
		{"public, expired", provider{err: errors.Join(auth.ErrUnauthenticated, auth.ErrExpired)}, httpapi.Public, "old", 401, "unauthorized", `Bearer error="invalid_token"`},
		{"public, refused by policy", provider{err: auth.ErrForbidden}, httpapi.Public, "t", 403, "forbidden", ""},
		{"public, provider down", provider{err: auth.ErrUnavailable}, httpapi.Public, "t", 503, "unavailable", ""},
		{"public, provider panics", panics{}, httpapi.Public, "t", 503, "unavailable", ""},
		{"public, no subject", provider{v: nobody}, httpapi.Public, "t", 503, "unavailable", ""},
		{"user, anonymous", unauth, httpapi.User, "", 401, "unauthorized", "Bearer"},
		{"user, a person", provider{v: person("u1")}, httpapi.User, "t", 204, "", ""},
		{"user, an application", provider{v: app}, httpapi.User, "t", 403, "forbidden", ""},
		{"staff, anonymous", unauth, httpapi.Staff, "", 401, "unauthorized", "Bearer"},
		{"staff, without the permission", provider{v: person("u1")}, httpapi.Staff, "t", 403, "forbidden", ""},
		{"staff, holding it", provider{v: staff}, httpapi.Staff, "t", 204, "", ""},
		{"staff, an application holding it", provider{v: app}, httpapi.Staff, "t", 204, "", ""},
		{"staff, credential ended since", provider{v: expired}, httpapi.Staff, "t", 401, "unauthorized", `Bearer error="invalid_token"`},
		{"staff, the check failed", provider{v: down}, httpapi.Staff, "t", 503, "unavailable", ""},
		{"staff, no root scope", unrooted{staff}, httpapi.Staff, "t", 503, "unavailable", ""},
		{"staff, root scope unresolved", rootDown{provider{v: staff}}, httpapi.Staff, "t", 503, "unavailable", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := serve(t, tc.a, tc.tier, tc.bearer)
			if got.status != tc.status || got.code != tc.code || got.challenge != tc.challenge {
				t.Fatalf("%d %q %q, want %d %q %q", got.status, got.code, got.challenge, tc.status, tc.code, tc.challenge)
			}
		})
	}

	if a := serve(t, unauth, httpapi.Public, "").actor; !a.Anonymous || a.IP != "203.0.113.7" || a.Verified != nil {
		t.Fatalf("anonymous actor %+v", a)
	}
	*staff.calls = 0
	if a := serve(t, provider{v: staff}, httpapi.Staff, "t").actor; a.ID != "s1" || a.Kind != auth.SubjectUser || a.Invoker != (auth.Invoker{}) || a.IP != "203.0.113.7" {
		t.Fatalf("staff actor %+v", a)
	}
	if *staff.calls != 1 {
		t.Fatalf("the gate and the handler asked Can %d times, want once", *staff.calls)
	}
	if a := serve(t, provider{v: onBehalf}, httpapi.Public, "t").actor; a.ID != "app1" || a.Invoker != (auth.Invoker{Issuer: "https://other.test", ID: "u9"}) {
		t.Fatalf("invoker %+v", a)
	}
}

type catalog struct{ provider }

func (catalog) KnownPermission(p string) bool { return p == moderate }

func TestCheck(t *testing.T) {
	var typedNil *catalog
	if Check(nil) == nil || Check(typedNil) == nil {
		t.Fatal("a missing Authenticator passed")
	}
	if err := Check(unrooted{}); err != nil {
		t.Fatalf("a module without staff routes: %v", err)
	}
	if err := Check(catalog{}, access.CommentsModerate); err != nil {
		t.Fatalf("a declared permission: %v", err)
	}
	if err := Check(provider{}, access.Permissions()...); err != nil {
		t.Fatalf("an Authenticator without a catalog: %v", err)
	}
	if err := Check(catalog{}, access.CommentsModerate, access.CommentsBan); err == nil {
		t.Fatal("a permission the Authenticator does not know passed")
	}
	if err := Check(unrooted{}, access.CommentsModerate); err == nil {
		t.Fatal("an Authenticator naming no root scope passed")
	}
}
