package media_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

func TestRegistry(t *testing.T) {
	reg, err := media.NewRegistry(testConfig("doujins", "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Namespaces(); !reflect.DeepEqual(got, []string{"doujins", "accounts"}) {
		t.Fatalf("namespaces %v", got)
	}
	g, _ := reg.Kind("gallery")
	if got := g.Presets("originals/001.png", false); !reflect.DeepEqual(got, []string{"thumb", "high"}) {
		t.Fatalf("presets %v", got)
	}
	if got := g.Presets("cover.jpg", true); got != nil {
		t.Fatalf("a hidden item's cover presets %v", got)
	}
	thumb := &g.Private[0]
	if got := g.OutputPath(thumb, "originals/My Page 7.png"); got != "thumb/My Page 7.webp" {
		t.Fatalf("output path %q", got)
	}
	if got := g.PublicNames(nil, &g.Public[0], "cover.png"); !reflect.DeepEqual(got, []string{"cover-230.webp", "cover-460.webp"}) {
		t.Fatalf("public names %v", got)
	}
	u, _ := reg.Kind("user")
	ref, err := reg.Ref("user", cid(1))
	if err != nil || ref.TenantID != "accounts" || u.NS() != "accounts" {
		t.Fatalf("shared ref %+v %v", ref, err)
	}
	if got := reg.PublicURL(ref, "avatar-64.webp"); got != "https://"+mediaHost+"/v1/accounts/user/"+cid(1)+"/public/avatar-64.webp" {
		t.Fatalf("public URL %s", got)
	}
	if got := reg.SrcSet(ref, "avatar"); !strings.HasSuffix(got, "avatar-128.webp 128w") || !strings.Contains(got, "avatar-64.webp 64w, ") {
		t.Fatalf("srcset %s", got)
	}
	agent := media.AgentConfig(reg)
	if got := layout.FormatDefaults(agent.Defaults); got != "accounts/user: avatar-{w}.webp; doujins/gallery: cover-{w}.webp" {
		t.Fatalf("agent defaults %q", got)
	}
	b, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := media.ParseConfig([]byte(string(b[:len(b)-1]) + `,"unknown":1}`))
	if err != nil {
		t.Fatalf("lenient registry JSON: %v", err)
	}
	again, err := media.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := json.Marshal(again)
	if string(b) != string(b2) {
		t.Fatalf("JSON round trip:\n%s\n%s", b, b2)
	}
}

func TestRegistryRefuses(t *testing.T) {
	images := []string{"image/png"}
	up := []media.Upload{{Path: "originals/{name}", Types: images, MaxBytes: 1}, {Path: "cover", Types: images, MaxBytes: 1}}
	for name, k := range map[string]media.Kind{
		"no uploads":         {Name: "k"},
		"unbounded upload":   {Name: "k", Uploads: []media.Upload{{Path: "a", Types: images}}},
		"bad pattern":        {Name: "k", Uploads: []media.Upload{{Path: "a/{name}/b", Types: images, MaxBytes: 1}}},
		"named literal":      {Name: "k", Uploads: []media.Upload{{Path: "a", Types: images, MaxBytes: 1, Named: true, Max: 1}}},
		"named unbounded":    {Name: "k", Uploads: []media.Upload{{Path: "a/{name}", Types: images, MaxBytes: 1, Named: true}}},
		"frames of no video": {Name: "k", Uploads: append(up, media.Upload{Path: "poster", Types: images, MaxBytes: 1, Frames: "cover"})},
		"two producers":      {Name: "k", Uploads: up, Private: []media.Private{{Name: "p", From: "cover", To: "a.webp", Image: &media.Image{}, MP4: media.Rung(480)}}},
		"from nothing":       {Name: "k", Uploads: up, Private: []media.Private{{Name: "p", From: "nope", To: "a.webp", Image: &media.Image{}}}},
		"hls to a file":      {Name: "k", Uploads: up, Private: []media.Private{{Name: "p", From: "cover", To: "hls.m3u8", HLS: &media.HLS{}}}},
		"to escapes":         {Name: "k", Uploads: up, Private: []media.Private{{Name: "p", From: "cover", To: "../x.webp", Image: &media.Image{}}}},
		"duplicate preset":   {Name: "k", Uploads: up, Private: []media.Private{{Name: "p", From: "cover", To: "a.webp", Image: &media.Image{}}}, Public: []media.Public{{Name: "p", From: "cover", To: "p.webp"}}},
		"public w no widths": {Name: "k", Uploads: up, Public: []media.Public{{Name: "c", From: "cover", To: "c-{w}.webp"}}},
		"public unsafe name": {Name: "k", Uploads: up, Public: []media.Public{{Name: "c", From: "originals/{name}", To: "{name}.webp"}}},
		"zero width":         {Name: "k", Uploads: up, Public: []media.Public{{Name: "c", From: "cover", To: "c-{w}.webp", Widths: []int{0}}}},
		"bad ladder":         {Name: "k", Uploads: up, Private: []media.Private{{Name: "p", From: "cover", To: "h/", HLS: &media.HLS{Ladder: []int{480, 1080}}}}},
		"underscore kind":    {Name: "_k", Uploads: up},
		"to on an upload":    {Name: "k", Uploads: up, Private: []media.Private{{Name: "p", From: "originals/{name}", To: "originals/{name}.webp", Image: &media.Image{}}}},
		"svg to a preset":    {Name: "k", Uploads: []media.Upload{{Path: "cover", Types: []string{"image/svg+xml"}, MaxBytes: 1}}, Public: []media.Public{{Name: "c", From: "cover", To: "c.webp"}}},
		"bmp to a preset":    {Name: "k", Uploads: []media.Upload{{Path: "a/{name}", Types: []string{"image/png", "image/bmp"}, MaxBytes: 1}}, Private: []media.Private{{Name: "p", From: "a/{name}", To: "b/{name}.webp", Image: &media.Image{}}}},
		"widths, lone width": {Name: "k", Uploads: up, Public: []media.Public{{Name: "c", From: "cover", To: "c-{w}.webp", Widths: []int{100}, Image: media.Image{Width: 100}}}},
	} {
		if _, err := media.NewRegistry(media.Config{Namespace: "d", Kinds: []media.Kind{k}}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	svg := media.Kind{Name: "k", Uploads: []media.Upload{{Path: "logo", Types: []string{"image/svg+xml"}, MaxBytes: 1}}}
	if _, err := media.NewRegistry(media.Config{Namespace: "d", Kinds: []media.Kind{svg}}); err != nil {
		t.Errorf("an SVG stored without presets refused: %v", err)
	}
	missing := media.Kind{Name: "k", Uploads: up, Defaults: fstest.MapFS{"other.png": {}},
		Public: []media.Public{{Name: "c", From: "cover", To: "c.webp", Default: "cover.png"}}}
	if _, err := media.NewRegistry(media.Config{Namespace: "d", Kinds: []media.Kind{missing}}); err == nil {
		t.Error("a Default missing from the kind's Defaults accepted")
	}
	missing.Defaults = fstest.MapFS{"cover.png": {}}
	if _, err := media.NewRegistry(media.Config{Namespace: "d", Kinds: []media.Kind{missing}}); err != nil {
		t.Errorf("a kind's own default refused: %v", err)
	}
	if _, err := media.NewRegistry(media.Config{Namespace: "accounts", Kinds: []media.Kind{{Name: "user", Namespace: "accounts", Uploads: up}}}); err == nil {
		t.Error("an app namespace named after a shared one accepted")
	}
	if _, err := media.NewRegistry(media.Config{Namespace: "d", Hooks: media.Hooks{PurgePublic: func(context.Context, []string) {}}}); err == nil {
		t.Error("PurgePublic without BaseURL accepted")
	}
}
