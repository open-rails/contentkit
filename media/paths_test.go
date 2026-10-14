package media

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
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
		Uploads: []Upload{{Path: "p/{name}", Types: []string{"image/png"}, MaxBytes: 1}, {Path: "cover", Types: []string{"image/png"}, MaxBytes: 1}},
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

// A preview (Public.First) is named by position among its Upload's attached
// files; it is pending exactly where a position's file changed.
func TestPreviews(t *testing.T) {
	uploads := []Upload{{Path: "p/{name}", Types: []string{"image/png"}, MaxBytes: 1}, {Path: "cover", Types: []string{"image/png"}, MaxBytes: 1}}
	r, err := NewRegistry(Config{Namespace: "d", Kinds: []Kind{{Name: "g", Uploads: uploads,
		Public: []Public{{Name: "preview", From: "p/{name}", To: "preview-{n}-{w}.webp", First: 2, Widths: []int{100, 200}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	k, _ := r.Kind("g")
	p := &k.Public[0]
	blob := func(c string) string { return "sha256-" + strings.Repeat(c, 64) }
	m := &Manifest{Files: []File{
		{Path: "p/new.png", Blob: blob("9"), Unattached: true, Pending: []string{"preview"}},
		{Path: "p/a.png", Blob: blob("a")},
		{Path: "cover.png", Blob: blob("c")},
		{Path: "p/b.png", Blob: blob("b"), Pending: []string{"preview"}},
		{Path: "p/c.png", Blob: blob("d"), Pending: []string{"preview"}},
	}}
	if got := k.PublicNames(m, p, "p/b.png"); !slices.Equal(got, []string{"preview-2-100.webp", "preview-2-200.webp"}) {
		t.Fatalf("names of the second page %v", got)
	}
	for _, path := range []string{"p/c.png", "p/new.png", "cover.png"} {
		if got := k.PublicNames(m, p, path); got != nil {
			t.Fatalf("%s is no preview: %v", path, got)
		}
	}
	pending := func(m *Manifest) string {
		var out []string
		for _, f := range m.Files {
			if slices.Contains(f.Pending, "preview") {
				out = append(out, f.Path)
			}
		}
		return strings.Join(out, " ")
	}
	// A new item: the first two attached pages, whatever a put set.
	k.syncPreviews(nil, m)
	if got := pending(m); got != "p/a.png p/b.png" {
		t.Fatalf("pending on a new item: %q", got)
	}
	if got := k.previewNames(m); got != nil || k.PublicKept(m) != nil {
		t.Fatalf("previews listed or kept before they are rendered: %v", got)
	}
	rendered := m.Clone()
	for i := range rendered.Files {
		rendered.Files[i].Pending = nil
		f := rendered.Files[i]
		if names := k.PublicNames(rendered, p, f.Path); len(names) > 0 {
			dims := make([]Dims, len(names))
			for j := range dims {
				dims[j] = Dims{W: p.Widths[j], H: p.Widths[j]}
			}
			rendered.SetPublication(f.Path, Publication{Preset: p.Name, Source: f.Key(), FP: "test",
				Generation: uuid.NewString(), Names: names, Dims: dims, State: PublicationReady})
		}
	}
	var expected []string
	for _, f := range rendered.Files {
		for _, pub := range f.Public {
			expected = append(expected, pub.NamesOnDisk()...)
		}
	}
	if got := k.previewNames(rendered); !slices.Equal(got, expected) ||
		!slices.Equal(k.PublicKept(rendered), got) {
		t.Fatalf("previews %v, kept %v", got, k.PublicKept(rendered))
	}
	// Nothing moved: nothing pending. A swap: both positions. A removal:
	// the page that moved up, and the one that entered.
	same := rendered.Clone()
	k.syncPreviews(rendered, same)
	if got := pending(same); got != "" {
		t.Fatalf("pending without a change: %q", got)
	}
	swapped := rendered.Clone()
	swapped.Files[1], swapped.Files[3] = swapped.Files[3], swapped.Files[1]
	k.syncPreviews(rendered, swapped)
	if got := pending(swapped); got != "p/b.png p/a.png" {
		t.Fatalf("pending after a swap: %q", got)
	}
	removed := rendered.Clone()
	removed.Files = slices.Delete(removed.Files, 1, 2)
	k.syncPreviews(rendered, removed)
	if got := pending(removed); got != "p/b.png p/c.png" || k.PublicKept(removed) != nil {
		t.Fatalf("pending after a removal: %q; the removed page's image is kept: %v", got, k.PublicKept(removed))
	}
	edited := rendered.Clone()
	edited.Files[3].Edit = &Edit{Rotate: 90}
	k.syncPreviews(rendered, edited)
	if got := pending(edited); got != "p/b.png" {
		t.Fatalf("pending after an edit: %q", got)
	}
	hidden := m.Clone()
	hidden.Hidden = true
	k.syncPreviews(nil, hidden)
	if got := pending(hidden) + strings.Join(k.previewNames(hidden), ""); got != "" {
		t.Fatalf("a hidden item's previews: %q", got)
	}
	for name, pub := range map[string]Public{
		"a literal upload":  {Name: "x", From: "cover", To: "x-{n}.webp", First: 1},
		"no {n}":            {Name: "x", From: "p/{name}", To: "x.webp", First: 1},
		"{n} without First": {Name: "x", From: "cover", To: "x-{n}.webp"},
		"{name}":            {Name: "x", From: "p/{name}", To: "{name}-{n}.webp", First: 1},
		"too many":          {Name: "x", From: "p/{name}", To: "x-{n}.webp", First: maxFirst + 1},
		"a default":         {Name: "x", From: "p/{name}", To: "x-{n}.webp", First: 1, Default: "x.png"},
	} {
		if _, err := NewRegistry(Config{Namespace: "d", Kinds: []Kind{{Name: "g", Uploads: uploads, Public: []Public{pub}}}}); err == nil {
			t.Errorf("accepted a preview with %s", name)
		}
	}
}
