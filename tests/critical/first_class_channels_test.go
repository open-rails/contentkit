package critical_test

import (
	"encoding/json"
	"github.com/goccy/go-yaml"
	"github.com/open-rails/contentkit/kinds"
	"strings"
	"testing"
)

func TestFirstClassChannelConfiguration(t *testing.T) {
	const model = `languages: [en, zh]
channels:
  creation: staff
  entities: [tag]
content_kinds:
  blog:
    route: blog
    presentation: post
    versions: {languages: one, hidden: true}
    access_levels: [public]
    channel: {fixed: blog}
    entities: [tag]
entity_kinds:
  tag: {creatable: true}
`
	t.Run("CK-J03-S67_fixed_blog_without_public_channel_route", func(t *testing.T) {
		var cfg kinds.Config
		must(t, yaml.Unmarshal([]byte(model), &cfg))
		must(t, cfg.Validate())
		if cfg.Channels.Route != "" || cfg.Kinds["blog"].Channel.Fixed != "blog" {
			t.Fatal("fixed internal channel policy changed")
		}
		if _, found := cfg.EntityKinds["channel"]; found {
			t.Fatal("channel leaked into descriptive metadata")
		}
		encoded, err := json.Marshal(cfg)
		must(t, err)
		var again kinds.Config
		must(t, json.Unmarshal(encoded, &again))
		must(t, again.Validate())
		if _, found := again.EntityKinds["channel"]; found {
			t.Fatal("public config roundtrip exposed private channel storage")
		}
		runtime := cfg.StorageModel()
		if !runtime.EntityKinds["channel"].AuthScope {
			t.Fatal("channel has no authoring scope")
		}
		if _, found := cfg.EntityKinds["channel"]; found {
			t.Fatal("runtime storage mutated caller config")
		}
	})
	t.Run("CK-J03-S68_metadata_cannot_become_an_authoring_scope", func(t *testing.T) {
		for _, field := range []string{"owner: user", "auth_scope: true", "entitled: true"} {
			var cfg kinds.Config
			err := yaml.Unmarshal([]byte(strings.Replace(model, "tag: {creatable: true}", "tag: {"+field+"}", 1)), &cfg)
			if err == nil {
				err = cfg.Validate()
			}
			if err == nil {
				t.Fatalf("metadata accepted %s", field)
			}
		}
		var cfg kinds.Config
		must(t, yaml.Unmarshal([]byte(model), &cfg))
		cfg.EntityKinds["channel"] = kinds.EntityKind{}
		if cfg.Validate() == nil {
			t.Fatal("reserved channel accepted as metadata")
		}
	})
	t.Run("CK-J03-S69_channel_policy_rejects_ambiguous_configuration", func(t *testing.T) {
		var cfg kinds.Config
		must(t, yaml.Unmarshal([]byte(model), &cfg))
		cfg.Channels = nil
		if cfg.Validate() == nil {
			t.Fatal("fixed policy accepted without channels")
		}
		must(t, yaml.Unmarshal([]byte(model), &cfg))
		cfg.Channels.Route = "blog"
		if cfg.Validate() == nil {
			t.Fatal("content and channel routes collided")
		}
		must(t, yaml.Unmarshal([]byte(model), &cfg))
		kind := cfg.Kinds["blog"]
		kind.Entities = append(kind.Entities, "channel")
		cfg.Kinds["blog"] = kind
		if cfg.StorageModel().Validate() == nil {
			t.Fatal("channel accepted as a descriptive association")
		}
	})
}
