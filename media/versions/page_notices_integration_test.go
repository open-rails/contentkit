package versions_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/gateway"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/versions"
)

// markBlank makes the native analysis call a page certainly blank, then
// regenerates so automatic selection removes it.
func markBlank(t *testing.T, s *site, ref contentref.ContentRef, paths ...string) {
	t.Helper()
	if _, err := s.ms.EditExisting(s.ctx, ref, func(m *media.Manifest) error {
		for _, p := range paths {
			i := m.Find(p)
			if i < 0 {
				t.Fatalf("missing source %s", p)
			}
			a, ok := media.ReadPageAnalysis(m.Files[i].Meta)
			if !ok {
				t.Fatalf("missing native analysis for %s", p)
			}
			a.Blank, a.Confidence = "blank", "exact"
			updated, err := media.WithPageAnalysis(m.Files[i].Meta, a)
			if err != nil {
				return err
			}
			m.Files[i].Meta = updated
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.commit(s.creator, ref, media.Op{Op: media.OpRegenerate})
}

func notices(t *testing.T, s *site, who access.Actor, v versions.Target) (int, versions.PageNotices) {
	t.Helper()
	resp, b := s.call(who, "GET", "/v1/contents/"+v.Content+"/versions/"+v.Version+"/page-notices", nil)
	var out versions.PageNotices
	if resp.StatusCode == 200 {
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, out
}

func TestPageNoticesBeforeFirstUpload(t *testing.T) {
	s := newSite(t, siteOptions{})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	status, got := notices(t, s, s.creator, v)
	if status != http.StatusOK || got.Notices == nil || len(got.Notices) != 0 {
		t.Fatalf("new version without a manifest: status %d, notices %+v", status, got)
	}
	if status, _ := notices(t, s, s.stranger, v); status != http.StatusForbidden {
		t.Fatalf("non-editor new version notices: status %d", status)
	}
}

func TestPageNoticesRestoreAndDismiss(t *testing.T) {
	s := newSite(t, siteOptions{})
	_, v := gallery(t, s)
	s.exec(`DELETE FROM `+s.table("content_segments")+` WHERE tenant_id=$1 AND version_id=$2`, s.tenant, v.Version)
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 5 })
	ref := s.ref("gallery", v)
	base := "/v1/contents/" + v.Content + "/versions/" + v.Version + "/page-notices/"

	markBlank(t, s, ref, "originals/002.png")
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 4 })
	status, got := notices(t, s, s.creator, v)
	if status != 200 || len(got.Notices) != 1 || got.Notices[0].Kind != "removed" || len(got.Notices[0].Pages) != 1 {
		t.Fatalf("notices: %d %+v", status, got)
	}
	page := got.Notices[0].Pages[0]
	if page.ID != sourcePageID(t, s, ref, "originals/002.png") || page.Reason != "blank" || page.PreviewURL == "" {
		t.Fatalf("removed page: %+v", page)
	}
	if status, _ := notices(t, s, s.stranger, v); status != 403 {
		t.Fatalf("stranger notices: %d", status)
	}
	if resp, _ := s.call(s.creator, "POST", base+"removed-unknown/dismiss", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown notice: %d", resp.StatusCode)
	}

	resp, b := s.call(s.creator, "POST", base+got.Notices[0].ID+"/restore", nil)
	var restored versions.PageNotices
	if resp.StatusCode != 200 || json.Unmarshal(b, &restored) != nil || len(restored.Notices) != 0 {
		t.Fatalf("restore: %d %s", resp.StatusCode, b)
	}
	plan := s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 5 })
	var order []string
	for _, seg := range plan.Segments {
		for _, p := range seg.Pages {
			order = append(order, p.Name)
		}
	}
	if strings.Join(order, ",") != "001,002,003,004,005" {
		t.Fatalf("restored page left its place: %v", order)
	}

	markBlank(t, s, ref, "originals/004.png")
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 4 })
	_, got = notices(t, s, s.creator, v)
	if len(got.Notices) != 1 {
		t.Fatalf("second removal: %+v", got)
	}
	for range 2 {
		if resp, b := s.call(s.creator, "POST", base+got.Notices[0].ID+"/dismiss", nil); resp.StatusCode != 204 {
			t.Fatalf("dismiss: %d %s", resp.StatusCode, b)
		}
	}
	if _, after := notices(t, s, s.creator, v); len(after.Notices) != 0 {
		t.Fatalf("dismissed notice still listed: %+v", after)
	}
	if p := s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil }); *p.Pages != 4 {
		t.Fatalf("a dismissal changed the reader: %d pages", *p.Pages)
	}
	if len(files(s.manifest(ref), "originals/")) != 5 {
		t.Fatal("removal deleted source assets")
	}
}

func TestPageNoticesLeaveSegmentedSelectionAlone(t *testing.T) {
	s := newSite(t, siteOptions{})
	c, v := gallery(t, s)
	s.exec(`UPDATE `+s.table("content_items")+` SET access='{"level":"tier","tiers":["premium"],"preview":{"pages":3,"segments":1}}' WHERE tenant_id=$1 AND id=$2`, s.tenant, c)
	anon := access.Actor{Anonymous: true, IP: "203.0.113.15"}
	s.plan(anon, v, func(p *versions.ReaderPlan) bool { return len(p.Segments) == 3 && len(p.Segments[0].Pages) == 2 })
	markBlank(t, s, s.ref("gallery", v), "originals/001.png")
	if status, got := notices(t, s, s.creator, v); status != 200 || len(got.Notices) != 0 {
		t.Fatalf("explicit segments must keep their pages: %d %+v", status, got)
	}
	current := s.plan(anon, v, func(p *versions.ReaderPlan) bool { return len(p.Segments) == 3 && len(p.Segments[0].Pages) == 2 })
	if current.Segments[0].Pages[0].Name != "001" || current.Segments[0].Pages[1].Name != "002" || !current.Segments[1].Locked || !current.Segments[2].Locked {
		t.Fatalf("segment access shifted: %+v", current)
	}
}

