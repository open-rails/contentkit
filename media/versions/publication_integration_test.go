package versions_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/versions"
)

// gallery is a premium gallery version, previewing three pages, of five
// pages in three segments, the last released in an hour, with an audio
// track.
func gallery(t *testing.T, s *site) (string, versions.Target) {
	t.Helper()
	c := s.content("gallery", `{"level":"tier","tiers":["premium"],"preview":{"pages":3}}`)
	v := s.version(c)
	ref := s.ref("gallery", v)
	for i := range 5 {
		s.mustUpload(s.creator, ref, fmt.Sprintf("originals/%03d", i+1), "image/png", pngOf(t, 30, 40, uint8(10*i+1)))
	}
	s.mustUpload(s.creator, ref, "audio/theme", "audio/wav", wav(t))
	later := time.Now().Add(time.Hour)
	s.segment(v, 1, 0, 2, nil)
	s.segment(v, 2, 2, 4, nil)
	s.segment(v, 3, 4, 5, &later)
	return c, v
}

func locks(p *versions.ReaderPlan) []bool {
	out := make([]bool, len(p.Segments))
	for i, g := range p.Segments {
		out[i] = g.Locked
	}
	return out
}

func eq[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Chapters select files from one canonical version. Reader grants open only
// released renditions, previews contain leading low-resolution pages, and
// audio waits for every chapter; editors retain the full version.
func TestPublication(t *testing.T) {
	s := newSite(t, siteOptions{})
	c, v := gallery(t, s)
	anon := access.Actor{Anonymous: true, IP: "203.0.113.1"}

	editor := s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return len(p.Segments) == 3 && p.Playback != nil })
	if *editor.Pages != 5 || !eq(locks(editor), []bool{false, false, false}) || editor.Segments[2].Released || !editor.Segments[0].Released {
		t.Fatalf("editor plan %+v", editor)
	}
	if g := editor.Segments[0]; g.Item == nil || len(g.Pages) != 2 || g.Pages[0].Name != "001" || g.Pages[0].Files["low"] != "low-res/001.webp" ||
		g.Pages[0].Files["thumb"] != "thumb/001.webp" || g.Title != "Part 1" {
		t.Fatalf("segment %+v", g)
	}
	fan := s.plan(s.fan, v, func(p *versions.ReaderPlan) bool { return len(p.Previews) == 3 })
	if !eq(locks(fan), []bool{false, false, true}) || fan.Playback != nil || fan.Segments[2].Item != nil {
		t.Fatalf("fan plan %+v", fan)
	}
	public := s.plan(anon, v, func(p *versions.ReaderPlan) bool { return len(p.Previews) == 3 })
	if !eq(locks(public), []bool{true, true, true}) || public.Segments[0].Item != nil || public.Previews[0].Name != "001" || public.Previews[2].Index != 2 {
		t.Fatalf("anonymous plan %+v", public)
	}

	canonical := media.RefBody{Kind: "gallery", ID: c, Version: v.Version}
	for _, segment := range editor.Segments {
		if segment.Item == nil || *segment.Item != canonical {
			t.Fatalf("chapter uses another storage item: %+v", segment.Item)
		}
	}
	// A requested path selects URLs, not permissions. All allowed metadata
	// remains visible, but no other file receives a URL in that request.
	read := func(a access.Actor, ref media.RefBody, paths ...string) (*media.ReadResult, error) {
		r, err := s.reg.VersionRef(ref.Kind, ref.ID, ref.Version)
		if err != nil {
			t.Fatal(err)
		}
		return s.reader.Read(s.ctx, r, a, media.ReadOptions{Paths: paths})
	}
	first, third := *editor.Segments[0].Item, *editor.Segments[2].Item
	firstLow := editor.Segments[0].Pages[0].Files["low"]
	futureLow := editor.Segments[2].Pages[0].Files["low"]
	if res, err := read(s.fan, first, firstLow); err != nil || res.Access != media.AccessPartial || len(res.Files) != 8 {
		t.Fatalf("fan reads released renditions: %+v %v", res, err)
	} else {
		for _, file := range res.Files {
			if (file.Path == firstLow) != (file.URL != "") || file.Path == futureLow || strings.HasPrefix(file.Path, "originals/") || strings.HasPrefix(file.Path, "high/") {
				t.Fatalf("reader received an unrequested or forbidden file: %+v", file)
			}
		}
	}
	if res, err := read(anon, first); err != nil || res.Access != media.AccessPartial || len(res.Files) != 3 {
		t.Fatalf("anonymous preview: %+v %v", res, err)
	} else {
		for _, file := range res.Files {
			if !strings.HasPrefix(file.Path, "low-res/") || file.URL == "" {
				t.Fatalf("preview exposed more than a low-resolution page: %+v", file)
			}
		}
	}
	if res, err := read(s.fan, third, futureLow); err != nil {
		t.Fatal(err)
	} else {
		for _, file := range res.Files {
			if file.Path == futureLow || file.URL != "" {
				t.Fatalf("requesting an unreleased path granted access: %+v", file)
			}
		}
	}
	if res, err := read(s.creator, third, futureLow); err != nil || res.Access != media.AccessFull {
		t.Fatalf("editor reads an unreleased segment: %+v %v", res, err)
	}
	grant, err := s.reader.Grant(s.ctx, s.ref("gallery", v), s.fan)
	if err != nil || grant.Full() || grant.Cookie() != nil {
		t.Fatalf("fan acquired whole-version access: %+v %v", grant, err)
	}
	for _, path := range []string{"originals/001.png", "high/001.webp", futureLow} {
		file, ok := grant.Manifest.Get(path)
		if !ok {
			t.Fatalf("missing denial fixture %q", path)
		}
		if _, err := grant.URL(file, false); !errors.Is(err, media.ErrNotAllowed) {
			t.Fatalf("fan opened forbidden %s: %v", path, err)
		}
	}

	// The first segment free: anyone reads it.
	s.exec(`UPDATE `+s.table("content_items")+` SET access = '{"level":"tier","tiers":["premium"],"preview":{"pages":3,"segments":1}}' WHERE tenant_id = $1 AND id = $2`, s.tenant, c)
	if p := s.plan(anon, v, func(*versions.ReaderPlan) bool { return true }); !eq(locks(p), []bool{false, true, true}) {
		t.Fatalf("free segment plan %v", locks(p))
	}
	if res, err := read(anon, first); err != nil || res.Access != media.AccessPartial || len(res.Files) != 5 {
		t.Fatalf("anonymous reads a free segment: %+v %v", res, err)
	}

	// The last segment released: it opens, and so does the audio.
	s.exec(`UPDATE `+s.table("content_segments")+` SET release_at = now() - interval '1 second' WHERE tenant_id = $1 AND version_id = $2 AND position = 3`,
		s.tenant, v.Version)
	fan = s.plan(s.fan, v, func(p *versions.ReaderPlan) bool { return p.Playback != nil })
	if !eq(locks(fan), []bool{false, false, false}) || !fan.Segments[2].Released || fan.Playback.Tracks[0].Name != "theme" ||
		fan.Playback.Tracks[0].Path != "listen/theme/audio.m4a" || fan.Publication != editor.Publication || fan.Playback.Item != canonical {
		t.Fatalf("released plan %+v", fan)
	}
	track := fan.Playback.Tracks[0].Path
	if res, err := read(s.fan, fan.Playback.Item, track); err != nil || res.Access != media.AccessPartial || len(res.Files) != 11 {
		t.Fatalf("fan reads the playback item: %+v %v", res, err)
	} else {
		for _, file := range res.Files {
			if (file.Path == track) != (file.URL != "") {
				t.Fatalf("audio request issued another file URL: %+v", file)
			}
		}
	}
	if res, err := read(s.stranger, fan.Playback.Item, track); err != nil {
		t.Fatal(err)
	} else {
		for _, file := range res.Files {
			if file.Path == track || file.URL != "" {
				t.Fatal("a stranger opened the playback file")
			}
		}
	}

	// The reader plan route serves the same.
	resp, body := s.call(s.fan, "GET", "/v1/contents/"+c+"/versions/"+v.Version+"/reader", nil)
	var got versions.ReaderPlan
	if resp.StatusCode != 200 || json.Unmarshal(body, &got) != nil || got.Publication != fan.Publication || len(got.Segments) != 3 {
		t.Fatalf("GET reader: %d %s", resp.StatusCode, body)
	}
	if resp, _ := s.call(s.fan, "GET", "/v1/contents/"+c+"/versions/"+s.version(c).Version+"/reader", nil); resp.StatusCode != 202 {
		t.Fatalf("an unpublished version's plan: %d", resp.StatusCode)
	}
}

