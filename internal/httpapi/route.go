// Package httpapi is ContentKit's HTTP surface as data. Every route of every
// module is declared once, as a Route in the module's table: method, path,
// auth tier, query, bodies and error codes. The module mounts its handlers
// from that table (Mount), and internal/contract renders the same tables as
// api/openapi.json, the browser SDK's generated types and docs/api/routes.md.
package httpapi

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
)

const (
	GET, POST, PUT, PATCH, DELETE = http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete
)

// Module is one mountable handler. contentkit.Runtime.Handler serves every
// configured module under one prefix, each at its fixed sub-path (Prefix).
type Module string

const (
	// Content is posts, comments, reactions, favorites, polls, comment bans
	// and moderation (content.Runtime.Handler).
	Content Module = "content"
	// Upload is the media upload API (media.UploadHandler).
	Upload Module = "upload"
	// Media is the media read API and playlists (media.Reader.Handler).
	Media Module = "media"
	// Codes resolves content codes (contenturl.Router.Handler).
	Codes Module = "codes"
	// Taxonomy is the taxonomy admin API (taxonomy.Handler).
	Taxonomy Module = "taxonomy"
)

// Modules lists every module in catalog order.
var Modules = []Module{Content, Upload, Media, Codes, Taxonomy}

// Prefix is the module's sub-path under the one mount: content at its root,
// the others beneath their own first segment, which no content kind may take.
func (m Module) Prefix() string {
	switch m {
	case Upload:
		return "/media/upload"
	case Media:
		return "/media"
	case Codes:
		return "/codes"
	case Taxonomy:
		return "/taxonomy"
	}
	return ""
}

// Reserved is the first path segments the non-content modules take: a content
// kind with one of these names would be unreachable under the one mount.
func Reserved() []string { return []string{"media", "codes", "taxonomy"} }

// Tier is what a route requires of its caller before its handler acts.
type Tier string

const (
	// Public: the actor is optional; the host's resolver decides what it sees.
	Public Tier = "public"
	// User: a signed-in actor; 401 unauthorized without one.
	User Tier = "user"
	// Staff: the host permission named by Perm; 403 forbidden without it.
	Staff Tier = "staff"
)

// Reply is one success outcome: its status and a zero value of its body's
// type (nil for none).
type Reply struct {
	Status int
	Body   any
}

func OK(body any) Reply       { return Reply{Status: http.StatusOK, Body: body} }
func Created(body any) Reply  { return Reply{Status: http.StatusCreated, Body: body} }
func Accepted(body any) Reply { return Reply{Status: http.StatusAccepted, Body: body} }

// NoContent is a 204.
var NoContent = Reply{Status: http.StatusNoContent}

// Stream is a body that is not JSON: an image, a playlist.
type Stream struct{ ContentType string }

// Param is one query parameter.
type Param struct {
	Name string
	// Kind is string, integer, number, boolean, flag (present means on),
	// strings (repeated) or list (comma-separated).
	Kind string
	Doc  string
}

func Text(name, doc string) Param     { return Param{Name: name, Kind: "string", Doc: doc} }
func Int(name, doc string) Param      { return Param{Name: name, Kind: "integer", Doc: doc} }
func Number(name, doc string) Param   { return Param{Name: name, Kind: "number", Doc: doc} }
func Bool(name, doc string) Param     { return Param{Name: name, Kind: "boolean", Doc: doc} }
func Flag(name, doc string) Param     { return Param{Name: name, Kind: "flag", Doc: doc} }
func Repeated(name, doc string) Param { return Param{Name: name, Kind: "strings", Doc: doc} }
func List(name, doc string) Param     { return Param{Name: name, Kind: "list", Doc: doc} }

// Page is the limit/offset query of a list.
var Page = []Param{Int("limit", "page size: 20 by default, at most 100"), Int("offset", "items to skip")}

// Spec is a route without its handler: what the contract is generated from.
type Spec struct {
	Method string
	// Path is the route from its module's mount, in ServeMux syntax.
	Path string
	// Resource is the route's section in the docs and its OpenAPI tag.
	Resource string
	// Doc says what the route does, in one line.
	Doc  string
	Auth Tier
	// Perm names the content.Perms field a Staff route checks.
	Perm string
	// Query lists the query parameters; Request is a zero value of the JSON
	// body's type (nil for none); Responses is every success outcome.
	Query     []Param
	Request   any
	Responses []Reply
	// Errors lists the route's own codes; ErrorSets names the shared ones.
	Errors []string
	// Module is set by Register.
	Module Module
}

// Key is the route's identity within its module: "GET /posts/{id}".
func (s Spec) Key() string { return s.Method + " " + s.Path }

// FullPath is the route's path under the one mount.
func (s Spec) FullPath() string { return s.Module.Prefix() + s.Path }

