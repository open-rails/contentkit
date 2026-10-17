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
		`content_filters: {applies_to: [gallery], domains: [{name: main, filters: [{entity: {kind: tag, slug: gore}, labels: {en: Gore}}]}]}`,
		`content_filters: {applies_to: [gallery], domains: [{name: main, filters: [{entity: {kind: tag, slug: gore}, default_visible: false}]}]}`,
		`content_filters: {applies_to: [gallery], domains: [{name: main, filters: [{entity: {kind: tag, slug: gore}, visible: false, user_togle: false}]}]}`,
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
        - {entity: {kind: tag, slug: furry}, visible: true}
        - {entity: {kind: tag, slug: restricted}, visible: false, user_toggle: false}
`
	if err := yaml.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.ContentFilters.AppliesTo, []string{"gallery", "blog"}) || len(c.ContentFilters.Domains) != 1 {
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
	}
}

func TestDecodeIsolatedContentFilterBlockStrictly(t *testing.T) {
	raw := []byte(`{"applies_to":["gallery","blog"],"domains":[{"name":"main","hosts":["example.test"],"require_entities":{"gallery":[{"kind":"tag","slug":"yaoi"}]},"filters":[{"entity":{"kind":"tag","slug":"furry"},"visible":true}]}]}`)
	policy, err := DecodeContentFilters(raw)
	if err != nil || len(policy.Domains) != 1 || len(policy.AppliesTo) != 2 {
		t.Fatalf("isolated JSONblock: %+v %v", policy, err)
	}
	for _, bad := range []string{`domains: [{name: main, gallery: {filters: []}}]`, `{"applies_to":["gallery"],"domains":[{"name":"main","gallery":{"filters":[]}}]}`, `applies_to: [gallery]
domains: [{name: main, filters: [{entity: {kind: tag, slug: furry}, user_togle: false}]}]`} {
		if _, err := DecodeContentFilters([]byte(bad)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe isolated block accepted %s: %v", bad, err)
		}
	}
}