// Chapter edits need no new file snapshot, even when publishers race.
func TestRacingPublishers(t *testing.T) {
	s := newSite(t, siteOptions{})
	_, v := gallery(t, s)
	initial := s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Playback != nil })
	current := func() int {
		var n int
		if err := s.pool.QueryRow(s.ctx, `SELECT count(*) FROM `+s.table("content_version_publications")+`
WHERE tenant_id = $1 AND version_id = $2 AND kind = 'pages' AND state = 'current'`, s.tenant, v.Version).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for round := range 6 {
		// Other chapter spans: the same immutable files.
		s.exec(`UPDATE `+s.table("content_segments")+` SET end_index = $3 WHERE tenant_id = $1 AND version_id = $2 AND position = 1`,
			s.tenant, v.Version, 1+round%2)
		errs := make([]error, 4)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Go(func() { errs[i] = versions.Publish(s.svc, s.ctx, v) })
		}
		wg.Wait()
		published := false
		for _, err := range errs {
			if err != nil && !errors.Is(err, versions.ErrNotReady) {
				t.Fatalf("round %d: %v", round, err)
			}
			published = published || err == nil
		}
		if n := current(); published && n != 1 {
			t.Fatalf("round %d: published, with %d current pages publications", round, n)
		}
		if plan := s.plan(s.creator, v, func(*versions.ReaderPlan) bool { return true }); plan.Publication != initial.Publication {
			t.Fatalf("chapter edit rebuilt the file snapshot: %s -> %s", initial.Publication, plan.Publication)
		}
	}
}

