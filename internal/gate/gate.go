// Package gate builds ContentKit's route gates from the host's auth. It asks
// the auth.Authenticator who a request is, at most once, builds the
// access.Actor from the Verified, enforces the route's tier (httpapi.Tier)
// and answers every refusal with the status and challenge authkit/auth's
// Refuse gives, in ContentKit's error body.
package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/readerprefs"
)

// Gate guards one module's routes.
type Gate struct {
	Auth auth.Authenticator
	// Then wraps each route inside the gate, after the actor is bound.
	Then func(http.Handler) http.Handler
	Log  *slog.Logger
}

// ScopedAuthorization is the fixed app policy installed by a scoped engine.
// It receives the provider's original verified credential, never an identity
// reconstructed from request data. Legacy authenticators do not implement it.
type ScopedAuthorization interface {
	ResolveActor(context.Context, auth.Verified) (string, error)
	Authorize(context.Context, auth.Verified, string, string) (bool, error)
}

// ScopedAccounts is the optional tenant-membership part of app authorization.
// It never substitutes an operator's source account for a target-tenant member.
type ScopedAccounts interface {
	ResolveTenantAccount(context.Context, auth.Verified, string) (string, bool, error)
}

// Check refuses a missing Authenticator, typed nil pointers included. A
// module with staff routes passes their permissions: the Authenticator must
// name its root scope (auth.Root), where they are held, and know each one
// when it can say (auth.PermissionCatalog), so a host whose roles lack one
// fails at start instead of refusing every request.
func Check(a auth.Authenticator, perms ...access.Permission) error {
	if isNil(a) {
		return errors.New("an auth.Authenticator is required")
	}
	if len(perms) == 0 {
		return nil
	}
	if _, scoped := a.(ScopedAuthorization); scoped {
		return nil
	}
	if _, ok := a.(auth.Root); !ok {
		return fmt.Errorf("%T names no root scope (auth.Root), where staff hold their permissions", a)
	}
	if c, ok := a.(auth.PermissionCatalog); ok {
		for _, p := range perms {
			if !c.KnownPermission(string(p)) {
				return fmt.Errorf("the Authenticator does not know %q: declare access.Permissions() in its roles", p)
			}
		}
	}
	return nil
}

// Can reports whether actor holds perm, an access.Permission's name, as
// staff: the root:<resource>:<action> permission in the Authenticator's root
// scope. It is the one place a staff
// permission is checked; a tenant that corresponded to a group would also
// admit that group's merchant: form here. An error never grants.
func Can(ctx context.Context, a auth.Authenticator, actor access.Actor, perm string) (bool, error) {
	if perm == "" || actor.Anonymous {
		return false, nil
	}
	if _, ok := actor.Verified.(hostVerified); ok {
		return true, nil
	}
	if scoped, ok := a.(ScopedAuthorization); ok {
		return authorize(ctx, scoped, actor.Verified, "", perm)
	}
	root, err := RootScope(ctx, a)
	if err != nil {
		return false, err
	}
	return actor.Can(ctx, root, perm)
}

// ChannelEditor checks current capabilities in a channel AuthKit group.
// Owner attribution and a missing group never grant authority. Staff are the
// caller's to admit. An error never grants.
func ChannelEditor(ctx context.Context, a auth.Authenticator, actor access.Actor, owner, scope string, perms ...string) (bool, error) {
	switch {
	case actor.Anonymous:
		return false, nil
	case scope == "":
		return false, nil
	}
	if scoped, ok := a.(ScopedAuthorization); ok {
		for _, p := range perms {
			if ok, err := authorize(ctx, scoped, actor.Verified, scope, p); err != nil || ok {
				return ok, err
			}
		}
		return false, nil
	}
	root, err := RootScope(ctx, a)
	if err != nil {
		return false, err
	}
	for _, p := range perms {
		if ok, err := actor.Can(ctx, auth.Scope{Authority: root.Authority, ID: scope}, p); err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// RootScope is where a's root permissions are held, asked per check. An
// Authenticator without auth.Root, an error or a zero scope is unresolved: an
// error matching auth.ErrUnavailable, which never grants.
func RootScope(ctx context.Context, a auth.Authenticator) (s auth.Scope, err error) {
	r, ok := a.(auth.Root)
	if !ok {
		return auth.Scope{}, errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("%T names no root scope", a)))
	}
	defer func() {
		if p := recover(); p != nil {
			s, err = auth.Scope{}, errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("%T.RootScope panicked: %v", a, p)))
		}
	}()
	switch s, err = r.RootScope(ctx); {
	case err != nil:
		return auth.Scope{}, errors.Join(auth.ErrUnavailable, err)
	case s.Authority == "" || s.ID == "":
		return auth.Scope{}, errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("%T.RootScope returned a zero scope", a)))
	}
	return s, nil
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Interface, reflect.Slice, reflect.Chan:
		return rv.IsNil()
	}
	return false
}

