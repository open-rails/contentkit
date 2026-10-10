package contract

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/open-rails/contentkit/internal/httpapi"
)

// obj is a JSON object whose keys keep their insertion order, so the
// generated document is stable and reads top-down.
type obj struct {
	keys []string
	vals map[string]any
}

func newObj(kv ...any) *obj {
	o := &obj{vals: map[string]any{}}
	for i := 0; i+1 < len(kv); i += 2 {
		o.set(kv[i].(string), kv[i+1])
	}
	return o
}

func (o *obj) set(k string, v any) *obj {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
	return o
}

func (o *obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(k)
		b.Write(key)
		b.WriteByte(':')
		val, err := marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func marshalIndent(v any) ([]byte, error) {
	raw, err := marshal(v)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		return nil, err
	}
	return append(b.Bytes(), '\n'), nil
}

// schema is t's JSON Schema.
func (m *model) schema(t reflect.Type) *obj {
	t = elem(t)
	k, _ := classify(t)
	switch k {
	case kindString:
		if values := m.enums.of(t); values != nil {
			return newObj("type", "string", "enum", values)
		}
		return newObj("type", "string")
	case kindInteger:
		return newObj("type", "integer")
	case kindNumber:
		return newObj("type", "number")
	case kindBoolean:
		return newObj("type", "boolean")
	case kindTime:
		return newObj("type", "string", "format", "date-time")
	case kindAny:
		return newObj()
	case kindArray:
		return newObj("type", "array", "items", m.schema(t.Elem()))
	case kindMap:
		return newObj("type", "object", "additionalProperties", m.schema(t.Elem()))
	}
	return newObj("$ref", "#/components/schemas/"+m.names[t])
}

func (m *model) memberSchema(f field) *obj {
	s := m.schema(f.t)
	if name := m.enumOf(f); name != "" {
		s = newObj("$ref", "#/components/schemas/"+name)
	}
	if f.nullable {
		return newObj("oneOf", []any{s, newObj("type", "null")})
	}
	return s
}

func (m *model) objectSchema(o *object) *obj {
	props := newObj()
	var required []string
	for _, f := range o.fields {
		props.set(f.name, m.memberSchema(f))
		if o.output && !f.optional {
			required = append(required, f.name)
		}
	}
	s := newObj("type", "object", "properties", props)
	if len(required) > 0 {
		s.set("required", required)
	}
	return s
}

func (m *model) content(bodies []any) *obj {
	c := newObj()
	var alternatives []any
	for _, body := range bodies {
		if stream, ok := body.(httpapi.Stream); ok {
			c.set(stream.ContentType, newObj("schema", newObj("type", "string")))
			continue
		}
		alternatives = append(alternatives, m.schema(reflect.TypeOf(body)))
	}
	switch len(alternatives) {
	case 0:
	case 1:
		c.set("application/json", newObj("schema", alternatives[0]))
	default:
		c.set("application/json", newObj("schema", newObj("oneOf", alternatives)))
	}
	return c
}

var paramSchemas = map[string]*obj{
	"string":  newObj("type", "string"),
	"integer": newObj("type", "integer"),
	"number":  newObj("type", "number"),
	"boolean": newObj("type", "boolean"),
}