// The first presign creates the manifest before any upload; readers wait for
// the first page instead of reading an empty plan.
func TestNoPublicationBeforeFirstUpload(t *testing.T) {
	s := newSite(t, siteOptions{})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	body := pngOf(t, 30, 40, 1)
	sum := sha256.Sum256(body)
	if _, err := s.uploads.Presign(s.ctx, s.creator, media.PresignRequest{Ref: ref, Path: "originals/001", Type: "image/png", Size: int64(len(body)), SHA256: sum[:]}); err != nil {
		t.Fatal(err)
	}
	if err := versions.Publish(s.svc, s.ctx, v); err != nil {
		t.Fatal(err)
	}
	if p, err := s.svc.Plan(s.ctx, s.fan, v); !errors.Is(err, versions.ErrNotReady) {
		t.Fatalf("a manifest without uploads planned %+v, %v", p, err)
	}
	s.mustUpload(s.creator, ref, "originals/001", "image/png", body)
	s.plan(s.fan, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 1 })
}

// A republication that adds pages records when (new chapters in the same
// edition); the first publication and an unchanged republication do not.
func TestPagesAddedMarksTheVersion(t *testing.T) {
	s := newSite(t, siteOptions{})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	added := func() *time.Time {
		t.Helper()
		var at *time.Time
		if err := s.pool.QueryRow(s.ctx, `SELECT pages_added_at FROM `+s.table("content_versions")+` WHERE tenant_id = $1 AND id = $2`, s.tenant, v.Version).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	for i := range 2 {
		s.mustUpload(s.creator, ref, fmt.Sprintf("originals/%03d", i+1), "image/png", pngOf(t, 30, 40, uint8(i+1)))
	}
	s.plan(s.fan, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 2 })
	if at := added(); at != nil {
		t.Fatalf("first publication recorded added pages at %v", at)
	}
	if err := versions.Publish(s.svc, s.ctx, v); err != nil {
		t.Fatal(err)
	}
	if at := added(); at != nil {
		t.Fatalf("unchanged republication recorded added pages at %v", at)
	}
	s.mustUpload(s.creator, ref, "originals/003", "image/png", pngOf(t, 30, 40, 3))
	s.plan(s.fan, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 3 })
	if at := added(); at == nil || time.Since(*at) > time.Minute {
		t.Fatalf("added page not recorded: %v", at)
	}
}

