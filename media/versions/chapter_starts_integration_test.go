package versions_test

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

func sourcePageID(t *testing.T, s *site, ref contentref.ContentRef, path string) string {
	t.Helper()
	k, err := s.reg.Kind(ref.ContentKind)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := s.manifest(ref).Get(path)
	if !ok || k.PageID(f) == "" {
		t.Fatalf("missing source hash for %s", path)
	}
	return k.PageID(f)
}

func TestAnchoredPlanWithoutPublicationIsPending(t *testing.T) {
	s := newSite(t, siteOptions{})
	c := s.content("gallery", `{"level":"public"}`)
	v := s.version(c)
	// A withdrawn publication leaves valid stored chapter starts while its
	// replacement is being built. A missing snapshot means pending, not an empty
	// gallery whose chapter markers need repair.
	s.exec(`INSERT INTO `+s.table("content_segments")+` (tenant_id,version_id,position,start_page)
 VALUES ($1,$2,0,$3)`, s.tenant, v.Version, strings.Repeat("a", 64))
	if _, err := s.svc.Plan(s.ctx, s.fan, v); !errors.Is(err, versions.ErrNotReady) {
		t.Fatalf("missing publication must remain pending: %v", err)
	}
}

func TestChapterHashAnchorsPreservePublishedAccess(t *testing.T) {
	s := newSite(t, siteOptions{})
	c := s.content("gallery", `{"level":"tier","tiers":["premium"],"preview":{"segments":1}}`)
	v := s.version(c)
	ref := s.ref("gallery", v)
	for i := 1; i <= 3; i++ {
		s.mustUpload(s.creator, ref, fmt.Sprintf("originals/%03d", i), "image/png", pngOf(t, 30, 40, uint8(i)))
	}
	before := s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 3 })
	first := sourcePageID(t, s, ref, "originals/001.png")
	last := sourcePageID(t, s, ref, "originals/003.png")
	s.exec(`INSERT INTO `+s.table("content_segments")+` (tenant_id,version_id,position,title,start_page,release_at)
 VALUES ($1,$2,0,'Released',$3,NULL),($1,$2,1,'Future',$4,now()+interval '1 hour')`, s.tenant, v.Version, first, last)
	// An existing plan still names the same version. Its next grant must obey
	// the edited chapter boundaries without creating another storage item.
	item := before.Segments[0].Item
	if item == nil {
		t.Fatal("published pages have no version item")
	}
	old, err := s.reg.VersionRef(item.Kind, item.ID, item.Version)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := s.reader.Grant(s.ctx, old, s.fan)
	if err != nil {
		t.Fatal(err)
	}
	firstFile, firstOK := s.manifest(ref).Get("low-res/001.webp")
	lastFile, lastOK := s.manifest(ref).Get("low-res/003.webp")
	if !firstOK || !lastOK || grant.Full() || !grant.Allowed(firstFile) || grant.Allowed(lastFile) {
		t.Fatal("the current file grant does not match the released chapter prefix")
	}
	if _, err := grant.URL(lastFile, false); !errors.Is(err, media.ErrNotAllowed) {
		t.Fatalf("unreleased page received a signed URL: %v", err)
	}
	republish := func() {
		t.Helper()
		if err := pgx.BeginFunc(s.ctx, s.pool, func(tx pgx.Tx) error { return s.svc.RepublishTx(s.ctx, tx, []versions.Target{v}) }); err != nil {
			t.Fatal(err)
		}
	}
	republish()
	updated := s.plan(s.fan, v, func(p *versions.ReaderPlan) bool {
		return len(p.Segments) == 2 && p.Segments[0].End == 2 && p.Segments[1].Locked
	})
	if updated.Publication != before.Publication {
		t.Fatal("chapter boundaries rebuilt the immutable file snapshot")
	}
	download, err := s.svc.Download(s.ctx, s.fan, v)
	if err != nil {
		t.Fatalf("released chapter download: %v", err)
	}
	u, err := url.Parse(download.URL)
	if err != nil || u.Query().Get("pages") != "2" {
		t.Fatalf("download exceeded the released prefix: %+v %v", download, err)
	}
	// A new source inserted before the same hash changes only the derived end.
	s.mustUpload(s.creator, ref, "originals/004", "image/png", pngOf(t, 30, 40, 4))
	index := 1
	s.commit(s.creator, ref, media.Op{Op: media.OpMove, Path: "originals/004.png", Index: &index})
	s.plan(s.fan, v, func(p *versions.ReaderPlan) bool {
		return len(p.Segments) == 2 && p.Segments[0].End == 3 && p.Segments[1].Locked
	})
	// Removing an unmarked page does not move the marker to another source.
	s.commit(s.creator, ref, media.Op{Op: media.OpRemove, Path: "originals/002.png", Takedown: true})
	s.plan(s.fan, v, func(p *versions.ReaderPlan) bool {
		return len(p.Segments) == 2 && p.Segments[0].End == 2 && p.Segments[1].Locked
	})
	if sourcePageID(t, s, ref, "originals/003.png") != last {
		t.Fatal("source identity changed after insertion/deletion")
	}
	// Removing the marker itself requires repair and cannot publish a guessed range.
	s.commit(s.creator, ref, media.Op{Op: media.OpRemove, Path: "originals/003.png", Takedown: true})
	eventually(t, "missing anchor rejected", time.Minute, func() bool {
		err := pgx.BeginFunc(s.ctx, s.pool, func(tx pgx.Tx) error { return s.svc.ValidateChapterStartsTx(s.ctx, tx, v, []string{first, last}) })
		return errors.Is(err, versions.ErrInvalid)
	})
}
