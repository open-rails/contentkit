package critical_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/notification"
	"github.com/open-rails/helpers/notify"
	"github.com/open-rails/openrails/billing"
)

func runTypeScriptContract(t *testing.T, group string, fixture map[string]any, want []string) {
	t.Helper()
	input, err := json.Marshal(fixture)
	must(t, err)
	command := exec.CommandContext(t.Context(), "node", "typescript_contracts/driver.mjs", group)
	command.Env = append(os.Environ(), "CONTENTKIT_TYPESCRIPT_FIXTURE="+string(input))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("built TypeScript %s: %v\n%s", group, err, output)
	}
	var report struct {
		Group    string
		Bindings []string
		Passed   bool
	}
	must(t, json.Unmarshal([]byte(strings.TrimSpace(string(output))), &report))
	sort.Strings(want)
	sort.Strings(report.Bindings)
	if report.Group != group || !report.Passed || !reflect.DeepEqual(report.Bindings, want) {
		t.Fatalf("required semantic binding cases omitted or changed: %+v want%v", report, want)
	}
}

func TestTypeScriptPublicContracts(t *testing.T) {
	t.Run("catalog_publication", func(t *testing.T) {
		w := newWorld(t)
		a := w.apps[0]
		owner, token := w.person(t, 0)
		_, readerToken := w.person(t, 0)
		persona, err := w.auth.Persona("app")
		must(t, err)
		authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(owner.ID), persona.OwnerRole())
		prior := w.clock.now().Add(-time.Hour)
		imported, err := a.engine.Client().PostImport(t.Context(), importer.Batch{Source: "typescript-critical", Contents: []importer.ContentRecord{{ExternalID: "historically-deleted", Kind: "article", State: "deleted", PublishedAt: &prior, PublishAt: &prior, DeletedAt: &prior, Text: map[string]catalog.Text{"en": {Title: "Restorable imported item", Body: "Retained historical body"}}}}})
		must(t, err)
		if len(imported.Failed) != 0 || imported.Written["contents"] != 1 {
			t.Fatalf("public import fixture did not preserve independently deleted content: %+v", imported)
		}
		legacy := imported.IDs["content"]["historically-deleted"]
		if legacy == "" {
			t.Fatal("import fixture omitted content identity")
		}
		runTypeScriptContract(t, "catalog_publication", map[string]any{"base": a.server.URL + "/content", "owner": token, "reader": readerToken, "legacy": legacy, "app": a.cfg.Scope.AppID.String()}, []string{
			"CatalogClient.listEvents", "CatalogClient.totals", "ContentsClient.publish", "ContentsClient.publishWhenReady", "ContentsClient.unpublish", "ContentsClient.restore", "ContentsClient.removeVersion", "ContentKitClient.config", "ContentKitClient.kinds", "ContentKitClient.url",
		})
	})
	t.Run("moderation_bans", func(t *testing.T) {
		w := newWorld(t, func(c *ck.Config) { c.Interactions.Limits.Disabled = true })
		a := w.apps[0]
		owner, token := w.person(t, 0)
		reader, readerToken := w.person(t, 0)
		persona, err := w.auth.Persona("app")
		must(t, err)
		authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(owner.ID), persona.OwnerRole())
		other := publishedArticle(t, a, "A thread with a different owner")
		runTypeScriptContract(t, "moderation_bans", map[string]any{"base": a.server.URL + "/content", "owner": token, "reader": readerToken, "ownerID": owner.ID, "readerID": reader.ID, "other": other.ID}, []string{
			"BansClient.list", "BansClient.get", "BansClient.ban", "BansClient.lift", "ModerationClient.held", "ModerationClient.resolve", "ModerationClient.bannedWords", "ModerationClient.banWords", "ModerationClient.unbanWords", "ModerationClient.ipBans", "ModerationClient.banIPs", "ModerationClient.liftIPBans",
		})
	})

	t.Run("discovery_entities", func(t *testing.T) {
		w := newWorld(t, func(c *ck.Config) {
			c.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags", MembershipScopes: []kinds.MembershipScope{kinds.ContentScope}}
			kind := c.Content.Kinds["article"]
			kind.Entities = []string{"tag"}
			c.Content.Kinds["article"] = kind
		})
		a := w.apps[0]
		commerce := attachBilling(t, w, a)
		configureDiscoverySignals(t, w, a)
		a.server.Close()
		must(t, a.engine.Close(t.Context()))
		var err error
		a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
		must(t, err)
		a.server = mountEngine(t, a.engine)
		owner, token, channel := ownedEntity(t, w, a, "TypeScript owned channel")
		reader, readerToken := w.person(t, 0)
		persona, err := w.auth.Persona("app")
		must(t, err)
		authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(owner.ID), persona.OwnerRole())
		paid, err := a.engine.Client().PostContents(t.Context(), catalog.ContentInput{Kind: "article", Access: &access.Rule{Level: access.PPV}, Text: map[string]catalog.Text{"en": {Title: "Purchased TypeScript library", Body: "Granted body"}}})
		must(t, err)
		_, err = a.engine.Client().PostContentPublish(t.Context(), paid.ID, catalog.PublishInput{})
		must(t, err)
		key, err := access.ContentKey(a.cfg.Scope.AppID.String(), paid.ID)
		must(t, err)
		product, err := commerce.merchant.CreateProduct(t.Context(), billing.CreateProductParams{Key: "typescript-library", DisplayName: "Library", Entitlements: []string{key.String()}})
		must(t, err)
		customer, err := billing.ParseCustomerID(reader.ID)
		must(t, err)
		_, err = commerce.merchant.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
		must(t, err)
		grants, err := commerce.merchant.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: "typescript-access", Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID}}})
		must(t, err)
		if len(grants) != 1 {
			t.Fatal("real purchased library grant absent")
		}
		foreign := publishedArticle(t, w.apps[1], "TypeScriptNeedle foreign app")
		must(t, a.engine.Start(t.Context()))
		runTypeScriptContract(t, "discovery_entities", map[string]any{"base": a.server.URL + "/content", "owner": token, "reader": readerToken, "ownerID": owner.ID, "channel": string(channel.ID), "foreign": foreign.ID, "paid": paid.ID}, []string{
			"EntitiesClient.create", "EntitiesClient.update", "EntitiesClient.remove", "EntitiesClient.mine", "DiscoveryClient.library", "DiscoveryClient.list", "DiscoveryClient.knownCards", "DiscoveryClient.search", "DiscoveryClient.typeahead", "DiscoveryClient.adminSuggestions", "DiscoveryClient.similar", "DiscoveryClient.recommendations", "DiscoveryClient.releaseDates", "DiscoveryClient.releases",
		})
	})

	t.Run("notification_receipts", func(t *testing.T) {
		w := newWorld(t)
		a := w.apps[0]
		recipient, token := w.person(t, 0)
		stranger, strangerToken := w.person(t, 0)
		persona, err := w.auth.Persona("app")
		must(t, err)
		authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(stranger.ID), persona.OwnerRole())
		inputs := []notify.Input{}
		for i, name := range []string{"first", "second", "other"} {
			user := recipient.ID
			if name == "other" {
				user = stranger.ID
			}
			body, err := json.Marshal(map[string]string{"message": "Private " + name})
			must(t, err)
			inputs = append(inputs, notify.Input{Recipient: notify.Recipient{Audience: "user", ID: user}, Origin: notify.Origin{System: "typescript", Scope: a.cfg.Scope.AppID.String(), ID: uuid.NewString()}, Kind: "critical." + name, Data: body, CreatedAt: w.clock.now().Add(time.Duration(i-4) * time.Second), Status: "open", ActionRequested: true})
		}
		published, err := a.engine.Client().PostNotificationsPublish(t.Context(), notification.Publication{Notifications: inputs})
		must(t, err)
		if len(published.Items) != 3 {
			t.Fatal("three actual recipient receipts are required")
		}
		ids := map[string]string{}
		for _, item := range published.Items {
			ids[item.Kind] = item.ID
		}
		runTypeScriptContract(t, "notification_receipts", map[string]any{"base": a.server.URL + "/content", "owner": token, "reader": strangerToken, "recipient": recipient.ID, "first": ids["critical.first"], "second": ids["critical.second"], "other": ids["critical.other"]}, []string{"NotificationsClient.list", "NotificationsClient.refresh", "NotificationsClient.setRead", "NotificationsClient.setDismissed"})
	})

}
