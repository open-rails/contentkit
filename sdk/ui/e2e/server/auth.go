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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/keys"

	"github.com/open-rails/contentkit/access"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
)

// AuthKit's issuer is the app origin, so its JSON API is at /auth/v1.
const authAPIPath = "/auth/v1"

const accountTenantID = "00000000-0000-4000-8000-000000000201"

var (
	rbac       = authkit.NewRoles()
	appPersona = rbac.Persona("app")
	// perm is ContentKit's staff permissions as AuthKit's: the host declares
	// them from access.Permissions() and builds its roles from the constants.
	perm = func() map[access.Permission]iam.Perm {
		out := map[access.Permission]iam.Perm{}
		for _, p := range access.Permissions() {
			out[p] = appPersona.Declare(ckauth.AppPermission(p))[0]
		}
		return out
	}()
	// The persona of authoring channels' groups (the site enables channels).
	_ = rbac.Persona("channel").Declare(access.ChannelPermissions()...)
	// Every fixture member can create/edit their own posts. Other roles add staff powers.
	contributor = appPersona.Role("contributor", perm[access.ContentCreate], perm[access.ContentEditOwn])
	// roles are what /__test/users additionally hands out.
	roles = map[string]iam.Role{
		"staff": appPersona.Role("staff", perm[access.ContentWrite], perm[access.ContentViewRestricted], perm[access.PollsWrite], perm[access.CommentsModerate],
			perm[access.ModerationReview], perm[access.CommentsBan], perm[access.EntitiesWrite], perm[access.LocationsWrite]),
		"moderator": appPersona.Role("moderator", perm[access.CommentsModerate], perm[access.ModerationReview], perm[access.CommentsBan]),
		"editor":    appPersona.Role("editor", perm[access.ContentWrite], perm[access.PollsWrite]),
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
	outbox := &authtest.Outbox{}
	tenantID, err := iam.ParseTenantID(accountTenantID)
	if err != nil {
		return nil, nil, err
	}
	cfg := authkit.Config{
		Issuer: origin,
		// The account tenant is a fixed ID the media gateway's routes and the
		// media registry name, which a single-tenant AuthKit would mint itself.
		Tenancy:         authkit.TenancyMulti,
		DefaultTenantID: tenantID,
		HTTP:            &authkit.HTTPConfig{DirectPeerIP: true},
		Database:        authkit.DatabaseConfig{Schema: authSchema, RiverSchema: authRiver},
		Roles:           rbac,
	}
	// Shared test accounts sign in from many pages at once: no limits, no session cap.
	authtest.LiftLimits(&cfg)
	auth, err := authkit.New(ctx, cfg, authkit.Deps{
		Postgres:  pool,
		KeySource: keys.Static{Active: signer, Public: map[string]crypto.PublicKey{signer.KID(): signer.Public()}, TOTP: totp},
		Email:     outbox.Email(),
		SMS:       outbox.SMS(),
	})
	if err != nil {
		return nil, nil, err
	}
	persona, err := auth.Persona("app")
	if err != nil {
		return nil, nil, err
	}
	if _, err := auth.CreateGroup(ctx, iam.NewGroup{ID: appID, TenantID: tenantID, Persona: persona}); err != nil {
		return nil, nil, err
	}
	return auth, outbox, nil
}

// withProofs presents a test user's token sent as Bearer (by the Node seeds
// and the bench) as DPoP with a fresh proof: AuthKit binds every sign-in
// session to a key, and only this process holds the keys of the sessions it
// signed in. Any other credential passes unchanged.
func (h *harness) withProofs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") && r.Header.Get("DPoP") == "" {
			r = r.Clone(r.Context())
			authtest.Authorize(requestTB{}, h.auth, r, token)
		}
		next.ServeHTTP(w, r)
	})
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

// newUser creates a verified account with contributor and optional app roles,
// and signs it in through AuthKit's HTTP surface.
func (h *harness) newUser(role string) (u testUser, err error) {
	r, ok := roles[role]
	if !ok && role != "" {
		return u, fmt.Errorf("unknown role %q", role)
	}
	defer recoverFailure(&err)
	t := requestTB{}
	au := authtest.NewUser(t, h.auth)
	authtest.GrantRole(t, h.auth, iam.GroupByID(appID), iam.UserSubject(au.ID), contributor)
	if ok {
		authtest.GrantRole(t, h.auth, iam.GroupByID(appID), iam.UserSubject(au.ID), r)
	}
	tokens := authtest.SignIn(t, h.auth, au)
	u = testUser{ID: au.ID, Username: au.Username, Email: au.Email, Password: au.Password, Role: role, AccessToken: tokens.AccessToken}
	h.users.Store(au.ID, u)
	return u, nil
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
