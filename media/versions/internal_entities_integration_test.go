package versions_test

import (
	"errors"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/media"
)

func TestInternalEntityMediaRequiresExplicitStaffEditorRead(t *testing.T) {
	s := newSite(t, siteOptions{})
	store, err := entities.New(entities.Options{Pool: s.pool, Schema: s.schema, Tenant: s.tenant, Kinds: siteKinds})
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateEntities(s.ctx, []entities.EntityInput{{Kind: "tag", Slug: "internal-marker", Internal: true}})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.reg.Ref("tag", string(created[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range []access.Actor{{Anonymous: true, IP: "203.0.113.3"}, s.stranger, s.staff} {
		if _, err := s.reader.Grant(s.ctx, ref, actor); !errors.Is(err, media.ErrNotVisible) {
			t.Fatalf("ordinary media read for %+v: %v", actor, err)
		}
	}
	if _, err := s.reader.Read(s.ctx, ref, s.stranger, media.ReadOptions{Editor: true}); !errors.Is(err, media.ErrNotVisible) {
		t.Fatalf("reader selected editor mode: %v", err)
	}
	if _, err := s.reader.Read(s.ctx, ref, testauth.Actor(s.staff, s.grants), media.ReadOptions{Editor: true}); err != nil {
		t.Fatalf("staff explicit editor read: %v", err)
	}
	if _, status := s.httpUpload(s.staff, media.RefBody{Kind: "tag", ID: string(created[0].ID)}, "cover/internal.png", "image/png", pngOf(t, 32, 32, 7)); status != 200 {
		t.Fatalf("staff internal cover upload: %d", status)
	}
	view, err := s.reader.Read(s.ctx, ref, testauth.Actor(s.staff, s.grants), media.ReadOptions{Editor: true})
	if err != nil || len(view.Files) != 1 || !view.Files[0].Upload {
		t.Fatalf("staff could not read its internal entity upload: %+v, %v", view, err)
	}
}
