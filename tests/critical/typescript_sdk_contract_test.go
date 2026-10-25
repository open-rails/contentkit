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

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/importer"
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
}
