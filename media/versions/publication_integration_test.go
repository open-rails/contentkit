package versions_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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

// A version is published as one immutable item per segment: grants open
// released segments to whoever opens the version, previews show a locked
// version's leading released pages, and audio waits for every segment.
func TestPublication(t *testing.T) {
	s := newSite(t, siteOptions{})
	c, v := gallery(t, s)
	anon := access.Actor{Anonymous: true, IP: "203.0.113.1"}

	editor := s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return len(p.Segments) == 3 && p.Playback != nil })
	if editor.Pages != 5 || !eq(locks(editor), []bool{false, false, false}) || editor.Segments[2].Released || !editor.Segments[0].Released {
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

	// Reads of the published items follow the plan.
	read := func(a access.Actor, ref media.RefBody) (*media.ReadResult, error) {
		r, err := s.reg.Ref(ref.Kind, ref.ID)
		if err != nil {
			t.Fatal(err)
		}
		return s.reader.Read(s.ctx, r, a, media.ReadOptions{})
	}
	first, third := *editor.Segments[0].Item, *editor.Segments[2].Item
	if res, err := read(s.fan, first); err != nil || res.Access != media.AccessFull || len(res.Files) != 4 || res.Files[0].URL == "" {
		t.Fatalf("fan reads a released segment: %+v %v", res, err)
	}
	if res, err := read(anon, first); err != nil || res.Access != media.AccessNone {
		t.Fatalf("anonymous reads a locked segment: %+v %v", res, err)
	}
	if _, err := read(s.fan, third); !errors.Is(err, media.ErrNotVisible) {
		t.Fatalf("fan reads an unreleased segment: %v", err)
	}
	if res, err := read(s.creator, third); err != nil || res.Access != media.AccessFull {
		t.Fatalf("editor reads an unreleased segment: %+v %v", res, err)
	}
	if g, err := s.reader.Grant(s.ctx, s.ref("gallery", v), s.fan); err != nil || g.Full() {
		t.Fatalf("fan opened the version's originals: %v", err)
	}

	// The first segment free: anyone reads it.
	s.exec(`UPDATE `+s.table("content_items")+` SET access = '{"level":"tier","tiers":["premium"],"preview":{"pages":3,"segments":1}}' WHERE tenant_id = $1 AND id = $2`, s.tenant, c)
	if p := s.plan(anon, v, func(*versions.ReaderPlan) bool { return true }); !eq(locks(p), []bool{false, true, true}) {
		t.Fatalf("free segment plan %v", locks(p))
	}
	if res, err := read(anon, first); err != nil || res.Access != media.AccessFull {
		t.Fatalf("anonymous reads a free segment: %+v %v", res, err)
	}

	// The last segment released: it opens, and so does the audio.
	s.exec(`UPDATE `+s.table("content_segments")+` SET release_at = now() - interval '1 second' WHERE tenant_id = $1 AND version_id = $2 AND position = 3`,
		s.tenant, v.Version)
	fan = s.plan(s.fan, v, func(p *versions.ReaderPlan) bool { return p.Playback != nil })
	if !eq(locks(fan), []bool{false, false, false}) || !fan.Segments[2].Released || fan.Playback.Tracks[0].Name != "theme" ||
		fan.Playback.Tracks[0].Path != "listen/theme.m4a" {
		t.Fatalf("released plan %+v", fan)
	}
	if res, err := read(s.fan, fan.Playback.Item); err != nil || res.Access != media.AccessFull || len(res.Files) != 1 {
		t.Fatalf("fan reads the playback item: %+v %v", res, err)
	}
	if res, err := read(s.stranger, fan.Playback.Item); err == nil && res.Access == media.AccessFull {
		t.Fatal("a stranger opened the playback item")
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

// Removing a page withdraws the publication showing it at once, previews
// included; the next one replaces it.
func TestWithdrawal(t *testing.T) {
	s := newSite(t, siteOptions{})
	_, v := gallery(t, s)
	ref := s.ref("gallery", v)
	before := s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return len(p.Previews) == 3 })
	s.commit(s.staff, ref, media.Op{Op: media.OpRemove, Path: "originals/002.png", Takedown: true})
	if _, err := s.svc.Plan(s.ctx, s.fan, v); !errors.Is(err, versions.ErrNotReady) {
		t.Fatalf("the withdrawn publication still plans: %v", err)
	}
	if left := files(s.manifest(ref), "previews/"); len(left) != 0 {
		t.Fatalf("previews kept: %v", left)
	}
	old, err := s.reg.Ref(before.Segments[0].Item.Kind, before.Segments[0].Item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.reader.Read(s.ctx, old, s.creator, media.ReadOptions{}); !errors.Is(err, media.ErrNotVisible) {
		t.Fatalf("a withdrawn item reads: %v", err)
	}
	after := s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages == 4 })
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
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages == 5 })
}

