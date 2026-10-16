package kinds

import (
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
    require_entities: [{kind: tag, slug: yaoi}]
    require_kinds: [gallery]
    filters:
      - entity: {kind: tag, slug: gore}
        visible: false
        user_toggle: false
`
	if err := yaml.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Domains) != 1 || c.Domains[0].Filters[0].Toggleable() || c.Domains[0].Filters[0].Visible || !c.Domains[0].Requires(GroupRef{Kind: "tag", Slug: "yaoi"}) || c.Domains[0].RequiresKind("blog") || !c.Domains[0].RequiresKind("gallery") {
		t.Fatalf("domain policy: %+v", c)
	}
	encoded, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "content_filters:") || strings.Contains(string(encoded), "sites:") {
		t.Fatalf("retired policy emitted: %s", encoded)
	}
}
