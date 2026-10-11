// Package gate builds ContentKit's route gates from the host's auth. It asks
// the auth.Authenticator who a request is, at most once, builds the
// access.Actor from the Verified, enforces the route's tier (httpapi.Tier)
// and answers every refusal with the status and challenge helpers/auth's
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

	"github.com/open-rails/helpers/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/httpapi"
)

// Gate guards one module's routes.
type Gate struct {
	Auth auth.Authenticator
	// Then wraps each route inside the gate, after the actor is bound.
	Then func(http.Handler) http.Handler
	Log  *slog.Logger
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
	root, err := RootScope(ctx, a)
	if err != nil {
		return false, err
	}
	return actor.Can(ctx, root, perm)
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
		if (s.Auth == httpapi.User || s.Auth == httpapi.Staff) && a.Anonymous {
			g.refuse(w, r, s, auth.ErrUnauthenticated)
			return
		}
		switch s.Auth {
		case httpapi.User:
			if !a.Person() {
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
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, a)))
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
	if id.Invoker != (auth.Invoker{}) && !id.SelfInvoked() {
		a.Invoker = id.Invoker
	}
	return a, nil
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
	v   auth.Verified
	mu  sync.Mutex
	can map[canKey]bool
}

type canKey struct {
	scope auth.Scope
	perm  string
}

var (
	_ auth.PermissionChecker   = (*memo)(nil)
	_ auth.RecentSignInChecker = (*memo)(nil)
	_ auth.Bound               = (*memo)(nil)
)

func (m *memo) Identity() auth.Identity { return m.v.Identity() }

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
