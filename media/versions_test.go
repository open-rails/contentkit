package media_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
)

func bookKind() media.Kind {
	pngs := []string{"image/png"}
	return media.Kind{Name: "book", KeepOriginals: true,
		Publish: &media.Publish{Pages: "pages/{name}", Renditions: []string{"read", "thumb"}, Previews: "preview", Playback: "listen",
			Import: "import/{name}", Download: "zip"},
		Uploads: []media.Upload{
			{Path: "cover", Types: pngs, MaxBytes: 1 << 20},
			{Path: "pages/{name}", Types: pngs, MaxBytes: 1 << 20},
			{Path: "audio/{name}", Types: []string{"audio/wav"}, MaxBytes: 1 << 20},
			{Path: "import/{name}", Types: []string{"application/zip"}, MaxBytes: 1 << 20},
			{Path: "previews/{name}", Types: []string{"image/webp"}, MaxBytes: 1 << 20, Max: 2},
		},
		Private: []media.Private{
			{Name: "read", From: "pages/{name}", To: "read/{name}.webp", Image: &media.Image{Width: 100}},
			{Name: "thumb", From: "pages/{name}", To: "thumb/{name}.webp", Image: &media.Image{Width: 10}},
			{Name: "high", From: "pages/{name}", To: "high/{name}.webp", Image: &media.Image{}},
			{Name: "zip", To: "download/pages.zip", Zip: "high/", HostOnly: true},
			{Name: "listen", From: "audio/{name}", To: "listen/{name}/", Audio: &media.Audio{}},
		},
		Public: []media.Public{
			{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{100}},
			{Name: "preview", From: "previews/{name}", To: "preview-{n}.webp", First: 2},
		}}
}