// Audio that finishes before any page publishes a pages publication
// without pages; the first pages replace it like any republication.
func TestFirstPagesReplaceAPagelessPublication(t *testing.T) {
	s := newSite(t, siteOptions{})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	s.mustUpload(s.creator, ref, "audio/theme", "audio/wav", wav(t))
	var pages string
	for deadline := time.Now().Add(30 * time.Second); ; {
		err := s.pool.QueryRow(s.ctx, `SELECT coalesce(jsonb_typeof(source->'pages'), '') FROM `+s.table("content_version_publications")+`
 WHERE tenant_id = $1 AND version_id = $2 AND kind = 'pages' AND state = 'current'`, s.tenant, v.Version).Scan(&pages)
		if err == nil {
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) || time.Now().After(deadline) {
			s.dumpJobs()
			t.Fatalf("no current pageless publication: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pages != "array" {
		t.Fatalf("the pageless publication stores pages as %q, not an array", pages)
	}
	// The go-live gate compares its own snapshot with that publication: an
	// audio-only version stops pending once both of its publications are current.
	for deadline := time.Now().Add(30 * time.Second); ; {
		tx, err := s.pool.Begin(s.ctx)
		if err != nil {
			t.Fatal(err)
		}
		pending, err := s.svc.PendingTx(s.ctx, tx, []versions.Target{v})
		_ = tx.Rollback(s.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			s.dumpJobs()
			t.Fatalf("a published audio-only version is pending: %+v", pending)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Publications written before the fix hold null pages; they must not
	// block the next one.
	if _, err := s.pool.Exec(s.ctx, `UPDATE `+s.table("content_version_publications")+` SET source = jsonb_set(source, '{pages}', 'null')
 WHERE tenant_id = $1 AND version_id = $2 AND kind = 'pages' AND state = 'current'`, s.tenant, v.Version); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		s.mustUpload(s.creator, ref, fmt.Sprintf("originals/%03d", i+1), "image/png", pngOf(t, 30, 40, uint8(i+1)))
	}
	s.plan(s.fan, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 2 })
}

// Removing a page withdraws the publication showing it at once, previews
// included; the next one replaces it.
func TestWithdrawal(t *testing.T) {
	s := newSite(t, siteOptions{})
	_, v := gallery(t, s)
	ref := s.ref("gallery", v)
	before := s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return len(p.Previews) == 3 })
	removed, ok := s.manifest(ref).Get(before.Segments[0].Pages[1].Files["low"])
	if !ok {
		t.Fatal("missing released-page rendition")
	}
	s.commit(s.staff, ref, media.Op{Op: media.OpRemove, Path: "originals/002.png", Takedown: true})
	if _, err := s.svc.Plan(s.ctx, s.fan, v); !errors.Is(err, versions.ErrNotReady) {
		t.Fatalf("the withdrawn publication still plans: %v", err)
	}
	if left := files(s.manifest(ref), "previews/"); len(left) != 0 {
		t.Fatalf("previews kept: %v", left)
	}
	old, err := s.reg.VersionRef(before.Segments[0].Item.Kind, before.Segments[0].Item.ID, before.Segments[0].Item.Version)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := s.reader.Grant(s.ctx, old, s.fan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := grant.URL(removed, false); !errors.Is(err, media.ErrNotAllowed) {
		t.Fatalf("a withdrawn page still gets a reader grant: %v", err)
	}
	if res, err := s.reader.Read(s.ctx, old, s.creator, media.ReadOptions{Editor: true}); err != nil || res.Access != media.AccessFull {
		t.Fatalf("withdrawal made the canonical editor item inaccessible: %+v %v", res, err)
	} else {
		for _, file := range res.Files {
			if file.Path == removed.Path {
				t.Fatalf("removed rendition remained in the canonical item: %+v", file)
			}
		}
	}
	after := s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 4 })
	if after.Publication == before.Publication || len(after.Segments) != 2 || after.Segments[0].Pages[1].Name != "003" {
		t.Fatalf("republished plan %+v", after)
	}
	s.plan(s.fan, v, func(p *versions.ReaderPlan) bool { return len(p.Previews) == 3 && p.Previews[1].Name == "003" })

	// Appending keeps the version readable: its current publication stays
	// until a complete successor replaces it.
	s.mustUpload(s.creator, ref, "originals/006", "image/png", pngOf(t, 30, 40, 99))
	if _, err := s.svc.Plan(s.ctx, s.creator, v); err != nil {
		t.Fatalf("an append withdrew the publication: %v", err)
	}
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 5 })
}

