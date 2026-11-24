package critical_test

import (
	"net/netip"
	"testing"

	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/kinds"
)

// An IPv4-mapped network is stored as the IPv4 network it names, so it covers
// the IPv4 visitors it was written for; one that mixes in IPv6 is refused.
func TestIPv4MappedNetworks(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) {
		article := cfg.Content.Kinds["article"]
		article.Versions.Hidden = false
		cfg.Content.Kinds["article"] = article
		cfg.Content.ContentFilters = kinds.ContentFilters{AppliesTo: []string{"article"}, Domains: []kinds.Domain{{Name: "main", Hosts: []string{"main.invalid"}}}}
	})
	a := w.apps[0]
	admin, ctx := a.engine.Client(), t.Context()
	item, err := admin.PostContents(ctx, catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Mapped network target"}}}}})
	must(t, err)
	_, err = w.pool.Exec(ctx, `UPDATE `+pgx.Identifier{w.schema, "content_facets"}.Sanitize()+` SET live_from=now()-interval '1 hour',published_at=now()-interval '1 hour',access_level='public' WHERE tenant_id=$1 AND content_id=$2`, a.cfg.Scope.AppID.String(), item.ID)
	must(t, err)
	covered := a.engine.VisitorClient(netip.MustParseAddr("192.0.2.5"), "main.invalid")
	outside := a.engine.VisitorClient(netip.MustParseAddr("198.51.100.5"), "main.invalid")
	_, err = covered.GetContent(ctx, item.ID, ck.GetContentQuery{})
	must(t, err)
	t.Run("CK-J20-S10_mapped_location_network_covers_ipv4_visitors", func(t *testing.T) {
		rule, err := admin.PostModerationLocationRules(ctx, content.LocationRuleInput{Locations: []content.LocationEntry{{Network: "::ffff:192.0.2.0/120"}},
			Target: "content", ContentIDs: []string{item.ID}, Domains: []string{"main"}, Enabled: true})
		must(t, err)
		defer func() { must(t, admin.DeleteModerationLocationRule(ctx, rule.ID)) }()
		if rule.Locations[0].Network != "192.0.2.0/24" {
			t.Fatalf("mapped network stored as %q", rule.Locations[0].Network)
		}
		_, err = covered.GetContent(ctx, item.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		_, err = outside.GetContent(ctx, item.ID, ck.GetContentQuery{})
		must(t, err)
	})
	t.Run("CK-J20-S10_mapped_ip_ban_is_the_ipv4_network", func(t *testing.T) {
		bans, err := admin.PostModerationIpBans(ctx, []content.IPBanInput{{Network: "::ffff:203.0.113.7"}, {Network: "::ffff:198.51.100.0/120"}})
		must(t, err)
		got := map[string]bool{}
		for _, b := range bans {
			got[b.Network] = true
		}
		if !got["203.0.113.7/32"] || !got["198.51.100.0/24"] {
			t.Fatalf("mapped bans stored as %+v", bans)
		}
	})
	t.Run("CK-J20-S10_network_mixing_mapped_and_ipv6_is_refused", func(t *testing.T) {
		_, err := admin.PostModerationLocationRules(ctx, content.LocationRuleInput{Locations: []content.LocationEntry{{Network: "::ffff:0:0/80"}},
			Target: "site", Domains: []string{"main"}})
		requireError(t, err, 400, "invalid_request")
		_, err = admin.PostModerationIpBans(ctx, []content.IPBanInput{{Network: "::ffff:0:0/95"}})
		requireError(t, err, 400, "invalid_request")
	})
}
