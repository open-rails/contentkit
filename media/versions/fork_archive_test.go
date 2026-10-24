package versions

import (
	"testing"

	"github.com/open-rails/contentkit/media"
)

func TestForkSkipsGenericArchiveSources(t *testing.T) {
	registry, err := media.NewRegistry(media.Config{Namespace: "site", Kinds: []media.Kind{{Name: "book", Versions: true, Uploads: []media.Upload{
		{Path: "zip/{name}", ArchiveTo: "body/{name}", Types: []string{"application/zip"}, MaxBytes: 1 << 20},
		{Path: "body/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	kind, err := registry.Kind("book")
	if err != nil {
		t.Fatal(err)
	}
	manifest := &media.Manifest{Files: []media.File{
		{Path: "zip/book.zip", Blob: "zip", Admission: &media.SourceAdmission{Owner: "source-owner", Enforce: true}},
		{Path: "body/page.png", Blob: "image"},
	}}
	_, files, _, err := forkSource(kind, manifest, false)
	if err != nil || len(files) != 1 || files[0].Path != "body/page.png" {
		t.Fatalf("fork copied a ZIP that would be expanded again: %+v / %v", files, err)
	}
}