type actorKey struct{}

type hostKey struct{}

// HostSubject is the actor id of the engine's own in-process client.
const HostSubject = "contentkit"

// AsHost marks ctx as a request of the engine's in-process client: the gate
// admits it as the host application holding every permission, with no
// credential. Only in-process code can set it; nothing on the wire does.
func AsHost(ctx context.Context) context.Context { return context.WithValue(ctx, hostKey{}, true) }

// hostVerified is the in-process host's authority: holding the Engine is it.
type hostVerified struct{}

func (hostVerified) Identity() auth.Identity {
	return auth.Identity{Subject: HostSubject, SubjectKind: auth.SubjectApplication}
}

func (hostVerified) Can(context.Context, auth.Scope, string) (bool, error) { return true, nil }

// Host is the engine's own actor, holding every permission: what its
// in-process client acts as, and its own cascades.
func Host() access.Actor {
	return access.Actor{ID: HostSubject, Kind: auth.SubjectApplication, Verified: hostVerified{}}
}

// IsHost recognizes the engine's in-process authority by its private verified
// type, never by a caller-supplied subject name.
func IsHost(a access.Actor) bool {
	_, ok := a.Verified.(hostVerified)
	return ok
}

type domainKey struct{}

// Domain is the request host the gate bound to ctx: it picks the domain whose
// scope every read applies (kinds.Config.DomainFor). "" outside a request.
func Domain(ctx context.Context) string {
	s, _ := ctx.Value(domainKey{}).(string)
	return s
}

// Actor is the actor the gate bound to ctx; anonymous when none was.
func Actor(ctx context.Context) access.Actor {
	if a, ok := ctx.Value(actorKey{}).(access.Actor); ok {
		return a
	}
	return access.Actor{Anonymous: true}
}