func TestVersionsKind(t *testing.T) {
	site := kinds.Config{Languages: []string{"en"}, Kinds: map[string]kinds.Kind{"book": {Route: "b", Presentation: "reader",
		Versions: kinds.VersionRules{Languages: "one"}}}, GroupKinds: map[string]kinds.GroupKind{"tag": {}, "series": {},
		"channel": {Owner: "user"}}}
	book := bookKind()
	cfg, err := media.ForContent(media.Config{Namespace: "site", Kinds: []media.Kind{book, {Name: "tag",
		Uploads: []media.Upload{{Path: "cover", Types: []string{"image/png"}, MaxBytes: 1}}}}}, site)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := media.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Every group kind has media; a declared one is kept as declared.
	for name, uploads := range map[string]string{"tag": "cover", "series": "cover/{name}", "channel": "cover/{name} avatar/{name}"} {
		k, err := reg.Kind(name)
		var paths []string
		for _, u := range k.Uploads {
			paths = append(paths, u.Path)
		}
		if err != nil || strings.Join(paths, " ") != uploads || k.Versions {
			t.Fatalf("group kind %s: %v %v", name, paths, err)
		}
	}
	content, version := cid(1), cid(2)
	// A content has no item of its own: every file lives in a version's.
	if _, err := reg.Ref("book", content); err == nil {
		t.Fatal("a content-level item")
	}
	ver, err := reg.VersionRef("book", content, version)
	if err != nil {
		t.Fatal(err)
	}
	vi, _ := reg.Item(ver)
	if vi.Prefix() != "site/book/"+content+"/"+version+"/" {
		t.Fatalf("folder %s", vi.Prefix())
	}
	// Body images live in a version's private/: a Named upload with a private
	// image preset and no public one, of a kind that publishes nothing.
	pngs := []string{"image/png"}
	post := func() media.Kind {
		return media.Kind{Name: "post", Versions: true, Uploads: []media.Upload{{Path: "inline/{name}", Body: true, Named: true, Max: 10, Types: pngs, MaxBytes: 1 << 20}},
			Private: []media.Private{{Name: "inline", From: "inline/{name}", To: "inline/{name}.webp", Image: &media.Image{Width: 800}}}}
	}
	posts, err := media.NewRegistry(media.Config{Namespace: "site", Kinds: []media.Kind{post()}})
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := posts.Kind("post"); !k.IsBody("inline/i-01920000-0000-7000-8000-000000000001.png") || k.IsBody("cover.png") || len(posts.Kinds()) != 1 {
		t.Fatalf("body kind %+v", k)
	}
	for name, mutate := range map[string]func(*media.Kind){
		"not named":        func(k *media.Kind) { k.Uploads[0].Named = false },
		"no private image": func(k *media.Kind) { k.Private = nil },
		"a public preset": func(k *media.Kind) {
			k.Public = []media.Public{{Name: "inline", From: "inline/{name}", To: "{name}.webp"}}
		},
		"a publishing kind": func(k *media.Kind) {
			b := bookKind()
			b.Uploads, b.Private = append(b.Uploads, k.Uploads...), append(b.Private, k.Private...)
			*k = b
			k.Versions = true
		},
	} {
		k := post()
		mutate(&k)
		if _, err := media.NewRegistry(media.Config{Namespace: "site", Kinds: []media.Kind{k}}); err == nil {
			t.Errorf("body image with %s: accepted", name)
		}
	}
	if len(vi.Kind().Uploads) != 5 || vi.Kind().Uploads[0].Path != "cover" || len(vi.Kind().Public) != 2 || vi.Kind().Public[1].Name != "preview" {
		t.Fatalf("version uploads %+v", vi.Kind().Uploads)
	}
	if got := reg.PublicURL(ver, "preview-1.webp"); got != "/v1/site/book/"+content+"/"+version+"/public/preview-1.webp" {
		t.Fatalf("version URL %s", got)
	}
	for _, bad := range []contentref.ContentRef{
		contentref.NewVersion("site", "book", content, "v1"),
		contentref.NewVersion("site", "tag", content, version),
	} {
		if _, err := reg.Item(bad); err == nil {
			t.Fatalf("%s took", bad)
		}
	}
	// The published kind: each rendition's and the audio's files, served whole.
	pub, err := reg.Kind("book" + media.PublishedSuffix)
	if err != nil || pub.Of() == nil || pub.Of().Name != "book" || !pub.ServeOriginals {
		t.Fatalf("published kind %+v %v", pub, err)
	}
	var paths []string
	for _, u := range pub.Uploads {
		paths = append(paths, u.Path+"="+strings.Join(u.Types, ","))
	}
	if strings.Join(paths, " ") != "read/{name}=image/webp thumb/{name}=image/webp listen/{name}=audio/mp4" {
		t.Fatalf("published uploads %v", paths)
	}
	// The worker's registry derives them again.
	b, err := json.Marshal(reg)
	if err != nil || strings.Contains(string(b), media.PublishedSuffix) {
		t.Fatalf("registry JSON carries derived kinds: %v", err)
	}
	parsed, err := media.ParseConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := media.NewRegistry(parsed); err != nil || len(again.Kinds()) != len(reg.Kinds()) {
		t.Fatalf("reparsed registry: %v", err)
	}

	for name, mutate := range map[string]func(*media.Kind){
		"versions of no content kind": func(k *media.Kind) { k.Name = "other" },
		"content without versions":    func(k *media.Kind) { k.Publish = nil; k.Versions = false; k.Name = "tag" },
		"pages no upload":             func(k *media.Kind) { k.Publish.Pages = "cover" },
		"rendition not an image":      func(k *media.Kind) { k.Publish.Renditions = []string{"zip"} },
		"previews not a preview":      func(k *media.Kind) { k.Publish.Previews = "cover" },
		"playback not audio":          func(k *media.Kind) { k.Publish.Playback = "read" },
		"import not a zip":            func(k *media.Kind) { k.Publish.Import = "pages/{name}" },
		"download not host only zip":  func(k *media.Kind) { k.Private[3].HostOnly = false },
		"pages as body images":        func(k *media.Kind) { k.Uploads[1].Body = true },
	} {
		k := bookKind()
		mutate(&k)
		if name == "versions of no content kind" || name == "content without versions" {
			if _, err := media.ForContent(media.Config{Namespace: "site", Kinds: []media.Kind{func() media.Kind { k.Versions = true; return k }()}}, site); err == nil {
				t.Errorf("%s: accepted", name)
			}
			continue
		}
		k.Versions = true
		if _, err := media.NewRegistry(media.Config{Namespace: "site", Kinds: []media.Kind{k}}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
