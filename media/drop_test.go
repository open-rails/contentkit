package media

import "testing"

// A body image's source outlives its rendition: new versions copy it and
// editors view it. Another upload feeding only private presets is dropped.
func TestBodyImageSourcesAreKept(t *testing.T) {
	pngs := []string{"image/png"}
	reg, err := NewRegistry(Config{Namespace: "site", Kinds: []Kind{{Name: "post", Versions: true,
		Uploads: []Upload{{Path: "inline/{name}", Body: true, Named: true, Max: 10, Types: pngs, MaxBytes: 1 << 20},
			{Path: "files/{name}", Types: pngs, MaxBytes: 1 << 20}},
		Private: []Private{{Name: "inline", From: "inline/{name}", To: "shown/{name}.webp", Image: &Image{Width: 64}},
			{Name: "small", From: "files/{name}", To: "small/{name}.webp", Image: &Image{Width: 64}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	k, _ := reg.Kind("post")
	blob := "sha256-" + "00000000000000000000000000000000000000000000000000000000000000aa-0192f000-0000-7000-8000-000000000001"
	body := File{Path: "inline/i-0192f000-0000-7000-8000-000000000002.png", Type: "image/png", Blob: blob}
	other := File{Path: "files/a.png", Type: "image/png", Blob: blob}
	if k.droppable(&Manifest{}, body) || !k.droppable(&Manifest{}, other) {
		t.Fatalf("droppable: body %v, other %v", k.droppable(&Manifest{}, body), k.droppable(&Manifest{}, other))
	}
}
