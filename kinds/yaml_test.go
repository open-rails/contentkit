package kinds

import (
	"encoding/json"
	"errors"
	"github.com/goccy/go-yaml"
	"reflect"
	"testing"
)

func TestConfigYAMLRejectsRetiredPolicyFields(t *testing.T) {
	for _, raw := range []string{
		`sites: [{name: main, hosts: [localhost]}]`,
		`domains: [{name: main, gallery: {filters: []}}]`,
		`content_filters: [{entity: {kind: tag, slug: gore}}]`,
		`content_filters: {applies_to: [gallery], domains: [{name: main, gallery: {filters: []}}]}`,
		`content_filters: {applies_to: [gallery], domains: [{name: main, kinds: {gallery: {filters: []}}}]}`,
		`content_filters: {applies_to: [gallery], domains: [{name: main, require_entities: [{kind: tag, slug: yaoi}]}]}`,
		`content_filters: {applies_to: [gallery], domains: [{name: main, filters: [{kind: tag, slug: gore, labels: {en: Gore}}]}]}`,
		`content_filters: {applies_to: [gallery], domains: [{name: main, filters: [{kind: tag, slug: gore, default_visible: false}]}]}`,
		`content_filters: {applies_to: [gallery], domains: [{name: main, filters: [{kind: tag, slug: gore, visible: false, user_togle: false}]}]}`,
	} {
		var c Config
		if err := yaml.Unmarshal([]byte(raw), &c); !errors.Is(err, ErrInvalid) {
			t.Fatalf("retired or misspelled policy accepted %s: %v", raw, err)
		}
	}
}

func TestConfigYAMLDomainPolicy(t *testing.T) {
	var c Config
	raw := `languages: [en]
content_filters:
  applies_to: [gallery, blog]
  domains:
    - name: gay
      hosts: [doujins.gay]
      require_entities:
        gallery: [{kind: tag, slug: yaoi}]
      filters:
        - {kind: tag, slug: furry, visible: true}
        - {kind: tag, slug: restricted, visible: false, user_toggle: false}
    - name: main
`
	if err := yaml.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.ContentFilters.AppliesTo, []string{"gallery", "blog"}) || len(c.ContentFilters.Domains) != 2 {
		t.Fatalf("policy %+v", c)
	}
	d := c.ContentFilters.Domains[0]
	if d.Filters[1].Toggleable() || !d.Requires(GroupRef{Kind: "tag", Slug: "yaoi"}) || len(d.RequireEntities["blog"]) != 0 {
		t.Fatalf("flat domain %+v", d)
	}
	encoded, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var next Config
	if err := yaml.Unmarshal(encoded, &next); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.ContentFilters, next.ContentFilters) || !reflect.DeepEqual(c.Languages, next.Languages) {
		t.Fatalf("roundtrip changed %s", encoded)
	}
}

func TestDomainYAMLAndJSONShapes(t *testing.T) {
	d := doujins().ContentFilters.Domains[0]
	encoded, err := yaml.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var fromYAML map[string]any
	if err := yaml.Unmarshal(encoded, &fromYAML); err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON map[string]any
	if err := json.Unmarshal(encoded, &fromJSON); err != nil {
		t.Fatal(err)
	}
	for _, data := range []map[string]any{fromYAML, fromJSON} {
		if data["kinds"] != nil || data["gallery"] != nil || data["filters"] == nil {
			t.Fatalf("domain must have one flat filter list: %+v", data)
		}
		filters, ok := data["filters"].([]any)
		if !ok || len(filters) == 0 {
			t.Fatalf("missing filter rows: %+v", data)
		}
		row, ok := filters[0].(map[string]any)
		if !ok || row["entity"] != nil || row["kind"] != "tag" || row["slug"] != "big-breasts" {
			t.Fatalf("filter reference must be flat: %+v", filters[0])
		}
	}
}

func TestDecodeIsolatedContentFilterBlockStrictly(t *testing.T) {
	raw := []byte(`{"applies_to":["gallery","blog"],"domains":[{"name":"main","hosts":["example.test"],"require_entities":{"gallery":[{"kind":"tag","slug":"yaoi"}]},"filters":[{"kind":"tag","slug":"furry","visible":true}]}]}`)
	policy, err := DecodeContentFilters(raw)
	if err != nil || len(policy.Domains) != 1 || len(policy.AppliesTo) != 2 {
		t.Fatalf("isolated JSONblock: %+v %v", policy, err)
	}
	for _, bad := range []string{`domains: [{name: main, gallery: {filters: []}}]`, `{"applies_to":["gallery"],"domains":[{"name":"main","gallery":{"filters":[]}}]}`, `applies_to: [gallery]
domains: [{name: main, filters: [{kind: tag, slug: furry, user_togle: false}]}]`} {
		if _, err := DecodeContentFilters([]byte(bad)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe isolated block accepted %s: %v", bad, err)
		}
	}
}

