package versions

import (
	"errors"
	"reflect"
	"testing"

	"github.com/open-rails/contentkit/media"
)

func TestZipPagesUsesEffectivePageSelection(t *testing.T) {
	reg, err := media.NewRegistry(media.Config{Namespace: "test", Kinds: []media.Kind{{
		Name: "gallery", Versions: true, KeepOriginals: true,
		Publish: &media.Publish{Pages: "originals/{name}", Renditions: []string{"high"}},
		Uploads: []media.Upload{{Path: "originals/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20}},
		Private: []media.Private{
			{Name: "high", From: "originals/{name}", To: "high/{name}.webp", Image: &media.Image{}},
			{Name: "zip", To: "download/pages.zip", Zip: "high/"},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	k, err := reg.Kind("gallery")
	if err != nil {
		t.Fatal(err)
	}
	z := &k.Private[1]
	for _, tc := range []struct {
		name         string
		includeFirst bool
		firstReady   bool
		want         []string
		pending      bool
	}{
		{name: "excluded ready page", firstReady: true, want: []string{"high/002.webp"}},
		{name: "excluded pending page", want: []string{"high/002.webp"}},
		{name: "restored page follows source order", includeFirst: true, firstReady: true, want: []string{"high/001.webp", "high/002.webp"}},
		{name: "included pending page still waits", includeFirst: true, pending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := media.File{Path: "originals/001.png", Type: "image/png", Meta: map[string]any{"page": map[string]any{"included": tc.includeFirst}}}
			if tc.firstReady {
				first.Blob = "first-original"
			} else {
				first.Pending = []string{"high"}
			}
			m := &media.Manifest{Files: []media.File{first, {Path: "originals/002.png", Type: "image/png", Blob: "second-original"},
				{Path: "high/001.webp", From: first.Path, Preset: "high", Blob: "first-output"},
				{Path: "high/002.webp", From: "originals/002.png", Preset: "high", Blob: "second-output"}}}
			pages, err := zipPages(k, z, m)
			if tc.pending {
				if !errors.Is(err, ErrNotReady) {
					t.Fatalf("included pending source: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, page := range pages {
				got = append(got, page.Path)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("download pages %v, want %v", got, tc.want)
			}
		})
	}
}
