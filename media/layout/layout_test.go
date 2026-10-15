package layout_test

import (
	"crypto/sha256"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/media/layout"
)

func TestParse(t *testing.T) {
	sum := sha256.Sum256(nil)
	hash := layout.SHA256Name(sum[:])
	blob := layout.BlobName(sum[:], "0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b")
	public := strings.Repeat("c", 123) + "-0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b.webp"
	for key, want := range map[string]layout.Key{
		"d/gallery/1/manifest.json":         {Namespace: "d", Kind: "gallery", ID: "1", Area: layout.AreaManifest},
		"d/gallery/1/private/" + blob:       {Namespace: "d", Kind: "gallery", ID: "1", Area: layout.AreaPrivate, Name: blob},
		"d/gallery/1/private/" + hash:       {Namespace: "d", Kind: "gallery", ID: "1", Area: layout.AreaPrivate, Name: hash},
		"accounts/user/42/public/a-64.webp": {Namespace: "accounts", Kind: "user", ID: "42", Area: layout.AreaPublic, Name: "a-64.webp"},
		"d/gallery/_default/public/c.webp":  {Namespace: "d", Kind: "gallery", ID: "_default", Area: layout.AreaPublic, Name: "c.webp"},
		"d/gallery/1/temp/u-1":              {Namespace: "d", Kind: "gallery", ID: "1", Area: layout.AreaTemp, Name: "u-1"},
		"d/gallery/1/public/" + public:      {Namespace: "d", Kind: "gallery", ID: "1", Area: layout.AreaPublic, Name: public},
		"d/gallery/1/v/manifest.json":       {Namespace: "d", Kind: "gallery", ID: "1", Version: "v", Area: layout.AreaManifest},
		"d/gallery/1/v/private/" + blob:     {Namespace: "d", Kind: "gallery", ID: "1", Version: "v", Area: layout.AreaPrivate, Name: blob},
		"d/gallery/1/v/download/" + blob:    {Namespace: "d", Kind: "gallery", ID: "1", Version: "v", Area: layout.AreaDownload, Name: blob},
		"d/gallery/1/v/public/c.webp":       {Namespace: "d", Kind: "gallery", ID: "1", Version: "v", Area: layout.AreaPublic, Name: "c.webp"},
		"d/gallery/1/v/temp/u-1":            {Namespace: "d", Kind: "gallery", ID: "1", Version: "v", Area: layout.AreaTemp, Name: "u-1"},
	} {
		got, ok := layout.Parse(key)
		if !ok || got != want {
			t.Errorf("%s: %+v %v", key, got, ok)
		}
	}
	for _, bad := range []string{"d/gallery/1", "d/gallery/1/private/x", "d/gallery/1/private/" + hash + "-", "d/gallery/1/private/" + blob + "/x",
		"d/gallery/1/originals/" + hash, "d/../1/manifest.json", "d/gallery/1/other/x", "d/gallery/1/public/.x",
		"d/gallery/1/public/a b", "d/gallery/1/private/SHA256-x", "d/gallery/1/public/" + strings.Repeat("c", 166),
		"d/gallery/1/temp/" + public, "d/gallery/1/private/" + public, "d/gallery/1/v/w/manifest.json", "d/gallery/1/_v/manifest.json",
		"d/gallery/1/public/private/" + blob, "d/gallery/1/v/private/x", "d/gallery/1/v", "d/gallery/1/v/",
		"d/gallery/1/download/" + hash, "d/gallery/1/download/download/" + blob, "d/gallery/1/download/manifest.json"} {
		if _, ok := layout.Parse(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
	for prefix, ok := range map[string]bool{"d/gallery/1/": true, "d/gallery/1/v/": true, "d/gallery/1": false,
		"d/gallery/1/private/": false, "d/gallery/1/v/w/": false} {
		if k, got := layout.ParseFolder(prefix); got != ok || ok && k.Folder() != prefix {
			t.Errorf("ParseFolder(%q) = %+v %v", prefix, k, got)
		}
	}
	for ref, want := range map[string][2]string{blob: {layout.AreaPrivate, blob}, hash: {layout.AreaPrivate, hash},
		"download/" + blob: {layout.AreaDownload, blob}, "download/" + hash: {}, "private/" + blob: {}, "download/x": {}} {
		area, name, ok := layout.SplitBlobRef(ref)
		if ok != (want[0] != "") || ok && (area != want[0] || name != want[1] || layout.BlobRef(area, name) != ref) {
			t.Errorf("SplitBlobRef(%q) = %q %q %v", ref, area, name, ok)
		}
	}
	if layout.ValidBlobName("download/" + blob) {
		t.Error("a reference passed as a name")
	}
	if _, ok := layout.ParseSHA256Name(hash[:len(hash)-1] + "A"); ok {
		t.Error("upper-case hex accepted")
	}
}

func TestValidStagedName(t *testing.T) {
	if !layout.ValidStagedName("u-0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b") {
		t.Error("a staged name refused")
	}
	for _, bad := range []string{"u-1", "u-0192A3B4-C5D6-7E8F-9A0B-1C2D3E4F5A6B", "i-0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b",
		"u-0192a3b4c5d67e8f9a0b1c2d3e4f5a6b", "u-0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6g", "sha256-00"} {
		if layout.ValidStagedName(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestDisposition(t *testing.T) {
	const plain = "attachment"
	for _, c := range []struct{ name, typ, want string }{
		{"[A] Title.zip", "application/zip", `attachment; filename="[A] Title.zip"; filename*=UTF-8''%5BA%5D%20Title.zip`},
		{"[A] 日本語 \"x\".zip", "application/zip", `attachment; filename="[A] ___ _x_.zip"; filename*=UTF-8''%5BA%5D%20%E6%97%A5%E6%9C%AC%E8%AA%9E%20%22x%22.zip`},
		{"Page.JPG", "image/jpeg; charset=binary", `attachment; filename="Page.JPG"; filename*=UTF-8''Page.JPG`},
		{"setup.exe", "application/zip", plain},
		{"pages.zip.exe", "application/zip", plain},
		{"pages.zip", "image/webp", plain},
		{"pages.zip", "application/octet-stream", plain},
		{"pages.zip", "", plain},
		{"pages", "application/zip", plain},
		{".zip", "application/zip", plain},
		{"", "application/zip", plain},
		{"a/b.zip", "application/zip", plain},
		{`a\b.zip`, "application/zip", plain},
		{"a\r\nSet-Cookie: x.zip", "application/zip", plain},
		{"a\xffb.zip", "application/zip", plain},
		{"invoice\u202efdp.zip", "application/zip", plain}, // displays as invoicepiz.pdf
		{"a\u200eb.zip", "application/zip", plain},
		{"a\u200fb.zip", "application/zip", plain},
		{"a\u202ab.zip", "application/zip", plain},
		{"a\u2066b.zip", "application/zip", plain},
		{"a\u2069b.zip", "application/zip", plain},
		{"a\u2065b.zip", "application/zip", `attachment; filename="a_b.zip"; filename*=UTF-8''a%E2%81%A5b.zip`},
		{strings.Repeat("a", layout.MaxDownloadName) + ".zip", "application/zip", plain},
	} {
		if got := layout.Disposition(c.name, c.typ); got != c.want {
			t.Errorf("Disposition(%q, %q) = %q, want %q", c.name, c.typ, got, c.want)
		}
	}
}

func TestDownloadName(t *testing.T) {
	long := strings.Repeat("日", layout.MaxDownloadName)
	for _, c := range []struct{ name, ext, want string }{
		{"[A (B)] Title (Naruto) (English)", ".zip", "[A (B)] Title (Naruto) (English).zip"},
		{" ..\u202e.hidden/ name\r\n ", ".zip", "hidden name.zip"},
		{"a\xffb", ".mp4", "ab.mp4"},
		{"...", ".zip", ""},
		{"", ".zip", ""},
		{long, ".zip", strings.Repeat("日", (layout.MaxDownloadName-4)/3) + ".zip"},
	} {
		got := layout.DownloadName(c.name, c.ext)
		if got != c.want {
			t.Errorf("DownloadName(%q, %q) = %q, want %q", c.name, c.ext, got, c.want)
		}
		if got != "" && layout.Disposition(got, "application/zip") == "attachment" && c.ext == ".zip" {
			t.Errorf("Disposition refuses %q", got)
		}
	}
}
