package critical_test

import (
	"net/netip"
	"testing"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/kinds"
)

func TestChannelReadBoundary(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		kind := cfg.Content.Kinds["article"]
		kind.Channel = &kinds.ChannelPolicy{}
		cfg.Content.Kinds["article"] = kind
	})
	a := w.apps[0]
	startPublicationRuntime(t, a)
	_, token, channel := ownedChannel(t, w, a, "Located channel")
	owner := a.client(token)
	item, err := owner.PostContents(t.Context(), catalog.ContentInput{Kind: "article", ChannelID: channel.ID, Versions: []catalog.VersionInput{
		{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Channel located post", Body: "Channel located body"}}}}})
	must(t, err)
	_, err = owner.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, owner, item.ID, item.Versions[0].ID)
	blocked := a.engine.VisitorClient(netip.MustParseAddr("203.0.113.45"), "")
	uncovered := a.engine.VisitorClient(netip.MustParseAddr("203.0.114.45"), "")
	admin := a.engine.Client()

	t.Run("CK-J07-S32_channel_targeted_location_rules_cover_its_content", func(t *testing.T) {
		before, err := blocked.GetChannel(t.Context(), channel.ID, ck.GetChannelQuery{})
		must(t, err)
		if before.Count != 1 {
			t.Fatalf("channel count positive control: %+v", before)
		}
		rule, err := admin.PostModerationLocationRules(t.Context(), content.LocationRuleInput{Locations: []content.LocationEntry{{Network: "203.0.113.0/24"}},
			Target: "entities", EntityIDs: []string{channel.ID}, Domains: []string{"main"}, Enabled: true})
		must(t, err)
		defer func() { must(t, admin.DeleteModerationLocationRule(t.Context(), rule.ID)) }()
		_, err = blocked.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		listed, err := blocked.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article"})
		must(t, err)
		assertCards(t, listed)
		feed, err := blocked.GetChannelContents(t.Context(), channel.ID, ck.GetChannelContentsQuery{})
		must(t, err)
		assertCards(t, feed)
		page, err := blocked.GetChannel(t.Context(), channel.ID, ck.GetChannelQuery{})
		must(t, err)
		if page.Count != 0 {
			t.Fatalf("channel page counted covered content: %+v", page)
		}
		_, err = uncovered.GetContent(t.Context(), item.ID, ck.GetContentQuery{})
		must(t, err)
		elsewhere, err := uncovered.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article"})
		must(t, err)
		assertCards(t, elsewhere, item.ID)
	})
	t.Run("CK-J07-S33_deleted_channel_metadata_is_not_public", func(t *testing.T) {
		public := a.client("")
		_, err := public.GetChannelEntities(t.Context(), channel.ID, ck.GetChannelEntitiesQuery{})
		must(t, err)
		must(t, admin.DeleteChannel(t.Context(), channel.ID))
		_, err = public.GetChannelEntities(t.Context(), channel.ID, ck.GetChannelEntitiesQuery{})
		requireError(t, err, 404, "not_found")
		managed, err := admin.GetManageChannel(t.Context(), channel.ID, ck.GetManageChannelQuery{})
		must(t, err)
		if managed.ID != channel.ID {
			t.Fatalf("management lost the deleted channel: %+v", managed)
		}
	})
}