// Replacing a source at the same display path never grants its old rendition
// through the new canonical version. A later snapshot uses the new bytes.
func TestSourceReplacementWithdrawsFileGrants(t *testing.T) {
	s := newSite(t, siteOptions{})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	s.mustUpload(s.creator, ref, "originals/001", "image/png", pngOf(t, 30, 40, 1))
	before := s.plan(s.fan, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 1 })
	old, ok := s.manifest(ref).Get(before.Segments[0].Pages[0].Files["low"])
	if !ok {
		t.Fatal("missing first rendition")
	}
	grant, err := s.reader.Grant(s.ctx, ref, s.fan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := grant.URL(old, false); err != nil {
		t.Fatalf("initial rendition was not granted: %v", err)
	}
	s.mustUpload(s.creator, ref, "originals/001", "image/png", pngOf(t, 30, 40, 2))
	grant, err = s.reader.Grant(s.ctx, ref, s.fan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := grant.URL(old, false); !errors.Is(err, media.ErrNotAllowed) {
		t.Fatalf("replacement still granted the old rendition: %v", err)
	}
	after := s.plan(s.fan, v, func(p *versions.ReaderPlan) bool {
		return p.Publication != before.Publication && len(p.Segments) == 1 && len(p.Segments[0].Pages) == 1
	})
	current, ok := s.manifest(ref).Get(after.Segments[0].Pages[0].Files["low"])
	if !ok || current.Blob == old.Blob {
		t.Fatalf("replacement reused its old rendition: %+v", current)
	}
	grant, err = s.reader.Grant(s.ctx, ref, s.fan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := grant.URL(current, false); err != nil {
		t.Fatalf("new rendition was not granted: %v", err)
	}
	if _, err := grant.URL(old, false); !errors.Is(err, media.ErrNotAllowed) {
		t.Fatalf("new publication revived the old rendition: %v", err)
	}
}

// Previews follow release metadata immediately, without media jobs.
func TestPreviewsFollowReleases(t *testing.T) {
	s := newSite(t, siteOptions{})
	c := s.content("gallery", `{"level":"tier","tiers":["premium"],"preview":{"pages":3}}`)
	v := s.version(c)
	ref := s.ref("gallery", v)
	for i := range 4 {
		s.mustUpload(s.creator, ref, fmt.Sprintf("originals/%03d", i+1), "image/png", pngOf(t, 30, 40, uint8(i+1)))
	}
	later := time.Now().Add(time.Hour)
	s.segment(v, 1, 0, 1, nil)
	s.segment(v, 2, 1, 4, &later)
	anon := access.Actor{Anonymous: true, IP: "203.0.113.4"}
	s.plan(anon, v, func(p *versions.ReaderPlan) bool { return len(p.Previews) == 1 })
	s.exec(`UPDATE `+s.table("content_segments")+` SET release_at = now() - interval '1 second' WHERE tenant_id = $1 AND version_id = $2 AND position = 2`,
		s.tenant, v.Version)
	s.plan(anon, v, func(p *versions.ReaderPlan) bool { return len(p.Previews) == 3 })
}

// Reader content gets what its host never uploads: each version a cover from
// its first published page, and while locked without a preview of its own, the preview
// pages its kind's locked view names; a view hiding length hides its page
// count and locked pages. A cover an editor uploads is never replaced.
func TestDerivedCoversAndPreviews(t *testing.T) {
	s := newSite(t, siteOptions{gallery: func(k *kinds.Kind) { k.Locked = &kinds.Locked{Preview: &kinds.Preview{Pages: 2}} }})
	c := s.content("gallery", `{"level":"tier","tiers":["premium"]}`)
	v := s.version(c)
	ref := s.ref("gallery", v)
	for i := range 3 {
		s.mustUpload(s.creator, ref, fmt.Sprintf("originals/%03d", i+1), "image/png", pngOf(t, 30, 40, uint8(i+1)))
	}
	anon := access.Actor{Anonymous: true, IP: "203.0.113.5"}
	p := s.plan(anon, v, func(p *versions.ReaderPlan) bool { return len(p.Previews) == 2 })
	if len(p.Segments) != 0 || p.Pages != nil || p.Previews[0].Name != "001" || p.Previews[1].Name != "002" {
		t.Fatalf("anonymous plan %+v", p)
	}

	// The cover is the leading page, rendered public, and follows it.
	coverOf := func(page string) media.File {
		t.Helper()
		var cover media.File
		eventually(t, "the automatic cover of "+page, 2*time.Minute, func() bool {
			m, _, err := s.ms.Get(s.ctx, ref)
			if err != nil {
				return false
			}
			src, _ := s.manifest(ref).Get(page)
			cover, _ = m.Get("cover.png")
			return cover.Meta["auto_cover"] == true && sameBytes(cover.Blob, src.Blob)
		})
		return cover
	}
	coverOf("originals/001.png")
	eventually(t, "the public cover", 2*time.Minute, func() bool {
		got, err := s.svc.Covers(s.ctx, []versions.CoverOf{{Kind: "gallery", ID: c}})
		return err == nil && len(got[0]) > 0 && strings.Contains(got[0][0].URL, "/gallery/"+v.Version+"/public/cover-")
	})
	s.mustUpload(s.creator, ref, "originals/001", "image/png", pngOf(t, 30, 40, 9))
	coverOf("originals/001.png")

	// An editor's cover stays through later publications.
	mine := pngOf(t, 64, 64, 5)
	s.mustUpload(s.creator, ref, "cover", "image/png", mine)
	s.mustUpload(s.creator, ref, "originals/004", "image/png", pngOf(t, 30, 40, 4))
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 4 })
	if err := versions.Publish(s.svc, s.ctx, v); err != nil && !errors.Is(err, versions.ErrNotReady) {
		t.Fatal(err)
	}
	sum := sha256.Sum256(mine)
	eventually(t, "the editor's cover placed", 2*time.Minute, func() bool {
		cover, _ := s.manifest(ref).Get("cover.png")
		return layout.ValidBlobName(cover.Blob)
	})
	if cover, _ := s.manifest(ref).Get("cover.png"); cover.Meta["auto_cover"] != nil || !bytes.Equal(digest(cover.Blob), sum[:]) {
		t.Fatalf("the editor's cover was replaced: %+v", cover)
	}

	// A rule's own preview wins over the kind's.
	s.exec(`UPDATE `+s.table("content_items")+` SET access = '{"level":"tier","tiers":["premium"],"preview":{"pages":1}}' WHERE tenant_id = $1 AND id = $2`, s.tenant, c)
	s.plan(anon, v, func(p *versions.ReaderPlan) bool { return len(p.Previews) == 1 })
}

func digest(blob string) []byte {
	d, _ := layout.BlobDigest(blob)
	return d
}

func sameBytes(a, b string) bool { return a != "" && b != "" && bytes.Equal(digest(a), digest(b)) }
