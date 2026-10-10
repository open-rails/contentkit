package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/keys"
	"github.com/open-rails/authkit/verify"
	helpersauth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/contentkit/access"
)

// AuthKit's JSON API is served at {authAPIPath}/v1.
const authAPIPath = "/api/auth"

var (
	rbac = authkit.NewRoles()
	// perms maps content.Perms' opaque strings to AuthKit permissions.
	perms = map[string]iam.Perm{
		"post":     rbac.Root.Permission("posts", "write"),
		"poll":     rbac.Root.Permission("polls", "write"),
		"moderate": rbac.Root.Permission("comments", "moderate"),
		"review":   rbac.Root.Permission("moderation", "review"),
		"ban":      rbac.Root.Permission("comments", "ban"),
		"taxonomy": rbac.Root.Permission("taxonomy", "write"),
	}
	// permMedia lets staff edit every item's media.
	permMedia = rbac.Root.Permission("media", "edit")
	// roles are what /__test/users hands out.
	roles = map[string]iam.Role{
		"staff":     rbac.Root.Role("staff", permMedia, perms["post"], perms["poll"], perms["moderate"], perms["review"], perms["ban"], perms["taxonomy"]),
		"moderator": rbac.Root.Role("moderator", perms["moderate"], perms["review"], perms["ban"]),
		"editor":    rbac.Root.Role("editor", perms["post"], perms["poll"]),
	}
)

func newAuth(ctx context.Context, origin string, pool *pgxpool.Pool) (*authkit.Client, *authtest.Outbox, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	signer, err := keys.SignerFromKey("e2e-1", key)
	if err != nil {
		return nil, nil, err
	}
	totp := make([]byte, 32)
	if _, err := rand.Read(totp); err != nil {
		return nil, nil, err
	}
	limits := authkit.DefaultRateLimits()
	for bucket := range limits {
		limits[bucket] = authkit.RateLimit{Limit: 100000, Window: time.Minute}
	}
	outbox := &authtest.Outbox{}
	auth, err := authkit.New(ctx, authkit.Config{
		HTTP:     &authkit.HTTPConfig{DirectPeerIP: true, APIPath: authAPIPath, RefreshCookie: true, RateLimits: limits},
		Database: authkit.DatabaseConfig{Schema: authSchema, RiverSchema: riverSchema},
		Roles:    rbac,
		// Shared test accounts sign in from many pages at once: no session cap.
		Token:     authkit.TokenConfig{Issuer: origin, IssuedAudiences: []string{"contentkit-e2e"}, SessionMaxPerUser: -1},
		SignIn:    authkit.SignInConfig{AccountsPerDevice: -1, AccountsPerAddress: -1, NewDevicesPerAccount: -1},
		TwoFactor: authkit.TwoFactorConfig{TOTPSecretKey: totp},
	}, authkit.Deps{
		Postgres:  pool,
		KeySource: keys.Static{Active: signer, Public: map[string]crypto.PublicKey{signer.KID(): signer.Public()}},
		Email:     outbox.Email(),
		SMS:       outbox.SMS(),
	})
	return auth, outbox, err
}

type actorKey struct{}

// identify is the host's auth middleware in front of ContentKit: AuthKit's
// gate verifies a bearer (refusing a bad one with 401) and the verified user
// becomes the actor; without one the actor is anonymous.
func (h *harness) identify(next http.Handler) http.Handler {
	return verify.Optional(h.auth)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := access.Actor{Anonymous: true}
		if id, ok := verify.IdentityFromContext(r.Context()); ok && id.SubjectKind == helpersauth.SubjectUser {
			a = access.Actor{ID: id.Subject, Kind: "user"}
		}
		// The harness stands behind a trusted local proxy: tests give each
		// signed-out visitor its own address.
		if a.IP = clientIP(r); r.Header.Get("X-Forwarded-For") != "" {
			a.IP = strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, a)))
	}))
}

// identity is content.Identity and media.Identity over identify's actor.
type identity struct{}

func (identity) Actor(ctx context.Context) (access.Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(access.Actor)
	return a, ok
}

// Can is content.Authorizer: the permission checked live in AuthKit's root
// group for the identity the gate verified.
func (h *harness) Can(ctx context.Context, a access.Actor, perm string) (bool, error) {
	p, ok := perms[perm]
	if !ok {
		return false, fmt.Errorf("unknown permission %q", perm)
	}
	return h.can(ctx, a, p)
}

func (h *harness) can(ctx context.Context, a access.Actor, p iam.Perm) (bool, error) {
	who, ok := verify.IdentityFromContext(ctx)
	if !ok || a.Anonymous || who.Subject != a.ID {
		return false, nil
	}
	return h.auth.Can(ctx, who, iam.RootGroup(), p)
}

// testUser is a signed-in account the control surface hands a test.
type testUser struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Email       string `json:"email"`
	Password    string `json:"password"`
	Role        string `json:"role,omitempty"`
	AccessToken string `json:"access_token"`
}

// newUser creates a verified account with authtest, holding role in the root
// group ("" none), and signs it in through AuthKit's HTTP surface.
func (h *harness) newUser(role string) (u testUser, err error) {
	r, ok := roles[role]
	if !ok && role != "" {
		return u, fmt.Errorf("unknown role %q", role)
	}
	defer recoverFailure(&err)
	t := requestTB{}
	au := authtest.NewUser(t, h.auth)
	if ok {
		authtest.GrantRole(t, h.auth, iam.RootGroup(), iam.UserSubject(au.ID), r)
	}
	tokens := authtest.SignIn(t, h.auth, au)
	return testUser{ID: au.ID, Username: au.Username, Email: au.Email, Password: au.Password, Role: role, AccessToken: tokens.AccessToken}, nil
}

// requestTB runs authtest's helpers outside a test: a failure panics with
// failure, which recoverFailure turns into the request's error. Only the
// methods those helpers call are implemented.
type requestTB struct{ testing.TB }

type failure string

func (requestTB) Helper()                      {}
func (requestTB) Cleanup(func())               {}
func (requestTB) Logf(string, ...any)          {}
func (requestTB) Fatal(args ...any)            { panic(failure(fmt.Sprint(args...))) }
func (requestTB) Fatalf(f string, args ...any) { panic(failure(fmt.Sprintf(f, args...))) }
func (requestTB) Errorf(f string, args ...any) { panic(failure(fmt.Sprintf(f, args...))) }

func recoverFailure(err *error) {
	if p := recover(); p != nil {
		f, ok := p.(failure)
		if !ok {
			panic(p)
		}
		*err = fmt.Errorf("%s", string(f))
	}
}
