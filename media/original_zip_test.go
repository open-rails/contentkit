package media

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestSourceFormatOutputPaths(t *testing.T) {
	reg, err := NewRegistry(Config{Namespace: "test", Kinds: []Kind{{Name: "gallery", KeepOriginals: true,
		Uploads: []Upload{{Path: "originals/{name}", Types: []string{"image/png", "image/jpeg"}, MaxBytes: 1 << 20}},
		Private: []Private{{Name: "download", From: "originals/{name}", To: "download/{name}{ext}", Image: &Image{Format: ImageFormatSource, Width: 6000, Height: 6000}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(encoded)
	if err != nil {
		t.Fatal(err)
	}
	reg, err = NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := reg.Kind("gallery")
	if k.Private[0].Image.Format != ImageFormatSource {
		t.Fatal("worker registry JSON lost the source-format selection")
	}
	for _, source := range []string{"originals/001.png", "originals/002.jpg"} {
		got := k.OutputPath(&k.Private[0], source)
		if got != "download/"+source[len("originals/"):] {
			t.Fatalf("lost original extension: %q -> %q", source, got)
		}
	}
}

func TestSourceFormatRegistryRefusesInvalidFormatsAndPaths(t *testing.T) {
	for _, tt := range []struct {
		name   string
		to     string
		format ImageFormat
		public bool
		editor bool
	}{
		{name: "unknown format", to: "download/{name}{ext}", format: "jpeg"},
		{name: "missing extension", to: "download/{name}.webp", format: ImageFormatSource},
		{name: "misleading extension", to: "download/{name}{ext}.webp", format: ImageFormatSource},
		{name: "WebP extension placeholder", to: "download/{name}{ext}"},
		{name: "public source format", to: "cover.webp", format: ImageFormatSource, public: true},
		{name: "editor source format", format: ImageFormatSource, editor: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Namespace: "test", Kinds: []Kind{{Name: "gallery",
				Uploads: []Upload{{Path: "originals/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20}},
			}}}
			spec := Image{Format: tt.format}
			switch {
			case tt.public:
				cfg.Kinds[0].Public = []Public{{Name: "cover", From: "originals/{name}", To: tt.to, Image: spec}}
			case tt.editor:
				cfg.Editor = spec
			default:
				cfg.Kinds[0].Private = []Private{{Name: "source", From: "originals/{name}", To: tt.to, Image: &spec}}
			}
			if _, err := NewRegistry(cfg); err == nil {
				t.Fatal("invalid source-format registry accepted")
			}
		})
	}
}

func TestRemovingSourceInvalidatesOriginalArchive(t *testing.T) {
	k := &Kind{Private: []Private{{Name: "zip", Zip: "originals/"}, {Name: "other", Zip: "covers/"}}}
	m := &Manifest{Files: []File{
		{Path: "originals/001.png", Blob: "source"},
		{Path: "download/pages.zip", From: "originals/", Preset: "zip", Blob: "old-zip"},
		{Path: "download/covers.zip", From: "covers/", Preset: "other", Blob: "cover-zip"},
	}}
	run := opRun{k: k, m: m}
	run.dropZips("originals/001.png")
	if m.Find("download/pages.zip") >= 0 || m.Find("originals/001.png") < 0 || m.Find("download/covers.zip") < 0 {
		t.Fatal("source archive invalidation removed unrelated files or retained the old ZIP")
	}
}

func TestRemovedPresetDoesNotLeavePendingWork(t *testing.T) {
	reg, err := NewRegistry(Config{Namespace: "test", Kinds: []Kind{{Name: "gallery", KeepOriginals: true,
		Uploads: []Upload{{Path: "originals/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20}},
		Private: []Private{{Name: "source", From: "originals/{name}", To: "download-pages/{name}{ext}", Image: &Image{Format: ImageFormatSource}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	k, _ := reg.Kind("gallery")
	m := &Manifest{Files: []File{{Path: "originals/001.png", Blob: "original", Pending: []string{"high", "source"}}}}
	k.Normalize(m)
	if !slices.Equal(m.Files[0].Pending, []string{"source"}) || m.Files[0].Blob != "original" || m.Files[0].Gone {
		t.Fatalf("obsolete work not cleared without deleting the original: %+v", m.Files[0])
	}
}