// Route is a Spec with the handler that serves it, built from the state S
// the module mounts with.
type Route[S any] struct {
	Spec
	Serve func(S) http.HandlerFunc
}

// H adapts a handler method expression, such as (*posts).handleList, to Serve.
func H[S any](fn func(S, http.ResponseWriter, *http.Request)) func(S) http.HandlerFunc {
	return func(s S) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { fn(s, w, r) }
	}
}

// Mount registers routes on mux, served from s.
func Mount[S any](mux *http.ServeMux, s S, routes []Route[S]) {
	for _, r := range routes {
		mux.HandleFunc(r.Key(), r.Serve(s))
	}
}

var registered = map[Module][]Spec{}

// Register adds a module's routes to the catalog. Modules call it from init,
// once per table, in the order their routes are documented.
func Register[S any](m Module, routes []Route[S]) {
	for _, r := range routes {
		s := r.Spec
		s.Module = m
		if err := s.check(); err != nil {
			panic("httpapi: " + string(m) + " " + s.Key() + ": " + err.Error())
		}
		if r.Serve == nil {
			panic("httpapi: " + string(m) + " " + s.Key() + ": no Serve")
		}
		for _, o := range registered[m] {
			if o.Key() == s.Key() {
				panic("httpapi: " + string(m) + " " + s.Key() + ": declared twice")
			}
		}
		registered[m] = append(registered[m], s)
	}
}

func (s Spec) check() error {
	switch {
	case !strings.HasPrefix(s.Path, "/"):
		return fmt.Errorf("path must start with /")
	case s.Resource == "" || s.Doc == "":
		return fmt.Errorf("needs a Resource and a Doc")
	case len(s.Responses) == 0:
		return fmt.Errorf("needs a response")
	case (s.Auth == Staff) != (s.Perm != ""):
		return fmt.Errorf("a Staff route, and only one, names a Perm")
	case s.Auth != Public && s.Auth != User && s.Auth != Staff:
		return fmt.Errorf("unknown tier %q", s.Auth)
	}
	for _, code := range s.Errors {
		if _, ok := LookupErrorCode(code); !ok {
			return fmt.Errorf("error code %q is not registered", code)
		}
	}
	return nil
}

// Catalog is every registered route, by module (Modules order), each module
// in declaration order.
func Catalog() []Spec {
	var out []Spec
	for _, m := range Modules {
		out = append(out, registered[m]...)
	}
	return out
}

// Match finds the catalog route serving a method and a path under the one
// mount, preferring literal segments as ServeMux does.
func Match(method, path string) (Spec, bool) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	var best Spec
	bestScore, found := -1, false
	for _, s := range Catalog() {
		if s.Method != method && !(method == http.MethodHead && s.Method == GET) {
			continue
		}
		score, ok := matchPath(strings.Split(strings.Trim(s.FullPath(), "/"), "/"), segs)
		if ok && score > bestScore {
			best, bestScore, found = s, score, true
		}
	}
	return best, found
}

// matchPath matches pattern segments against path segments; the score ranks
// literal segments earlier in the path above later ones.
func matchPath(pattern, path []string) (int, bool) {
	score := 0
	for i, p := range pattern {
		if strings.HasSuffix(p, "...}") {
			return score, i < len(path)
		}
		if i >= len(path) {
			return 0, false
		}
		if strings.HasPrefix(p, "{") {
			continue
		}
		if p != path[i] {
			return 0, false
		}
		score += 1 << (16 - i)
	}
	return score, len(pattern) == len(path)
}

// ErrorSets names the shared code sets a route answers besides its own Errors.
func (s Spec) ErrorSets() []string {
	sets := []string{"module:" + string(s.Module)}
	if s.Auth != Public {
		sets = append(sets, "tier:"+string(s.Auth))
	}
	if s.Request != nil || len(s.Query) > 0 || strings.Contains(s.Path, "{") {
		sets = append(sets, "request")
	}
	return sets
}

// ErrorSet lists the codes of a set ErrorSets names.
func ErrorSet(name string) []string {
	switch name {
	case "request":
		return []string{CodeInvalidRequest}
	case "tier:user":
		return []string{CodeUnauthorized}
	case "tier:staff":
		return []string{CodeForbidden}
	case "module:content":
		return []string{CodeInternal, CodeTenantMismatch}
	case "module:upload":
		return []string{CodeInternal, CodeUnavailable, CodeUpgrade}
	case "module:media":
		return []string{CodeInternal, CodeNotFound, CodeRateLimited, CodeUnavailable, CodeUpgrade}
	case "module:codes", "module:taxonomy":
		return []string{CodeInternal}
	}
	return nil
}

// AllErrors is every code the route can answer, sorted.
func (s Spec) AllErrors() []string {
	out := slices.Clone(s.Errors)
	for _, set := range s.ErrorSets() {
		out = append(out, ErrorSet(set)...)
	}
	sort.Strings(out)
	return slices.Compact(out)
}