// An archive listing p10 before p2 is put in natural order, and says so.
func TestPageNoticesNameArchiveReorder(t *testing.T) {
	s := newSite(t, siteOptions{})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	s.mustUpload(s.creator, ref, "import/book", "application/zip", zipOf(t, map[string][]byte{
		"p1.png": pngOf(t, 20, 30, 1), "p10.png": pngOf(t, 20, 30, 2), "p2.png": pngOf(t, 20, 30, 3)}))
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 3 })
	status, got := notices(t, s, s.creator, v)
	if status != 200 || len(got.Notices) != 1 || got.Notices[0].Kind != "reordered" {
		t.Fatalf("reorder notice: %d %+v", status, got)
	}
	var applied []string
	for _, p := range got.Notices[0].Pages {
		applied = append(applied, fmt.Sprintf("%s:%d>%d", p.SourceName, p.From, p.To))
	}
	if strings.Join(applied, ",") != "p1.png:1>1,p2.png:3>2,p10.png:2>3" {
		t.Fatalf("archive order → applied order: %v", applied)
	}
	if resp, b := s.call(s.creator, "POST", "/v1/contents/"+v.Content+"/versions/"+v.Version+"/page-notices/"+got.Notices[0].ID+"/restore", nil); resp.StatusCode != 400 {
		t.Fatalf("a reorder has nothing to restore: %d %s", resp.StatusCode, b)
	}
}

func TestPageNoticePreviewsCollectEveryURLWindow(t *testing.T) {
	s := newSite(t, siteOptions{reader: func(o *media.ReaderOptions) {
		o.MaxLimit = 2
		o.Queue = nil // stored editor views stay deterministic
	}})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	for i := 1; i <= 5; i++ {
		s.mustUpload(s.creator, ref, fmt.Sprintf("originals/%03d", i), "image/png", pngOf(t, 30, 40, uint8(i)))
	}
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 5 })
	item, _ := s.reg.Item(ref)
	eventually(t, "completed source outputs", time.Minute, func() bool {
		return item.Kind().Readiness(s.manifest(ref)).State == media.StateReady
	})
	// Four removed pages span several read windows; each keeps its preview.
	want := map[string][]byte{}
	views := map[string]string{}
	for _, i := range []int{2, 3, 4, 5} {
		id := fmt.Sprintf("%03d", i)
		hash := sourcePageID(t, s, ref, "originals/"+id+".png")
		want[hash] = pngOf(t, 20, 25, uint8(100+i))
		sum := sha256.Sum256(want[hash])
		names, err := s.ms.NewBlobs(s.ctx, ref, layout.AreaPrivate, [][]byte{sum[:]})
		if err != nil {
			t.Fatal(err)
		}
		key, _ := item.Blob(names[0])
		if _, err := s.Store.Put(s.ctx, key, bytes.NewReader(want[hash]), int64(len(want[hash])), media.PutOptions{ContentType: "image/png"}); err != nil {
			t.Fatal(err)
		}
		views["originals/"+id+".png"] = names[0]
	}
	if _, err := s.ms.EditExisting(s.ctx, ref, func(m *media.Manifest) error {
		for path, blob := range views {
			i := m.Find(path)
			m.Files[i].Editor = &media.EditorImage{Blob: blob, FP: s.reg.EditorFingerprint(m.Files[i])}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	markBlank(t, s, ref, "originals/002.png", "originals/003.png", "originals/004.png", "originals/005.png")
	ring, _ := token.NewRing(signer, nil)
	buckets := map[string]string{}
	for _, ns := range s.reg.Namespaces() {
		buckets[ns] = s.Config.Bucket
	}
	h, err := gateway.New(gateway.Config{Endpoint: s.Config.Endpoint, Region: s.Config.Region,
		AccessKeyID: s.Config.AccessKeyID, SecretAccessKey: s.Config.SecretAccessKey, Ring: ring,
		Hosts: map[string][]string{"media.example": s.reg.Namespaces()}, Buckets: buckets})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)
	var removed []versions.NoticePage
	eventually(t, "four removed pages", time.Minute, func() bool {
		_, got := notices(t, s, s.creator, v)
		if len(got.Notices) == 1 {
			removed = got.Notices[0].Pages
		}
		return len(removed) == 4
	})
	for _, page := range removed {
		if page.PreviewURL == "" {
			t.Errorf("page %s has no preview", page.ID)
			continue
		}
		u, err := url.Parse(page.PreviewURL)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest("GET", gw.URL+u.RequestURI(), nil)
		req.Host = u.Host
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || !bytes.Equal(body, want[page.ID]) {
			t.Errorf("page %s preview: status %d, bytes match %t, error %v", page.ID, resp.StatusCode, bytes.Equal(body, want[page.ID]), err)
		}
	}
}
