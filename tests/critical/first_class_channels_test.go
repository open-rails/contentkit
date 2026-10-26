package critical_test

import (
	"encoding/json"
	"github.com/goccy/go-yaml"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/importer"
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

func TestChannelRepostAnonymization(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	_, sourceToken, source := ownedChannel(t, w, a, "Retained original")
	original := authoredArticle(t, a, a.client(sourceToken), source.ID)
	user, token, destination := ownedChannel(t, w, a, "Repost destination")
	client := a.client(token)
	posted, err := client.PutChannelRepost(t.Context(), destination.ID, original.ID)
	must(t, err)
	if posted.ActorID != user.ID {
		t.Fatal("repost did not record initial attribution")
	}
	t.Run("CK-J03-S70_import_cannot_reassign_original_channel", func(t *testing.T) {
		result, err := a.engine.Client().PostImport(t.Context(), importer.Batch{Source: "channel-transfer", Contents: []importer.ContentRecord{{ExternalID: "original", ID: original.ID, Kind: "article", State: "published", Channel: &importer.Ref{ID: destination.ID}}}})
		must(t, err)
		if len(result.Failed) != 1 {
			t.Fatalf("import changed original channel: %+v", result)
		}
		still, err := a.client("").GetContent(t.Context(), original.ID, ck.GetContentQuery{})
		must(t, err)
		if still.ChannelID != source.ID {
			t.Fatal("failed import changed primary channel")
		}
	})
	t.Run("CK-J24-S07_terminal_account_event_unlinks_repost_attribution", func(t *testing.T) {
		must(t, a.engine.AnonymizeAccount(t.Context(), user.ID))
		must(t, a.engine.AnonymizeAccount(t.Context(), user.ID))
		feed, err := a.client("").GetChannelContents(t.Context(), destination.ID, ck.GetChannelContentsQuery{})
		must(t, err)
		if len(feed.Contents) != 1 || feed.Contents[0].ID != original.ID || feed.Contents[0].Repost == nil || feed.Contents[0].Repost.ActorID != "" {
			t.Fatalf("anonymization lost reference or retained account: %+v", feed.Contents)
		}
		still, err := a.client("").GetContent(t.Context(), original.ID, ck.GetContentQuery{})
		must(t, err)
		if still.ChannelID != source.ID {
			t.Fatal("account purge changed original channel")
		}
		_, err = client.PutChannelRepost(t.Context(), destination.ID, original.ID)
		requireError(t, err, 403, "forbidden")
		log, err := a.engine.Client().GetEvents(t.Context(), ck.GetEventsQuery{})
		must(t, err)
		for _, event := range log.Events {
			if event.Type == "content.reposted" && strings.Contains(string(event.Data), user.ID) {
				t.Fatal("immutable repost event retained erasable account attribution")
			}
		}
	})
}

func TestChannelMetadataMerge(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		cfg.Content.EntityKinds["tag"] = kinds.EntityKind{}
		cfg.Content.Channels.Entities = []string{"tag"}
	})
	a := w.apps[0]
	_, token, channel := ownedChannel(t, w, a, "Described channel")
	tags, err := a.engine.Client().PostEntities(t.Context(), []entities.EntityInput{{Kind: "tag", Slug: "old-topic", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Old topic"}}}, {Kind: "tag", Slug: "canonical-topic", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Canonical topic"}}}})
	must(t, err)
	t.Run("CK-J03-S73_channel_metadata_survives_canonical_entity_merge", func(t *testing.T) {
		must(t, a.client(token).PutChannelEntities(t.Context(), channel.ID, []string{string(tags[0].ID), string(tags[1].ID)}))
		_, err := a.engine.Client().PostEntityMerge(t.Context(), string(tags[0].ID), entities.MergeInput{Into: tags[1].ID})
		must(t, err)
		got, err := a.client("").GetChannelEntities(t.Context(), channel.ID, ck.GetChannelEntitiesQuery{})
		must(t, err)
		if len(got) != 1 || got[0].ID != tags[1].ID {
			t.Fatalf("channel metadata did not follow canonical survivor: %+v", got)
		}
		_, err = a.client("").GetEntity(t.Context(), channel.ID, ck.GetEntityQuery{})
		requireError(t, err, 404, "not_found")
	})
}