// Guard is an httpapi.Guard: it binds the request's actor and admits it by
// s's tier before next serves it.
func (g *Gate) Guard(s httpapi.Spec, next http.Handler) http.Handler {
	if g.Then != nil {
		next = g.Then(next)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := anonymous(r)
		if s.Auth != httpapi.Open {
			var err error
			if a, err = g.actor(r); err != nil {
				g.refuse(w, r, s, err)
				return
			}
		}
		if (s.Auth == httpapi.User || s.Auth == httpapi.Person || s.Auth == httpapi.Staff || s.Auth == httpapi.Editor) && a.Anonymous {
			g.refuse(w, r, s, auth.ErrUnauthenticated)
			return
		}
		switch s.Auth {
		case httpapi.Person:
			if !a.Person() {
				g.refuse(w, r, s, auth.ErrForbidden)
				return
			}
		case httpapi.User:
			if !a.Member() {
				g.refuse(w, r, s, auth.ErrForbidden)
				return
			}
		case httpapi.Staff:
			ok, err := Can(r.Context(), g.Auth, a, s.Perm)
			if err == nil && !ok {
				err = auth.ErrForbidden
			}
			if err != nil {
				g.refuse(w, r, s, err)
				return
			}
		}
		ctx := context.WithValue(context.WithValue(r.Context(), actorKey{}, a), domainKey{}, r.Host)
		if a.Anonymous {
			ctx = readerprefs.WithHeader(ctx, r.Header.Values(readerprefs.Header))
		}
		if s.Auth == httpapi.Public && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			w.Header().Add("Vary", readerprefs.Header)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// anonymous is a caller without a credential, keyed by its address.
func anonymous(r *http.Request) access.Actor {
	return access.Actor{Anonymous: true, IP: clientIP(r)}
}

// clientIP is the request's peer address: the host's trusted proxy, if any,
// rewrote RemoteAddr to the client's.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// fault is a provider misbehaving or missing a capability: logged at ERROR,
// answered 503.
type fault string

func (f fault) Error() string { return string(f) }

// actor asks the Authenticator who r is: anonymous when it carries no
// credential, else the Verified's subject.
func (g *Gate) actor(r *http.Request) (access.Actor, error) {
	if host, _ := r.Context().Value(hostKey{}).(bool); host {
		return Host(), nil
	}
	v, err := g.authenticate(r)
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrUnauthenticated) && !g.presented(r):
		return anonymous(r), nil
	default:
		return access.Actor{}, err
	}
	id, err := identity(v)
	switch {
	case err != nil:
		return access.Actor{}, err
	case strings.TrimSpace(id.Subject) == "":
		return access.Actor{}, errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("%T: the Identity names no subject", v)))
	case id.SubjectKind != auth.SubjectUser && id.SubjectKind != auth.SubjectApplication:
		return access.Actor{}, errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("%T: the Identity's subject kind %q is neither a user nor an application", v, id.SubjectKind)))
	}
	a := access.Actor{ID: id.Subject, Kind: id.SubjectKind, IP: clientIP(r), Verified: &memo{v: v}}
	if scoped, ok := g.Auth.(ScopedAuthorization); ok {
		if strings.TrimSpace(id.Issuer) == "" {
			return access.Actor{}, errors.Join(auth.ErrUnavailable, fault("scoped identity names no issuer"))
		}
		a.ID, err = resolveActor(r.Context(), scoped, v)
		if err != nil {
			return access.Actor{}, err
		}
		if strings.TrimSpace(a.ID) == "" {
			return access.Actor{}, errors.Join(auth.ErrUnavailable, fault("app authorization returned an empty actor ID"))
		}
		if accounts, ok := g.Auth.(ScopedAccounts); ok {
			account, member, err := resolveTenantAccount(r.Context(), accounts, v, a.ID)
			if err != nil {
				return access.Actor{}, err
			}
			a.Account = &access.AccountMembership{}
			if member {
				if strings.TrimSpace(account) == "" {
					return access.Actor{}, errors.Join(auth.ErrUnavailable, fault("tenant membership returned an empty account ID"))
				}
				a.Account.ID = account
			}
		}
	}
	if id.Invoker != (auth.Invoker{}) && !id.SelfInvoked() {
		a.Invoker = id.Invoker
	}
	return a, nil
}

func resolveTenantAccount(ctx context.Context, a ScopedAccounts, v auth.Verified, actorID string) (id string, member bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			id, member, err = "", false, errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("app authorization ResolveTenantAccount panicked: %v", p)))
		}
	}()
	return a.ResolveTenantAccount(ctx, v, actorID)
}

func resolveActor(ctx context.Context, a ScopedAuthorization, v auth.Verified) (id string, err error) {
	defer func() {
		if p := recover(); p != nil {
			id, err = "", errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("app authorization ResolveActor panicked: %v", p)))
		}
	}()
	return a.ResolveActor(ctx, v)
}

func authorize(ctx context.Context, a ScopedAuthorization, v auth.Verified, group, permission string) (ok bool, err error) {
	var memoized *memo
	key := canKey{scope: auth.Scope{ID: group}, perm: permission}
	if m, wrapped := v.(*memo); wrapped {
		memoized = m
		m.mu.Lock()
		cached, hit := m.scoped[key]
		m.mu.Unlock()
		if hit {
			return cached, nil
		}
		v = m.v
	}
	defer func() {
		if p := recover(); p != nil {
			ok, err = false, errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("app authorization Authorize panicked: %v", p)))
		}
	}()
	ok, err = a.Authorize(ctx, v, group, permission)
	if err != nil {
		return false, err
	}
	if memoized != nil {
		memoized.mu.Lock()
		if memoized.scoped == nil {
			memoized.scoped = make(map[canKey]bool)
		}
		memoized.scoped[key] = ok
		memoized.mu.Unlock()
	}
	return ok, nil
}

// presented reports a credential in r: any header the Authenticator's
// credentials travel in (auth.Headers), else Authorization.
func (g *Gate) presented(r *http.Request) bool {
	names := []string{"Authorization"}
	if h, ok := g.Auth.(auth.Headers); ok {
		names = h.AllowedHeaders()
	}
	for _, n := range names {
		if r.Header.Get(n) != "" {
			return true
		}
	}
	return false
}

