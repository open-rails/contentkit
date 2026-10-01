package media

import (
	"slices"
	"strings"
	"testing"
)

func TestPaths(t *testing.T) {
	for in, want := range map[string]string{
		"  Page 1  ":               "Page 1",
		"a/b\\c":                   "abc",
		"..evil..":                 "evil",
		"tab\there":                "tabhere",
		"é":                       "é", // NFC
		strings.Repeat("é", 150):   strings.Repeat("é", 100),
		strings.Repeat("<", 300):   strings.Repeat("<", 200), // unescaped in the manifest
		strings.Repeat(`"`, 300):   strings.Repeat(`"`, 100), // two bytes each in JSON
		strings.Repeat("x", 8<<20): strings.Repeat("x", 200), // linear in the input
	} {
		if got := CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string][2]string{
		"originals/001.PNG":  {"originals/001", "png"},
		"cover":              {"cover", ""},
		"a.b/c":              {"a.b/c", ""},
		"page.01.jpg":        {"page.01", "jpg"},
		"x.toolongextension": {"x.toolongextension", ""},
		".hidden":            {".hidden", ""},
	} {
		if s, e := splitExt(in); s != want[0] || e != want[1] {
			t.Errorf("splitExt(%q) = %q %q", in, s, e)
		}
	}
	names := []string{"10.png", "2.png", "1.png", "a10", "a2", "A1", "b"}
	slices.SortFunc(names, func(a, b string) int {
		if natLess(a, b) {
			return -1
		}
		if natLess(b, a) {
			return 1
		}
		return 0
	})
	if want := []string{"1.png", "2.png", "10.png", "A1", "a2", "a10", "b"}; !slices.Equal(names, want) {
		t.Fatalf("natural order %v", names)
	}
	if got := fill("{title} ({w}p)-{missing}", map[string]string{"title": "T", "w": "1080"}); got != "T (1080p)-" {
		t.Fatalf("fill %q", got)
	}
	if got := typeExt("image/jpeg") + typeExt("image/png") + typeExt("application/x-subrip"); got != "jpgpngsrt" {
		t.Fatalf("typeExt %q", got)
	}
}

// Normalize orders each Upload's files as they are (attached first), then
// each preset's outputs in their uploads' order.
func TestNormalizeOrder(t *testing.T) {
	r, err := NewRegistry(Config{Namespace: "d", Kinds: []Kind{{Name: "g",
		Uploads: []Upload{{Path: "p/{name}", Types: []string{"image/png"}, MaxBytes: 1, Pages: true}, {Path: "cover", Types: []string{"image/png"}, MaxBytes: 1}},
		Private: []Private{{Name: "t", From: "p/{name}", To: "t/{name}.webp", Image: &Image{}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	k, _ := r.Kind("g")
	b := "sha256-" + strings.Repeat("0", 64)
	m := &Manifest{Files: []File{
		{Path: "t/2.webp", Blob: b, From: "p/2.png", Preset: "t"},
		{Path: "cover.png", Blob: b},
		{Path: "p/9.png", Blob: b, Unattached: true},
		{Path: "p/2.png", Blob: b},
		{Path: "t/1.webp", Blob: b, From: "p/1.png", Preset: "t"},
		{Path: "p/1.png", Blob: b},
		{Path: "unknown.bin", Blob: b},
	}}
	k.Normalize(m)
	var got []string
	for _, f := range m.Files {
		got = append(got, f.Path)
	}
	if want := []string{"p/2.png", "p/1.png", "p/9.png", "cover.png", "t/2.webp", "t/1.webp", "unknown.bin"}; !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
}
