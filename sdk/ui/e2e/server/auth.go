package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/keys"

	"github.com/open-rails/contentkit/access"
)

// AuthKit's JSON API is served at {authAPIPath}/v1.
const authAPIPath = "/api/auth"

var (
	rbac = authkit.NewRoles()
	// perm is ContentKit's staff permissions as AuthKit's: the host declares
	// them from access.Permissions() and builds its roles from the constants.
	perm = func() map[access.Permission]iam.Perm {
		out := map[access.Permission]iam.Perm{}
		for _, p := range access.Permissions() {
			out[p] = rbac.Root.Declare(p.String())[0]
		}
		return out
	}()
	// roles are what /__test/users hands out.
	roles = map[string]iam.Role{
		"staff": rbac.Root.Role("staff", perm[access.ContentWrite], perm[access.ContentViewRestricted], perm[access.PollsWrite], perm[access.CommentsModerate],
			perm[access.ModerationReview], perm[access.CommentsBan], perm[access.GroupsWrite]),
		"moderator": rbac.Root.Role("moderator", perm[access.CommentsModerate], perm[access.ModerationReview], perm[access.CommentsBan]),
		"editor":    rbac.Root.Role("editor", perm[access.ContentWrite], perm[access.PollsWrite]),
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

// behindProxy is the host's trusted-proxy step in front of ContentKit: the
// client's address in X-Forwarded-For becomes RemoteAddr, which ContentKit
// keys signed-out visitors by. Tests give each signed-out visitor its own.
func behindProxy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			r.RemoteAddr = net.JoinHostPort(strings.TrimSpace(strings.Split(xff, ",")[0]), "0")
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
