package kinds

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestConfigYAMLRejectsRetiredPolicyFields(t *testing.T) {
	for _, raw := range []string{
		"sites: [{name: main, hosts: [localhost]}]",
		"content_filters: [{entity: {kind: tag, slug: gore}}]",
		"domains: [{name: main, content_filter_defaults: []}]",
		"domains: [{name: main, require_groups: []}]",
		"domains: [{name: main, filters: [{entity: {kind: tag, slug: gore}, default_visible: false}]}]",
		"domains: [{name: main, filters: [{entity: {kind: tag, slug: gore}, visible: false, labels: {en: Gore}}]}]",
		"domains: [{name: main, kinds: {gallery: {filters: []}}}]",
		"domains: [{name: main, gallery: {filter: []}}]",
		"domains: [{name: main, gallery: {filters: [{entity: {kind: tag, slug: gore}, visible: false, user_togle: false}]}}]",
	} {
		var c Config
		if err := yaml.Unmarshal([]byte(raw), &c); !errors.Is(err, ErrInvalid) {
			t.Fatalf("retired policy accepted: %s: %v", raw, err)
		}
	}
}

func TestConfigYAMLDomainPolicy(t *testing.T) {
	var c Config
	raw := `languages: [en]
domains:
  - name: main
    hosts: [localhost]
    gallery:
      require_entities: [{kind: tag, slug: yaoi}]
      filters:
        - entity: {kind: tag, slug: gore}
          visible: false
          user_toggle: false
`
	if err := yaml.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Domains) != 1 || c.Domains[0].Kinds["gallery"].Filters[0].Toggleable() || !c.Domains[0].Kinds["gallery"].Requires(GroupRef{Kind: "tag", Slug: "yaoi"}) {
		t.Fatalf("domain policy: %+v", c)
	}
	encoded, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "content_filters:") || strings.Contains(string(encoded), "sites:") {
		t.Fatalf("retired policy emitted: %s", encoded)
	}
	var roundTrip Config
	if err := yaml.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if len(roundTrip.Domains[0].Kinds["gallery"].Filters) != 1 {
		t.Fatalf("lost inline policy: %s", encoded)
	}
}

func TestDomainYAMLAndJSONShapes(t *testing.T) {
	domain := doujins().Domains[0]
	encoded, err := yaml.Marshal(domain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "kinds:") || !strings.Contains(string(encoded), "gallery:") {
		t.Fatalf("YAML kind policies must be direct domain keys: %s", encoded)
	}
	jsonData, err := json.Marshal(domain)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(jsonData, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["kinds"]; !ok || decoded["gallery"] != nil {
		t.Fatalf("JSON keeps its explicit kinds object: %s", jsonData)
	}
}
