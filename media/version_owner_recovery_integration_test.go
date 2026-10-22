package media_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

func TestWrongParentCannotRecoverVersionWork(t *testing.T) {
	f := newFixtureOn(t, s3test.Open(t), func(c *media.Config) { c.Kinds[0].Versions = true })
	ref, err := f.reg.VersionRef("gallery", cid(1), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	f.visible(1)
	f.put(ref, "originals/001.png", "image/png", png(1))
	item, _ := f.reg.Item(ref)
	lost := errors.New("injected lost manifest response")
	ms := s3test.Manifests(t, &lossyStore{Store: f.env.Store, key: item.ManifestKey(), land: true, err: lost}, f.reg, media.ManifestOptions{Journal: f.journal})
	if _, err := ms.Edit(t.Context(), ref, func(m *media.Manifest) error {
		m.Meta = map[string]any{"title": "Rightful parent"}
		return nil
	}); !errors.Is(err, lost) {
		t.Fatalf("failed to create uncertain rightful write: %v", err)
	}
	commits := pgx.Identifier{f.env.ContentSchema(), "content_media_commits"}.Sanitize()
	pending := func() string {
		t.Helper()
		var row string
		if err := f.env.Pool().QueryRow(t.Context(), `SELECT row_to_json(c)::text FROM `+commits+` c WHERE folder_prefix=$1 AND state='prepared'`, item.Prefix()).Scan(&row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	before := pending()
	head, err := f.env.Store.Head(t.Context(), item.ManifestKey())
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := f.reg.VersionRef("gallery", cid(3), cid(2))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.ms.Recover(t.Context(), wrong); !errors.Is(err, media.ErrCommitIdentity) {
		t.Fatalf("wrong parent recovered another gallery's pending version: %v", err)
	}
	if _, err := f.ms.Edit(t.Context(), wrong, func(m *media.Manifest) error {
		m.Meta = map[string]any{"title": "Wrong parent"}
		return nil
	}); !errors.Is(err, media.ErrCommitIdentity) {
		t.Fatalf("wrong parent edited another gallery's pending version: %v", err)
	}
	if after := pending(); after != before {
		t.Fatal("denied wrong-parent request changed the rightful journal attempt")
	}
	after, err := f.env.Store.Head(t.Context(), item.ManifestKey())
	if err != nil || after.ETag != head.ETag {
		t.Fatalf("denied wrong-parent request changed stored manifest: %v", err)
	}
	if err := f.ms.Recover(t.Context(), ref); err != nil {
		t.Fatalf("rightful owner could not resume: %v", err)
	}
	current, _, err := f.ms.Get(t.Context(), ref)
	if err != nil || current.Meta["title"] != "Rightful parent" {
		t.Fatalf("rightful pending write was lost: %+v %v", current, err)
	}
}
