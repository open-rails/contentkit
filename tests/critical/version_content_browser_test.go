package critical_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
)

// The actual editor and reader hook must address the selected edition without
// borrowing a sibling's rendered text or losing the parent navigation index.
func TestShippedVersionContentBrowser(t *testing.T) {
	if _, err := os.Stat(".browser-dist/index.html"); err != nil {
		t.Fatal("build the shipped SDK and browser harness before selecting this test")
	}
	w := newWorld(t, func(c *ck.Config) {
		c.Content.Languages = append(c.Content.Languages, "zh")
		kind := c.Content.Kinds["article"]
		kind.Versions.Hidden = false
		c.Content.Kinds["article"] = kind
	})
	a := w.apps[0]
	attachMedia(t, w, a)
	editor, token := w.person(t, 0)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(editor.ID), w.writer)
	client := a.client(token)
	item, err := client.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{
		{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "English edition", Body: "English body only"}}},
		{Languages: []string{"zh"}, Text: map[string]catalog.VersionText{"zh": {Title: "中文版本", Body: "中文内容"}}},
	}})
	must(t, err)
	_, err = client.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	for _, version := range item.Versions {
		awaitEditionPublication(t, a, client, item.ID, version.ID)
	}
	base, err := url.Parse(a.server.URL)
	must(t, err)
	mux := http.NewServeMux()
	mux.Handle("/content/", httputil.NewSingleHostReverseProxy(base))
	mux.Handle("/", http.FileServer(http.Dir(".browser-dist")))
	server := httptest.NewServer(mux)
	defer server.Close()
	fixture, err := json.Marshal(map[string]string{"url": server.URL, "item": item.ID, "alice": editor.ID, "aliceToken": token, "enVersion": item.Versions[0].ID, "zhVersion": item.Versions[1].ID})
	must(t, err)
	command := exec.CommandContext(t.Context(), "node", "browser/driver.mjs", "CK-J29-S09")
	command.Env = append(os.Environ(), "CONTENTKIT_BROWSER_FIXTURE="+string(fixture))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("real version editor browser: %v\n%s", err, output)
	}
	var receipt struct {
		Scenario string `json:"scenario"`
		Status   string `json:"status"`
	}
	must(t, json.Unmarshal([]byte(strings.TrimSpace(string(output))), &receipt))
	if receipt.Scenario != "CK-J29-S09" || receipt.Status != "passed" {
		t.Fatalf("version editor scenario did not execute: %s", output)
	}
}