// Previews follow releases: the recovery pass widens them once a segment
// releases.
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
	if err := versions.RecoverPublications(s.svc, s.ctx); err != nil {
		t.Fatal(err)
	}
	s.plan(anon, v, func(p *versions.ReaderPlan) bool { return len(p.Previews) == 3 })
}

// Reader content gets what its host never uploads: a cover from its first
// published page, and while locked without a preview of its own, its kind's
// default preview pages. A cover an editor uploads is never replaced.
func TestDerivedCoversAndPreviews(t *testing.T) {
	s := newSite(t, siteOptions{gallery: func(k *kinds.Kind) { k.Preview = &kinds.Preview{Pages: 2} }})
	c := s.content("gallery", `{"level":"tier","tiers":["premium"]}`)
	v := s.version(c)
	ref, top := s.ref("gallery", v), s.ref("gallery", versions.Target{Content: c})
	for i := range 3 {
		s.mustUpload(s.creator, ref, fmt.Sprintf("originals/%03d", i+1), "image/png", pngOf(t, 30, 40, uint8(i+1)))
	}
	anon := access.Actor{Anonymous: true, IP: "203.0.113.5"}
	p := s.plan(anon, v, func(p *versions.ReaderPlan) bool { return len(p.Previews) == 2 })
	if !eq(locks(p), []bool{true}) || p.Previews[0].Name != "001" || p.Previews[1].Name != "002" || p.Segments[0].Item != nil {
		t.Fatalf("anonymous plan %+v", p)
	}

	// The cover is the leading page, rendered public, and follows it.
	coverOf := func(page string) media.File {
		t.Helper()
		var cover media.File
		eventually(t, "the automatic cover of "+page, 2*time.Minute, func() bool {
			m, _, err := s.ms.Get(s.ctx, top)
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
		return err == nil && len(got[0]) > 0 && strings.Contains(got[0][0].URL, "/gallery/"+c+"/public/cover-")
	})
	s.mustUpload(s.creator, ref, "originals/001", "image/png", pngOf(t, 30, 40, 9))
	coverOf("originals/001.png")

	// An editor's cover stays through later publications.
	mine := pngOf(t, 64, 64, 5)
	s.mustUpload(s.creator, top, "cover", "image/png", mine)
	s.mustUpload(s.creator, ref, "originals/004", "image/png", pngOf(t, 30, 40, 4))
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages == 4 })
	if err := versions.Publish(s.svc, s.ctx, v); err != nil && !errors.Is(err, versions.ErrNotReady) {
		t.Fatal(err)
	}
	sum := sha256.Sum256(mine)
	eventually(t, "the editor's cover placed", 2*time.Minute, func() bool {
		cover, _ := s.manifest(top).Get("cover.png")
		return layout.ValidBlobName(cover.Blob)
	})
	if cover, _ := s.manifest(top).Get("cover.png"); cover.Meta["auto_cover"] != nil || !bytes.Equal(digest(cover.Blob), sum[:]) {
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