func (g *Gate) authenticate(r *http.Request) (v auth.Verified, err error) {
	defer func() {
		if p := recover(); p != nil {
			v, err = nil, errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("%T.Authenticate panicked: %v", g.Auth, p)))
		}
	}()
	v, err = g.Auth.Authenticate(r)
	if err == nil && isNil(v) {
		return nil, errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("%T.Authenticate returned neither a Verified nor an error", g.Auth)))
	}
	return v, err
}

func identity(v auth.Verified) (id auth.Identity, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("%T.Identity panicked: %v", v, p)))
		}
	}()
	return v.Identity(), nil
}

// refuse answers err as auth.Refuse says, in ContentKit's error body. Logs
// never carry credentials, nor the provider's text below an outage.
func (g *Gate) refuse(w http.ResponseWriter, r *http.Request, s httpapi.Spec, err error) {
	ref := auth.Refuse(r, err)
	for name, values := range ref.Header {
		w.Header()[name] = values
	}
	code, msg := httpapi.CodeUnavailable, "authentication is unavailable; retry"
	switch ref.Status {
	case http.StatusUnauthorized:
		code, msg = httpapi.CodeUnauthorized, "authentication required"
	case http.StatusForbidden:
		code, msg = httpapi.CodeForbidden, "forbidden"
	}
	log := g.Log
	if log == nil {
		log = slog.Default()
	}
	attrs := []any{"route", string(s.Module) + " " + s.Key(), "tier", string(s.Auth), "status", ref.Status}
	var f fault
	switch {
	case errors.As(err, &f):
		log.ErrorContext(r.Context(), "contentkit: auth provider fault", append(attrs, "fault", f.Error())...)
	case ref.Status >= http.StatusInternalServerError:
		log.WarnContext(r.Context(), "contentkit: route gate could not decide", append(attrs, "err", err.Error())...)
	default:
		log.DebugContext(r.Context(), "contentkit: route gate refused", attrs...)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(ref.Status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}{msg, code})
}

// memo is the request's Verified, remembering each Can it answered, so the
// gate and the handler ask the provider once per permission.
type memo struct {
	v      auth.Verified
	mu     sync.Mutex
	can    map[canKey]bool
	scoped map[canKey]bool
}

type canKey struct {
	scope auth.Scope
	perm  string
}

var (
	_ auth.PermissionChecker   = (*memo)(nil)
	_ auth.RecentSignInChecker = (*memo)(nil)
	_ auth.Bound               = (*memo)(nil)
	_ access.Entitled          = (*memo)(nil)
)

func (m *memo) Identity() auth.Identity { return m.v.Identity() }

// Entitlements forwards the token's entitlement claims (access.Entitled).
func (m *memo) Entitlements() []string {
	if e, ok := m.v.(access.Entitled); ok {
		return e.Entitlements()
	}
	return nil
}

func (m *memo) Can(ctx context.Context, scope auth.Scope, permission string) (ok bool, err error) {
	key := canKey{scope, permission}
	m.mu.Lock()
	ok, hit := m.can[key]
	m.mu.Unlock()
	if hit {
		return ok, nil
	}
	pc, has := m.v.(auth.PermissionChecker)
	if !has {
		return false, nil
	}
	defer func() {
		if p := recover(); p != nil {
			ok, err = false, errors.Join(auth.ErrUnavailable, fault(fmt.Sprintf("%T.Can panicked: %v", m.v, p)))
		}
	}()
	if ok, err = pc.Can(ctx, scope, permission); err != nil {
		return false, err
	}
	m.mu.Lock()
	if m.can == nil {
		m.can = map[canKey]bool{}
	}
	m.can[key] = ok
	m.mu.Unlock()
	return ok, nil
}

func (m *memo) CheckRecentSignIn(ctx context.Context) error {
	rs, ok := m.v.(auth.RecentSignInChecker)
	if !ok {
		return auth.ErrForbidden
	}
	return rs.CheckRecentSignIn(ctx)
}

func (m *memo) BoundScope() auth.Scope {
	if b, ok := m.v.(auth.Bound); ok {
		return b.BoundScope()
	}
	return auth.Scope{}
}
