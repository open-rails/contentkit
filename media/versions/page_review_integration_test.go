package versions_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

func TestPageReviewOverrideRestoreAndReorder(t *testing.T) {
	s := newSite(t, siteOptions{})
	_, v := gallery(t, s)
	s.exec(`DELETE FROM `+s.table("content_segments")+` WHERE tenant_id=$1 AND version_id=$2`, s.tenant, v.Version)
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 5 })
	url := "/v1/contents/" + v.Content + "/versions/" + v.Version + "/pages"
	resp, b := s.call(s.creator, "GET", url, nil)
	var review versions.PageReview
	if err := json.Unmarshal(b, &review); resp.StatusCode != 200 || err != nil || len(review.Pages) != 5 {
		t.Fatalf("review: %d %s %v", resp.StatusCode, b, err)
	}
	for _, page := range review.Pages {
		if page.SourceURL == "" {
			t.Errorf("editor review has no retained source URL for %s", page.ID)
		}
	}
	if t.Failed() {
		return
	}
	if resp, _ := s.call(s.stranger, "GET", url, nil); resp.StatusCode != 403 {
		t.Fatalf("stranger review: %d", resp.StatusCode)
	}
	old := review.Revision
	excluded := review.Pages[1].ID
	resp, b = s.call(s.creator, "PATCH", url, versions.PageReviewPatch{Revision: old, Pages: []versions.PageReviewChange{{ID: excluded, Mode: "exclude"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("exclude: %d %s", resp.StatusCode, b)
	}
	if err := json.Unmarshal(b, &review); err != nil {
		t.Fatal(err)
	}
	for _, page := range review.Pages {
		if page.ID == excluded && (page.Included || page.SourceURL == "") {
			t.Fatalf("excluded page lost its editor source capability: %+v", page)
		}
	}
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 4 })
	if resp, _ := s.call(s.creator, "PATCH", url, versions.PageReviewPatch{Revision: old}); resp.StatusCode != 409 {
		t.Fatalf("stale review accepted: %d", resp.StatusCode)
	}
	order := make([]string, len(review.Pages))
	for i, p := range review.Pages {
		order[len(order)-1-i] = p.ID
	}
	resp, b = s.call(s.creator, "PATCH", url, versions.PageReviewPatch{Revision: review.Revision, Pages: []versions.PageReviewChange{{ID: excluded, Mode: "include"}}, Order: order})
	if resp.StatusCode != 200 {
		t.Fatalf("restore/reorder: %d %s", resp.StatusCode, b)
	}
	got := s.plan(s.creator, v, func(p *versions.ReaderPlan) bool {
		return p.Pages != nil && *p.Pages == 5 && len(p.Segments) > 0 && len(p.Segments[0].Pages) > 0 && p.Segments[0].Pages[0].Name == order[0]
	})
	if *got.Pages != 5 {
		t.Fatalf("restored pages: %+v", got)
	}
	if len(files(s.manifest(s.ref("gallery", v)), "originals/")) != 5 {
		t.Fatal("exclusion removed source assets")
	}
}

func TestPageReviewSegmentGuardPreservesAccess(t *testing.T) {
	s := newSite(t, siteOptions{})
	c, v := gallery(t, s)
	s.exec(`UPDATE `+s.table("content_items")+` SET access='{"level":"tier","tiers":["premium"],"preview":{"pages":3,"segments":1}}' WHERE tenant_id=$1 AND id=$2`, s.tenant, c)
	anon := access.Actor{Anonymous: true, IP: "203.0.113.15"}
	initial := s.plan(anon, v, func(p *versions.ReaderPlan) bool { return len(p.Segments) == 3 && len(p.Segments[0].Pages) == 2 })
	if initial.Segments[0].Pages[0].Name != "001" || initial.Segments[0].Pages[1].Name != "002" {
		t.Fatalf("initial free pages: %+v", initial)
	}
	ref := s.ref("gallery", v)
	_, err := s.ms.EditExisting(s.ctx, ref, func(m *media.Manifest) error {
		i := m.Find("originals/001.png")
		if i < 0 {
			t.Fatal("missing source")
		}
		a, ok := media.ReadPageAnalysis(m.Files[i].Meta)
		if !ok {
			t.Fatal("missing native analysis")
		}
		a.Blank = "blank"
		a.Confidence = "exact"
		updated, e := media.WithPageAnalysis(m.Files[i].Meta, a)
		if e != nil {
			return e
		}
		m.Files[i].Meta = updated
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.commit(s.creator, ref, media.Op{Op: media.OpRegenerate})
	url := "/v1/contents/" + c + "/versions/" + v.Version + "/pages"
	resp, body := s.call(s.creator, "GET", url, nil)
	var review versions.PageReview
	if resp.StatusCode != 200 || json.Unmarshal(body, &review) != nil {
		t.Fatalf("review: %d %s", resp.StatusCode, body)
	}
	if review.Selected != 5 || !slices.Contains(review.Pages[0].Flags, "segmented_selection_preserved") {
		t.Fatalf("unsafe segmented selection: %+v", review)
	}
	for _, patch := range []versions.PageReviewPatch{
		{Revision: review.Revision, Pages: []versions.PageReviewChange{{ID: "001", Mode: "exclude"}}},
		{Revision: review.Revision, Order: []string{"005", "004", "003", "002", "001"}},
	} {
		if resp, body := s.call(s.creator, "PATCH", url, patch); resp.StatusCode != 400 {
			t.Fatalf("unsafe segment edit accepted: %d %s", resp.StatusCode, body)
		}
	}
	current := s.plan(anon, v, func(p *versions.ReaderPlan) bool { return len(p.Segments) == 3 && len(p.Segments[0].Pages) == 2 })
	if current.Segments[0].Pages[0].Name != "001" || current.Segments[0].Pages[1].Name != "002" || !current.Segments[1].Locked || !current.Segments[2].Locked {
		t.Fatalf("segment access shifted: %+v", current)
	}
}
