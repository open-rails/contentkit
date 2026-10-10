package contract

import (
	"encoding"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/media"
)

const module = "github.com/open-rails/contentkit"

// ErrorReply is the flat error body every module answers. A code's own
// members appear only with it: retry_after with rate_limited and unavailable,
// action with an interaction's rate_limited, ban with comment_banned, blobs
// with not_uploaded, details with an upload refusal.
type ErrorReply struct {
	Error      string              `json:"error"`
	Code       string              `json:"code"`
	RetryAfter int                 `json:"retry_after,omitempty"`
	Action     content.Action      `json:"action,omitempty"`
	Ban        *content.BanNotice  `json:"ban,omitempty"`
	Blobs      []string            `json:"blobs,omitempty"`
	Details    *media.ErrorDetails `json:"details,omitempty"`
}

// enum is a string field whose values are constants of an untyped string,
// given a name on the wire.
type enum struct {
	name   string
	values []string
	fields []string // "pkg.Type.json"
}

var enums = []enum{
	{"ErrorCode", errorCodeNames(), []string{"contract.ErrorReply.code"}},
	{"OpName", []string{media.OpPut, media.OpEdit, media.OpMove, media.OpRename, media.OpRemove, media.OpAttach,
		media.OpCopy, media.OpFrame, media.OpMeta, media.OpRegenerate}, []string{"media.Op.op"}},
	{"Access", []string{media.AccessFull, media.AccessNone}, []string{"media.ReadResult.access"}},
	{"ItemState", []string{media.StateReady, media.StateProcessing, media.StateFailed}, []string{"media.ReadResult.state"}},
	{"EncodePhase", media.EncodePhases, []string{"media.EncodeProgress.phase"}},
	{"PollKind", []string{content.PollMultipleChoice, content.PollFreeText}, []string{"content.Poll.kind", "content.PollInput.kind"}},
	{"ModerationState", []string{content.ModerationApproved, content.ModerationHeld, content.ModerationRejected},
		[]string{"content.Comment.moderation", "content.Post.moderation"}},
}

func errorCodeNames() []string {
	var out []string
	for _, c := range httpapi.ErrorCodes() {
		out = append(out, c.Code)
	}
	return out
}

// kind is how a Go type appears on the wire.
type kind int

const (
	kindString kind = iota
	kindInteger
	kindNumber
	kindBoolean
	kindTime
	kindAny
	kindArray
	kindMap
	kindObject
	kindStream
)

var (
	timeType          = reflect.TypeFor[time.Time]()
	rawMessageType    = reflect.TypeFor[json.RawMessage]()
	streamType        = reflect.TypeFor[httpapi.Stream]()
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// classify is t's wire kind, pointers looked through.
func classify(t reflect.Type) (kind, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == timeType:
		return kindTime, nil
	case t == rawMessageType:
		return kindAny, nil
	case t == streamType:
		return kindStream, nil
	}
	if t.Implements(jsonMarshalerType) || reflect.PointerTo(t).Implements(jsonMarshalerType) {
		return 0, fmt.Errorf("contract: %s writes its own JSON; say what it writes", t)
	}
	if t.Implements(textMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType) {
		return kindString, nil
	}
	switch t.Kind() {
	case reflect.String:
		return kindString, nil
	case reflect.Bool:
		return kindBoolean, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return kindInteger, nil
	case reflect.Float32, reflect.Float64:
		return kindNumber, nil
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return kindString, nil // base64
		}
		return kindArray, nil
	case reflect.Map:
		return kindMap, nil
	case reflect.Interface:
		return kindAny, nil
	case reflect.Struct:
		return kindObject, nil
	}
	return 0, fmt.Errorf("contract: %s has no wire form", t)
}

// field is one JSON member of an object.
type field struct {
	name     string
	owner    reflect.Type // the struct that declares it
	t        reflect.Type
	optional bool // omitempty or omitzero: absent when empty
	nullable bool // null when unset: a pointer or interface without omitempty
}

// fieldsOf lists t's JSON members the way encoding/json writes them:
// embedded structs flattened, unexported and "-" members skipped.
func fieldsOf(t reflect.Type) []field {
	var out []field
	seen := map[string]int{}
	var walk func(t reflect.Type, depth int)
	walk = func(t reflect.Type, depth int) {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		for i := range t.NumField() {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if f.Anonymous && name == "" && ft.Kind() == reflect.Struct && ft != timeType {
				walk(ft, depth+1)
				continue
			}
			if !f.IsExported() {
				continue
			}
			if name == "" {
				name = f.Name
			}
			member := field{name: name, owner: t, t: f.Type}
			for _, opt := range strings.Split(opts, ",") {
				if opt == "omitempty" || opt == "omitzero" {
					member.optional = true
				}
			}
			// Lists and maps are written empty, never null (Conform checks it).
			switch f.Type.Kind() {
			case reflect.Pointer, reflect.Interface:
				member.nullable = !member.optional
			}
			if at, dup := seen[name]; dup {
				if depth == 0 {
					out[at] = member // the outer member shadows the embedded one
				}
				continue
			}
			seen[name] = len(out)
			out = append(out, member)
		}
	}
	walk(t, 0)
	return out
}

// object is a named struct on the wire.
type object struct {
	name   string
	t      reflect.Type
	fields []field
	input  bool // reached from a request body
	output bool // reached from a response body
}

// model is every wire type a set of routes reaches.
type model struct {
	routes  []httpapi.Spec
	objects map[string]*object
	names   map[reflect.Type]string
	enums   *enumIndex
	fields  map[string]string // "pkg.Type.json" -> enum name
}

