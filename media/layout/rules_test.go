package layout_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media/layout"
)

func TestParseAndFormat(t *testing.T) {
	hosts, err := layout.ParseHosts(" Media.Doujins.ai = doujins, accounts ;media.hanime.media=hentai0,accounts; ")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"media.doujins.ai": {"doujins", "accounts"}, "media.hanime.media": {"hentai0", "accounts"}}
	if !reflect.DeepEqual(hosts, want) {
		t.Fatalf("ParseHosts = %v", hosts)
	}
	const hs = "media.doujins.ai=doujins,accounts; media.hanime.media=hentai0,accounts"
	if got := layout.FormatHosts(hosts); got != hs {
		t.Fatalf("FormatHosts = %q", got)
	}

	target := "sha256-" + strings.Repeat("a", 64) + ".webp"
	defs, err := layout.ParseDefaults("hentai0/video: poster-460.webp=" + target + ", thumb-230.webp=" + target + "; doujins/gallery: cover-460.webp=" + target)
	if err != nil {
		t.Fatal(err)
	}
	wantDefs := []layout.Default{{Namespace: "hentai0", Kind: "video", Files: map[string]string{"poster-460.webp": target, "thumb-230.webp": target}}, {Namespace: "doujins", Kind: "gallery", Files: map[string]string{"cover-460.webp": target}}}
	if !reflect.DeepEqual(defs, wantDefs) {
		t.Fatalf("ParseDefaults = %v", defs)
	}
	ds := "doujins/gallery: cover-460.webp=" + target + "; hentai0/video: poster-460.webp=" + target + ", thumb-230.webp=" + target
	if got := layout.FormatDefaults(defs); got != ds {
		t.Fatalf("FormatDefaults = %q", got)
	}
	if again, err := layout.ParseDefaults(ds); err != nil || layout.FormatDefaults(again) != ds {
		t.Fatalf("defaults round trip: %v %v", again, err)
	}
	if d, err := layout.ParseDefaults(" "); err != nil || d != nil {
		t.Fatalf("empty defaults: %v %v", d, err)
	}

	for _, bad := range []string{"", "media.doujins.ai", "media.doujins.ai=", "=doujins", "media.doujins.ai:443=doujins",
		"media.doujins.ai=doujins; media.doujins.ai=accounts", "media.doujins.ai=.hidden", "media.doujins.ai=a/b", "*.doujins.ai=doujins"} {
		if _, err := layout.ParseHosts(bad); err == nil {
			t.Errorf("layout.ParseHosts(%q) accepted", bad)
		}
	}
	for _, bad := range []string{"doujins: cover.webp", "doujins/gallery", "doujins/gallery:", "doujins/gallery: {h}.webp",
		"doujins/gallery: cover-{w.webp", "doujins/gallery: .{w}", "doujins/gallery: a/b", "doujins/gallery: a; doujins/gallery: b",
		"doujins/gallery: cover.webp=fixed.webp", "doujins/gallery: cover.webp=" + target + ", cover.webp=" + target,
		"doujins/gallery: ../cover.webp=" + target, "doujins/gallery: cover-{w}.webp=" + target} {
		if _, err := layout.ParseDefaults(bad); err == nil {
			t.Errorf("layout.ParseDefaults(%q) accepted", bad)
		}
	}
}
