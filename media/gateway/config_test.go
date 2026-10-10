package gateway

import (
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media/layout"
)

func TestDefaultSelection(t *testing.T) {
	target := "sha256-" + strings.Repeat("a", 64) + ".webp"
	defs, err := layout.CompileDefaults([]layout.Default{{Namespace: "d", Kind: "gallery", Files: map[string]string{"cover-460.webp": target}}})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{defaults: defs}
	for name, want := range map[string]bool{
		"cover-460.webp": true, "cover-0.webp": false, "cover-.webp": false, "cover-46a.webp": false, "cover-460.webpx": false,
		"cover-460-0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b.webp": true,
		"cover-460-not-a-generation.webp":                     false,
		"xcover-460.webp":                                     false, "cover-460xwebp": false,
	} {
		if got := h.defaultName(object{ns: "d", kind: "gallery", name: name}) != ""; got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	if h.defaultName(object{ns: "d", kind: "video", name: "cover-460.webp"}) != "" {
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
		"bad template": func(c *Config) {
			c.Defaults = []layout.Default{{Namespace: "doujins", Kind: "gallery", Files: map[string]string{"cover-{h}.webp": "fixed.webp"}}}
		},
		"no names":       func(c *Config) { c.Defaults = []layout.Default{{Namespace: "doujins", Kind: "gallery"}} },
		"bad endpoint":   func(c *Config) { c.Endpoint = "localhost:9000" },
		"bad bucket":     func(c *Config) { c.Bucket = "a/b" },
		"no credentials": func(c *Config) { c.SecretAccessKey = "" },
	} {
		c := base
		mut(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