// elem is t with pointers removed.
func elem(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func pkgName(t reflect.Type) string { return path.Base(t.PkgPath()) }

func newModel(fsys fs.FS, list []httpapi.Spec) (*model, error) {
	m := &model{routes: list, objects: map[string]*object{}, names: map[reflect.Type]string{},
		enums: &enumIndex{fsys: fsys, values: map[string][]string{}}, fields: map[string]string{}}
	for _, e := range enums {
		for _, f := range e.fields {
			m.fields[f] = e.name
		}
	}
	reached := map[reflect.Type]bool{}
	var find func(t reflect.Type) error
	find = func(t reflect.Type) error {
		t = elem(t)
		k, err := classify(t)
		if err != nil {
			return err
		}
		switch k {
		case kindArray, kindMap:
			return find(t.Elem())
		case kindObject:
			if reached[t] {
				return nil
			}
			if !token.IsExported(t.Name()) {
				return fmt.Errorf("contract: %s on the wire: bodies are exported named types", t)
			}
			reached[t] = true
			for _, f := range fieldsOf(t) {
				if err := find(f.t); err != nil {
					return fmt.Errorf("%s.%s: %w", t, f.name, err)
				}
			}
		}
		return nil
	}
	bodies := func(visit func(t reflect.Type, input bool) error) error {
		visit(reflect.TypeFor[ErrorReply](), false)
		for _, r := range list {
			if r.Request != nil {
				if err := visit(reflect.TypeOf(r.Request), true); err != nil {
					return fmt.Errorf("%s request: %w", r.Key(), err)
				}
			}
			for _, reply := range r.Responses {
				if reply.Body != nil {
					if err := visit(reflect.TypeOf(reply.Body), false); err != nil {
						return fmt.Errorf("%s response: %w", r.Key(), err)
					}
				}
			}
		}
		return nil
	}
	if err := find(reflect.TypeFor[ErrorReply]()); err != nil {
		return nil, err
	}
	for _, r := range list {
		for _, v := range append([]any{r.Request}, replyBodies(r)...) {
			if v == nil {
				continue
			}
			if k, _ := classify(reflect.TypeOf(v)); k == kindAny {
				return nil, fmt.Errorf("contract: %s answers %T: bodies are exported named types", r.Key(), v)
			}
			if err := find(reflect.TypeOf(v)); err != nil {
				return nil, fmt.Errorf("%s: %w", r.Key(), err)
			}
		}
	}
	// A bare name when one type holds it, its package's name in front when two do.
	byBase := map[string][]reflect.Type{}
	for t := range reached {
		byBase[t.Name()] = append(byBase[t.Name()], t)
	}
	for base, types := range byBase {
		for _, t := range types {
			name := base
			if len(types) > 1 {
				name = strings.ToUpper(pkgName(t)[:1]) + pkgName(t)[1:] + base
			}
			if other, dup := m.objects[name]; dup {
				return nil, fmt.Errorf("contract: %s and %s are both named %s on the wire", other.t, t, name)
			}
			m.names[t] = name
			m.objects[name] = &object{name: name, t: t, fields: fieldsOf(t)}
		}
	}
	var mark func(t reflect.Type, input bool)
	mark = func(t reflect.Type, input bool) {
		t = elem(t)
		switch k, _ := classify(t); k {
		case kindArray, kindMap:
			mark(t.Elem(), input)
		case kindObject:
			o := m.objects[m.names[t]]
			if input && o.input || !input && o.output {
				return
			}
			if input {
				o.input = true
			} else {
				o.output = true
			}
			for _, f := range o.fields {
				mark(f.t, input)
			}
		}
	}
	_ = bodies(func(t reflect.Type, input bool) error { mark(t, input); return nil })
	return m, nil
}

func replyBodies(r httpapi.Spec) []any {
	var out []any
	for _, reply := range r.Responses {
		out = append(out, reply.Body)
	}
	return out
}

func (m *model) sortedObjects() []*object {
	out := make([]*object, 0, len(m.objects))
	for _, o := range m.objects {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// enumOf is the named enum of a field, or "".
func (m *model) enumOf(f field) string {
	return m.fields[pkgName(f.owner)+"."+f.owner.Name()+"."+f.name]
}

func enumValues(name string) []string {
	for _, e := range enums {
		if e.name == name {
			return e.values
		}
	}
	return nil
}

// enumIndex finds the string constants declared with a named string type:
// the values its wire field takes.
type enumIndex struct {
	fsys   fs.FS
	values map[string][]string
}

func (e *enumIndex) of(t reflect.Type) []string {
	t = elem(t)
	if t.Kind() != reflect.String || t.Name() == "" || !strings.HasPrefix(t.PkgPath(), module) {
		return nil
	}
	key := t.PkgPath() + "." + t.Name()
	if values, ok := e.values[key]; ok {
		return values
	}
	dir := strings.TrimPrefix(strings.TrimPrefix(t.PkgPath(), module), "/")
	if dir == "" {
		dir = "."
	}
	var out []string
	entries, err := fs.ReadDir(e.fsys, dir)
	if err != nil {
		panic(fmt.Sprintf("contract: read %s: %v", dir, err))
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := fs.ReadFile(e.fsys, path.Join(dir, name))
		if err != nil {
			panic(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, body, parser.SkipObjectResolution)
		if err != nil {
			panic(fmt.Sprintf("contract: parse %s: %v", name, err))
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != t.Name() {
					continue
				}
				for _, v := range vs.Values {
					if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						value, _ := strconv.Unquote(lit.Value)
						out = append(out, value)
					}
				}
			}
		}
	}
	sort.Strings(out)
	e.values[key] = out
	return out
}

// pathParams lists a route path's {wildcards}, a trailing {name...} as name.
func pathParams(p string) []string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, strings.TrimSuffix(seg[1:len(seg)-1], "..."))
		}
	}
	return out
}