func TestFlatContentFilterEntryCodecsPreserveTypedReferenceAndDefaults(t *testing.T) {
	for _, format := range []struct {
		name   string
		decode func([]byte, any) error
		encode func(any) ([]byte, error)
	}{
		{"YAML", yaml.Unmarshal, yaml.Marshal}, {"JSON", json.Unmarshal, json.Marshal},
	} {
		t.Run(format.name, func(t *testing.T) {
			for _, raw := range []string{
				`{"kind":"tag","slug":"furry"}`,
				`{"kind":"tag","slug":"furry","visible":true}`,
				`{"kind":"tag","slug":"restricted","visible":false,"user_toggle":false}`,
				`{"kind":"tag","slug":"furry","visible":true,"user_toggle":true}`,
			} {
				var filter ContentFilter
				if err := format.decode([]byte(raw), &filter); err != nil {
					t.Fatal(err)
				}
				if filter.Group.Kind != "tag" || filter.Group.Slug == "" {
					t.Fatalf("typed reference lost: %+v", filter)
				}
				if filter.Group.Slug == "restricted" && filter.Toggleable() {
					t.Fatal("fixed exclusion became toggleable")
				}
				if filter.Group.Slug == "furry" && !filter.Toggleable() {
					t.Fatal("omitted user_toggle must allow overrides")
				}
				encoded, err := format.encode(filter)
				if err != nil {
					t.Fatal(err)
				}
				var next ContentFilter
				if err := format.decode(encoded, &next); err != nil || !reflect.DeepEqual(filter, next) {
					t.Fatalf("roundtrip: %s %+v %v", encoded, next, err)
				}
			}
			var omitted ContentFilter
			if err := format.decode([]byte(`{"kind":"tag","slug":"furry"}`), &omitted); err != nil || omitted.Visible || omitted.UserToggle != nil {
				t.Fatalf("defaults changed: %+v %v", omitted, err)
			}
			for _, raw := range []string{
				`{"entity":{"kind":"tag","slug":"furry"},"visible":true}`,
				`{"kind":"tag","slug":"furry","visibile":true}`,
				`{"kind":"tag","slug":"furry","user_togle":false}`,
				`{"kind":"tag","slug":"furry","labels":{"en":"Furry"}}`,
			} {
				if err := format.decode([]byte(raw), new(ContentFilter)); !errors.Is(err, ErrInvalid) {
					t.Fatalf("unsafe entry accepted: %s %v", raw, err)
				}
			}
		})
	}
}

func TestJSONPolicyRejectsUnknownKeysAndPreservesDestinationOnFailure(t *testing.T) {
	for _, raw := range []string{
		`{"applies_to":["gallery"],"domainz":[]}`,
		`{"applies_to":["gallery"],"domains":[{"name":"main","hostz":["example.test"]}]}`,
		`{"applies_to":["gallery"],"domains":[{"name":"main","filters":[{"entity":{"kind":"tag","slug":"furry"},"visible":true}]}]}`,
	} {
		original := ContentFilters{AppliesTo: []string{"gallery"}, Domains: []Domain{{Name: "kept", Hosts: []string{"kept.test"}}}}
		destination := original.Clone()
		if err := json.Unmarshal([]byte(raw), &destination); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe JSON policy accepted %s: %v", raw, err)
		}
		if !reflect.DeepEqual(destination, original) {
			t.Fatalf("failed decode replaced live policy: %+v", destination)
		}
		if err := json.Unmarshal([]byte(`{"content_filters":`+raw+`}`), new(Config)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe JSON model accepted %s: %v", raw, err)
		}
	}
}

func TestDecodedDomainIdentityRequiresMainAndUnambiguousHosts(t *testing.T) {
	for _, decode := range []func([]byte, any) error{yaml.Unmarshal, json.Unmarshal} {
		for _, raw := range []string{
			`{"domains":[{"name":"gay","hosts":["gay.test"]}]}`,
			`{"domains":[{"name":"main"},{"name":"main"}]}`,
			`{"domains":[{"name":"main","hosts":["example.test"]},{"name":"other","hosts":[" EXAMPLE.TEST.:443 "]}]}`,
			`{"domains":[{"name":"other"},{"name":"main"}]}`,
		} {
			if err := decode([]byte(raw), new(ContentFilters)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid fallback policy accepted: %s %v", raw, err)
			}
		}
		for _, raw := range []string{`{"domains":[]}`, `{"domains":[{"name":"gay","hosts":["gay.test"]},{"name":"main"}]}`} {
			if err := decode([]byte(raw), new(ContentFilters)); err != nil {
				t.Fatalf("valid unordered fallback policy rejected: %s %v", raw, err)
			}
		}
	}
}
