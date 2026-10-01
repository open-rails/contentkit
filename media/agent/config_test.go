package agent

import (
	"reflect"
	"testing"
)

func TestParseAndFormat(t *testing.T) {
	hosts, err := ParseHosts(" Media.Doujins.ai = doujins, accounts ;media.hanime.media=hentai0,accounts; ")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"media.doujins.ai": {"doujins", "accounts"}, "media.hanime.media": {"hentai0", "accounts"}}
	if !reflect.DeepEqual(hosts, want) {
		t.Fatalf("ParseHosts = %v", hosts)
	}
	const hs = "media.doujins.ai=doujins,accounts; media.hanime.media=hentai0,accounts"
	if got := FormatHosts(hosts); got != hs {
		t.Fatalf("FormatHosts = %q", got)
	}

	defs, err := ParseDefaults("hentai0/video: poster-{w}.webp, thumb-{w}.webp; doujins/gallery:cover-{w}.webp")
	if err != nil {
		t.Fatal(err)
	}
	wantDefs := []Default{{"hentai0", "video", []string{"poster-{w}.webp", "thumb-{w}.webp"}}, {"doujins", "gallery", []string{"cover-{w}.webp"}}}
	if !reflect.DeepEqual(defs, wantDefs) {
		t.Fatalf("ParseDefaults = %v", defs)
	}
	const ds = "doujins/gallery: cover-{w}.webp; hentai0/video: poster-{w}.webp, thumb-{w}.webp"
	if got := FormatDefaults(defs); got != ds {
		t.Fatalf("FormatDefaults = %q", got)
	}
	if again, err := ParseDefaults(ds); err != nil || FormatDefaults(again) != ds {
		t.Fatalf("defaults round trip: %v %v", again, err)
	}
	if d, err := ParseDefaults(" "); err != nil || d != nil {
		t.Fatalf("empty defaults: %v %v", d, err)
	}

	for _, bad := range []string{"", "media.doujins.ai", "media.doujins.ai=", "=doujins", "media.doujins.ai:443=doujins",
		"media.doujins.ai=doujins; media.doujins.ai=accounts", "media.doujins.ai=.hidden", "media.doujins.ai=a/b", "*.doujins.ai=doujins"} {
		if _, err := ParseHosts(bad); err == nil {
			t.Errorf("ParseHosts(%q) accepted", bad)
		}
	}
	for _, bad := range []string{"doujins: cover.webp", "doujins/gallery", "doujins/gallery:", "doujins/gallery: {h}.webp",
		"doujins/gallery: cover-{w.webp", "doujins/gallery: .{w}", "doujins/gallery: a/b", "doujins/gallery: a; doujins/gallery: b"} {
		if _, err := ParseDefaults(bad); err == nil {
			t.Errorf("ParseDefaults(%q) accepted", bad)
		}
	}
}

func TestDefaultTemplates(t *testing.T) {
	defs, err := compileDefaults([]Default{{"d", "gallery", []string{"cover-{w}.webp", "{name}.png", "x{w}-{name}.jpg"}}})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{defaults: defs}
	for name, want := range map[string]bool{
		"cover-460.webp": true, "cover-0.webp": true, "cover-.webp": false, "cover-46a.webp": false, "cover-460.webpx": false,
		"xcover-460.webp": false, "cover-460xwebp": false, "a.b-c.png": true, ".png": false, "x12-a.jpg": true, "x-a.jpg": false,
	} {
		if got := h.hasDefault(object{ns: "d", kind: "gallery", name: name}); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	if h.hasDefault(object{ns: "d", kind: "video", name: "cover-460.webp"}) {
		t.Error("another kind matched")
	}
}

func TestNewValidates(t *testing.T) {
	base := Config{Endpoint: "http://localhost:9000", Bucket: "media", AccessKeyID: "a", SecretAccessKey: "s",
		Hosts: map[string][]string{"media.doujins.ai": {"doujins"}}}
	if _, err := New(base); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"no hosts":         func(c *Config) { c.Hosts = nil },
		"host with port":   func(c *Config) { c.Hosts = map[string][]string{"media.doujins.ai:443": {"doujins"}} },
		"upper-case host":  func(c *Config) { c.Hosts = map[string][]string{"Media.doujins.ai": {"doujins"}} },
		"no namespaces":    func(c *Config) { c.Hosts = map[string][]string{"media.doujins.ai": nil} },
		"wildcard origin":  func(c *Config) { c.Origins = []string{"*"} },
		"origin path":      func(c *Config) { c.Origins = []string{"https://doujins.ai/"} },
		"origin no scheme": func(c *Config) { c.Origins = []string{"doujins.ai"} },
		"bad template":     func(c *Config) { c.Defaults = []Default{{"doujins", "gallery", []string{"cover-{h}.webp"}}} },
		"no names":         func(c *Config) { c.Defaults = []Default{{Namespace: "doujins", Kind: "gallery"}} },
		"bad endpoint":     func(c *Config) { c.Endpoint = "localhost:9000" },
		"bad bucket":       func(c *Config) { c.Bucket = "a/b" },
		"no credentials":   func(c *Config) { c.SecretAccessKey = "" },
	} {
		c := base
		mut(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
