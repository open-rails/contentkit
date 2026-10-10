package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-rails/contentkit/internal/httpapi"
)

// Checker holds real responses to the catalog: tests wrap a handler with it,
// so every answer of a real request is checked against what its route
// declares.
type Checker struct {
	m *model

	mu     sync.Mutex
	served map[string]bool
}

// NewChecker builds a checker over the catalog; fsys is the repository.
func NewChecker(fsys fs.FS) (*Checker, error) {
	m, err := newModel(fsys, httpapi.Catalog())
	if err != nil {
		return nil, err
	}
	return &Checker{m: m, served: map[string]bool{}}, nil
}

// Wrap serves h and reports each response that breaks its route's contract.
// path is the request path under the one mount; strip removes the host's
// mount prefix from it.
func (c *Checker) Wrap(strip string, h http.Handler, report func(error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{header: http.Header{}, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		path := strings.TrimPrefix(r.URL.Path, strip)
		if err := c.Check(r.Method, path, rec.status, rec.header, rec.body.Bytes()); err != nil {
			report(fmt.Errorf("%s %s -> %d: %w\n%s", r.Method, r.URL.Path, rec.status, err, rec.body.String()))
		}
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.status)
		_, _ = w.Write(rec.body.Bytes())
	})
}

// Served lists the catalog routes a checked response came from, as
// "module METHOD path".
func (c *Checker) Served() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.served)
}

// Check holds one response to its route's contract: a declared status with
// its body, or an error body whose code the route may answer with that code's
// status.
func (c *Checker) Check(method, path string, status int, header http.Header, body []byte) error {
	spec, ok := httpapi.Match(method, path)
	if !ok {
		return fmt.Errorf("no catalog route")
	}
	c.mu.Lock()
	c.served[RouteID(spec)] = true
	c.mu.Unlock()
	var bodies []any
	declared := false
	for _, reply := range spec.Responses {
		if reply.Status == status {
			declared = true
			if reply.Body != nil {
				bodies = append(bodies, reply.Body)
			}
		}
	}
	if declared {
		if len(bodies) == 0 {
			if len(bytes.TrimSpace(body)) > 0 {
				return fmt.Errorf("declares no body")
			}
			return nil
		}
		var errs []string
		for _, b := range bodies {
			err := c.body(b, header, body)
			if err == nil {
				return nil
			}
			errs = append(errs, err.Error())
		}
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	if status < 400 {
		return fmt.Errorf("undeclared status")
	}
	var reply struct {
		Code string `json:"code"`
	}
	if err := c.validateJSON(reflect.TypeFor[ErrorReply](), body); err != nil {
		return fmt.Errorf("error body: %w", err)
	}
	_ = json.Unmarshal(body, &reply)
	code, ok := httpapi.LookupErrorCode(reply.Code)
	switch {
	case !ok:
		return fmt.Errorf("unregistered code %q", reply.Code)
	case code.Status != status:
		return fmt.Errorf("code %q is %d, answered %d", reply.Code, code.Status, status)
	case !slices.Contains(spec.AllErrors(), reply.Code):
		return fmt.Errorf("route does not declare code %q", reply.Code)
	}
	return nil
}

// RouteID names a route across modules: "module METHOD path".
func RouteID(s httpapi.Spec) string { return string(s.Module) + " " + s.Key() }

func (c *Checker) body(declared any, header http.Header, body []byte) error {
	if stream, ok := declared.(httpapi.Stream); ok {
		got := header.Get("Content-Type")
		if got != stream.ContentType {
			return fmt.Errorf("content type %q, declared %q", got, stream.ContentType)
		}
		return nil
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return fmt.Errorf("content type %q, declared JSON", ct)
	}
	return c.validateJSON(reflect.TypeOf(declared), body)
}

func (c *Checker) validateJSON(t reflect.Type, body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	return c.validate(v, t, "$")
}

// validate holds a decoded JSON value to t's wire form: every member it
// declares present unless omitted empty, null only where Go writes null, no
// member it does not declare, enum values among their constants.
func (c *Checker) validate(v any, t reflect.Type, at string) error {
	t = elem(t)
	k, err := classify(t)
	if err != nil {
		return err
	}
	if v == nil {
		return fmt.Errorf("%s: null", at)
	}
	switch k {
	case kindString:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s: %T, want a string", at, v)
		}
		if values := c.m.enums.of(t); values != nil && !slices.Contains(values, s) {
			return fmt.Errorf("%s: %q is not one of %v", at, s, values)
		}
	case kindTime:
		s, ok := v.(string)
		if _, err := time.Parse(time.RFC3339Nano, s); !ok || err != nil {
			return fmt.Errorf("%s: %v, want an RFC 3339 time", at, v)
		}
	case kindInteger, kindNumber:
		n, ok := v.(json.Number)
		if !ok {
			return fmt.Errorf("%s: %T, want a number", at, v)
		}
		f, err := n.Float64()
		if err != nil || k == kindInteger && f != math.Trunc(f) {
			return fmt.Errorf("%s: %s, want an integer", at, n)
		}
	case kindBoolean:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: %T, want a boolean", at, v)
		}
	case kindArray:
		list, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: %T, want an array", at, v)
		}
		for i, item := range list {
			if err := c.validate(item, t.Elem(), fmt.Sprintf("%s[%d]", at, i)); err != nil {
				return err
			}
		}
	case kindMap:
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: %T, want an object", at, v)
		}
		for key, item := range m {
			if err := c.validate(item, t.Elem(), at+"."+key); err != nil {
				return err
			}
		}
	case kindObject:
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: %T, want an object", at, v)
		}
		fields := fieldsOf(t)
		known := map[string]bool{}
		for _, f := range fields {
			known[f.name] = true
			item, present := m[f.name]
			switch {
			case !present && !f.optional:
				return fmt.Errorf("%s.%s: missing", at, f.name)
			case !present:
				continue
			case item == nil && f.nullable:
				continue
			}
			if name := c.m.enumOf(f); name != "" {
				if s, ok := item.(string); !ok || !slices.Contains(enumValues(name), s) {
					return fmt.Errorf("%s.%s: %v is not a %s", at, f.name, item, name)
				}
				continue
			}
			if err := c.validate(item, f.t, at+"."+f.name); err != nil {
				return err
			}
		}
		for key := range m {
			if !known[key] {
				return fmt.Errorf("%s.%s: not declared", at, key)
			}
		}
	}
	return nil
}

type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if !r.wrote {
		r.status, r.wrote = status, true
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.body.Write(b)
}
