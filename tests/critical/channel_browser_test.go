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

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
)

// The shipped channel UI reads live destination authority and writes repost
// references; removing a reference must never invoke content deletion.
func TestShippedChannelBrowser(t *testing.T) {
	if _, err := os.Stat(".browser-dist/index.html"); err != nil {
		t.Fatal("build the shipped UI and critical browser harness before this selected test")
	}
	w := newWorld(t, func(cfg *ck.Config) {
		kind := cfg.Content.Kinds["article"]
		kind.Channel = &kinds.ChannelPolicy{}
		cfg.Content.Kinds["article"] = kind
	})
	a := w.apps[0]
	attachMedia(t, w, a)
	alice, aliceToken := w.person(t, 0)
	_, bobToken := w.person(t, 0)
	originalChannel, err := a.client(bobToken).PostChannels(t.Context(), channels.Input{Slug: "browser-original", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Original channel"}}})
	must(t, err)
	destination, err := a.client(aliceToken).PostChannels(t.Context(), channels.Input{Slug: "browser-destination", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Destination channel"}}})
	must(t, err)
	original, err := a.client(bobToken).PostContents(t.Context(), catalog.ContentInput{Kind: "article", ChannelID: originalChannel.ID, Text: map[string]catalog.Text{"en": {Title: "Browser canonical original", Body: "One original, shared by reference."}}})
	must(t, err)
	_, err = a.client(bobToken).PostContentPublish(t.Context(), original.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, a.client(bobToken), original.ID, original.Versions[0].ID)
	base, err := url.Parse(a.server.URL)
	must(t, err)
	mux := http.NewServeMux()
	mux.Handle("/content/", httputil.NewSingleHostReverseProxy(base))
	mux.HandleFunc("/fixture.png", func(out http.ResponseWriter, _ *http.Request) {
		out.Header().Set("Content-Type", "image/png")
		_, _ = out.Write(imageBytes(t))
	})
	mux.Handle("/", http.FileServer(http.Dir(".browser-dist")))
	server := httptest.NewServer(mux)
	defer server.Close()
	fixture, err := json.Marshal(map[string]string{"url": server.URL, "item": original.ID, "alice": alice.ID, "aliceToken": aliceToken, "originalChannel": originalChannel.ID, "destination": destination.ID})
	must(t, err)
	command := exec.CommandContext(t.Context(), "node", "browser/driver.mjs", "CK-J29-S08")
	command.Env = append(os.Environ(), "CONTENTKIT_BROWSER_FIXTURE="+string(fixture))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("real channel browser: %v\n%s", err, output)
	}
	var receipt struct {
		Scenario string `json:"scenario"`
		Status   string `json:"status"`
	}
	must(t, json.Unmarshal([]byte(strings.TrimSpace(string(output))), &receipt))
	if receipt.Scenario != "CK-J29-S08" || receipt.Status != "passed" {
		t.Fatalf("channel browser journey not executed: %s", output)
	}
}