func (m *model) operation(r httpapi.Spec) *obj {
	op := newObj("tags", []string{r.Resource}, "summary", r.Doc)
	var params []any
	for _, p := range pathParams(r.Path) {
		param := newObj("name", p, "in", "path", "required", true, "schema", newObj("type", "string"))
		if strings.Contains(r.Path, "{"+p+"...}") {
			param.set("description", "the rest of the path; may contain /")
			param.set("allowReserved", true)
		}
		params = append(params, param)
	}
	for _, q := range r.Query {
		param := newObj("name", q.Name, "in", "query")
		if q.Doc != "" {
			param.set("description", q.Doc)
		}
		switch q.Kind {
		case "flag":
			param.set("allowEmptyValue", true)
			param.set("schema", newObj("type", "boolean"))
		case "strings":
			param.set("style", "form")
			param.set("explode", true)
			param.set("schema", newObj("type", "array", "items", newObj("type", "string")))
		case "list":
			param.set("style", "form")
			param.set("explode", false)
			param.set("schema", newObj("type", "array", "items", newObj("type", "string")))
		default:
			param.set("schema", paramSchemas[q.Kind])
		}
		params = append(params, param)
	}
	if len(params) > 0 {
		op.set("parameters", params)
	}
	if r.Request != nil {
		op.set("requestBody", newObj("required", true, "content", m.content([]any{r.Request})))
	}
	responses := newObj()
	byStatus := map[int][]any{}
	var order []int
	for _, reply := range r.Responses {
		if _, seen := byStatus[reply.Status]; !seen {
			order = append(order, reply.Status)
			byStatus[reply.Status] = nil
		}
		if reply.Body != nil {
			byStatus[reply.Status] = append(byStatus[reply.Status], reply.Body)
		}
	}
	for _, status := range order {
		resp := newObj("description", http.StatusText(status))
		if len(byStatus[status]) > 0 {
			resp.set("content", m.content(byStatus[status]))
		}
		responses.set(strconv.Itoa(status), resp)
	}
	responses.set("default", newObj("$ref", "#/components/responses/Error"))
	op.set("responses", responses)
	if r.Auth == httpapi.Public {
		op.set("security", []any{newObj(), newObj("host", []string{})})
	} else {
		op.set("security", []any{newObj("host", []string{})})
	}
	op.set("x-contentkit-module", string(r.Module))
	op.set("x-contentkit-auth", string(r.Auth))
	if r.Perm != "" {
		op.set("x-contentkit-permission", r.Perm)
	}
	op.set("x-contentkit-errors", nonNil(r.Errors))
	op.set("x-contentkit-error-sets", r.ErrorSets())
	return op
}

func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

func (m *model) openAPI() ([]byte, error) {
	schemas := newObj()
	for _, o := range m.sortedObjects() {
		schemas.set(o.name, m.objectSchema(o))
	}
	for _, e := range enums {
		schemas.set(e.name, newObj("type", "string", "enum", e.values))
	}
	paths := newObj()
	sets, seen := newObj(), map[string]bool{}
	for _, r := range m.routes {
		p := strings.ReplaceAll(r.FullPath(), "...}", "}")
		if p == "" {
			p = "/"
		}
		item, _ := paths.vals[p].(*obj)
		if item == nil {
			item = newObj()
			paths.set(p, item)
		}
		item.set(strings.ToLower(r.Method), m.operation(r))
		for _, name := range r.ErrorSets() {
			if !seen[name] {
				seen[name] = true
				sets.set(name, httpapi.ErrorSet(name))
			}
		}
	}
	codes := newObj()
	for _, c := range httpapi.ErrorCodes() {
		codes.set(c.Code, newObj("status", c.Status, "meaning", c.Meaning))
	}
	modules := newObj()
	for _, mod := range httpapi.Modules {
		modules.set(string(mod), mod.Prefix())
	}
	doc := newObj(
		"openapi", "3.1.0",
		"info", newObj(
			"title", "ContentKit",
			"version", "v0",
			"description", "ContentKit's HTTP API, from the prefix the host mounts contentkit.Runtime.Handler at. "+
				"Each module sits at its own sub-path (x-contentkit-modules); a host that mounts a module alone serves its routes beneath that mount instead. "+
				"Generated from the route catalog (internal/httpapi) by `go generate ./internal/contract`; do not edit.",
		),
		"paths", paths,
		"components", newObj(
			"schemas", schemas,
			"responses", newObj("Error", newObj("description", "A refusal: a code of the operation's x-contentkit-errors or of an x-contentkit-error-sets set it names.",
				"content", newObj("application/json", newObj("schema", newObj("$ref", "#/components/schemas/ErrorReply"))))),
			"securitySchemes", newObj("host", newObj("type", "http", "scheme", "bearer",
				"description", "Whatever the host's auth middleware accepts: ContentKit reads the actor it puts in the request context.")),
		),
		"x-contentkit-modules", modules,
		"x-contentkit-error-sets", sets,
		"x-contentkit-error-codes", codes,
	)
	return marshalIndent(doc)
}
