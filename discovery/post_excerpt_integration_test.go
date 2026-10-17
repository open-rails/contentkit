package discovery

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/open-rails/contentkit/kinds"
)

func TestPostCardsCarryAnExcerptWithoutLeakingLockedBodies(t *testing.T) {
	cfg := siteConfig()
	cfg.Kinds["blog"] = kinds.Kind{Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one"},
		Locked: &kinds.Locked{Show: []string{kinds.ShowCover}}}
	f := newFixtureConfig(t, nil, "", cfg)
	premium := `{"level":"tier","tiers":["premium"]}`
	f.seed(
		item{n: 1, kind: "blog", titles: map[string]string{"en": "Described"}, age: time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 2, kind: "blog", titles: map[string]string{"en": "Body only"}, age: 2 * time.Hour, versions: []ver{{langs: []string{"en"}}}},
		item{n: 3, kind: "blog", titles: map[string]string{"en": "Locked"}, age: 3 * time.Hour, rule: premium, versions: []ver{{langs: []string{"en"}}}},
		item{n: 4, kind: "blog", titles: map[string]string{"en": "Locked body"}, age: 4 * time.Hour, rule: premium, versions: []ver{{langs: []string{"en"}}}},
	)
	f.seed(item{n: 5, titles: map[string]string{"en": "Gallery"}, age: 5 * time.Hour, versions: []ver{{langs: []string{"en"}}}})
	text := func(n int, description, body string) {
		f.exec(`UPDATE {s}.content_text SET description = $2, body = $3 WHERE tenant_id = $1 AND content_id = $4`, tenant, description, body, f.cid(n))
	}
	long := strings.Repeat("words ", 120)
	text(1, "  A plain   summary. ", "<p>Ignored body</p>")
	text(2, "", "<p>Hello &amp; <strong>welcome</strong></p><p>"+long+"</p>")
	text(3, "Secret summary", "<p>Secret body</p>")
	text(4, "", "<p>Paid body</p>")
	text(5, "Gallery summary", "")

	excerpts := func(user, kind string) map[string]string {
		t.Helper()
		_, page := f.list("main.test", user, "/contents?kind="+kind)
		out := map[string]string{}
		for _, c := range page.Contents {
			out[c.ID] = c.Excerpt
		}
		return out
	}
	guest := excerpts("", "blog")
	if guest[f.cid(1)] != "A plain summary." {
		t.Errorf("description excerpt %q", guest[f.cid(1)])
	}
	body := guest[f.cid(2)]
	if !strings.HasPrefix(body, "Hello & welcome words") || !strings.HasSuffix(body, "…") || utf8.RuneCountInString(body) > excerptChars+1 || strings.Contains(body, "<") {
		t.Errorf("body excerpt %q", body)
	}
	// The locked view shows no description and a locked body never shows.
	if guest[f.cid(3)] != "" || guest[f.cid(4)] != "" {
		t.Errorf("locked excerpts leaked: %q %q", guest[f.cid(3)], guest[f.cid(4)])
	}
	fan := excerpts("fan", "blog")
	if fan[f.cid(3)] != "Secret summary" || fan[f.cid(4)] != "Paid body" {
		t.Errorf("opened excerpts %q %q", fan[f.cid(3)], fan[f.cid(4)])
	}
	if g := excerpts("", "gallery")[f.cid(5)]; g != "" {
		t.Errorf("only post kinds carry excerpts: %q", g)
	}
}
