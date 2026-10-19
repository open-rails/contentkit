package versions_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

func TestLazyArchiveChecksCachedAccessChaptersAndSourceRevision(t *testing.T) {
	s := newSite(t, siteOptions{mediaGallery: func(k *media.Kind) {
		for i := range k.Private {
			if k.Private[i].Name == "zip" {
				k.Private[i].Zip = "originals/"
			}
		}
	}})
	c := s.content("gallery", `{"level":"public"}`)
	v := s.version(c)
	ref := s.ref("gallery", v)
	for i, name := range []string{"100", "200"} {
		s.mustUpload(s.creator, ref, "originals/"+name, "image/png", pngOf(t, 30, 40, uint8(i+1)))
	}
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 2 })
	if _, ok := s.manifest(ref).Get("download/pages.zip"); ok {
		t.Fatal("worker generated reader ZIP before a download")
	}
	path := "/v1/contents/" + c + "/versions/" + v.Version + "/download"
	resp, data := s.call(access.Actor{}, "GET", path, nil)
	var grant versions.FileDownload
	if resp.StatusCode != 200 || json.Unmarshal(data, &grant) != nil {
		t.Fatalf("grant: %d %s", resp.StatusCode, data)
	}
	archive := strings.TrimSuffix(path, "download") + grant.URL
	resp, first := s.call(access.Actor{}, "GET", archive, nil)
	if resp.StatusCode != 200 || len(first) == 0 {
		t.Fatalf("lazy archive: %d %s", resp.StatusCode, first)
	}
	item, _ := s.reg.Item(ref)
	cacheCount := 0
	for obj, err := range s.Store.List(s.ctx, item.TempPrefix()+"zip-cache-") {
		if err != nil {
			t.Fatal(err)
		}
		if obj.Size <= 0 {
			t.Fatal("empty cache")
		}
		cacheCount++
	}
	if cacheCount != 1 {
		t.Fatalf("cache count=%d", cacheCount)
	}
	resp, second := s.call(access.Actor{}, "GET", archive, nil)
	if resp.StatusCode != 200 || !bytes.Equal(first, second) {
		t.Fatal("cache changed ZIP bytes")
	}
	// A cached bearer ticket cannot bypass a newly gated account or chapter.
	s.exec(`UPDATE `+s.table("content_items")+` SET access='{"level":"tier","tiers":["premium"]}' WHERE tenant_id=$1 AND id=$2`, s.tenant, c)
	if resp, _ := s.call(access.Actor{}, "GET", archive, nil); resp.StatusCode != 403 {
		t.Fatalf("cached access check: %d", resp.StatusCode)
	}
	s.exec(`UPDATE `+s.table("content_items")+` SET access='{"level":"public"}' WHERE tenant_id=$1 AND id=$2`, s.tenant, c)
	firstID := sourcePageID(t, s, ref, "originals/100.png")
	s.exec(`INSERT INTO `+s.table("content_segments")+`(tenant_id,version_id,position,start_page,release_at) VALUES($1,$2,0,$3,now()+interval '1 hour')`, s.tenant, v.Version, firstID)
	if resp, _ := s.call(access.Actor{}, "GET", archive, nil); resp.StatusCode != 403 {
		t.Fatalf("cached chapter release: %d", resp.StatusCode)
	}
	s.exec(`UPDATE `+s.table("content_segments")+` SET start_page=$3 WHERE tenant_id=$1 AND version_id=$2`, s.tenant, v.Version, strings.Repeat("f", 64))
	if resp, _ := s.call(access.Actor{}, "GET", archive, nil); resp.StatusCode == 200 {
		t.Fatal("missing chapter anchor opened cache")
	}
	s.exec(`DELETE FROM `+s.table("content_segments")+` WHERE tenant_id=$1 AND version_id=$2`, s.tenant, v.Version)
	index := 0
	s.commit(s.creator, ref, media.Op{Op: media.OpMove, Path: "originals/200.png", Index: &index})
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool {
		return len(p.Segments) > 0 && len(p.Segments[0].Pages) > 0 && p.Segments[0].Pages[0].ID == sourcePageID(t, s, ref, "originals/200.png")
	})
	if resp, _ := s.call(access.Actor{}, "GET", archive, nil); resp.StatusCode != 404 {
		t.Fatalf("stale ordered ticket: %d", resp.StatusCode)
	}
}

func TestArchiveHTTPAbortsMidStreamFailure(t *testing.T) {
	s := newSite(t, siteOptions{mediaGallery: func(k *media.Kind) {
		for i := range k.Private {
			if k.Private[i].Name == "zip" {
				k.Private[i].Zip = "originals/"
			}
		}
	}})
	c := s.content("gallery", `{"level":"public"}`)
	v := s.version(c)
	ref := s.ref("gallery", v)
	for i, name := range []string{"a", "b"} {
		s.mustUpload(s.creator, ref, "originals/"+name, "image/png", pngOf(t, 200, 300, uint8(i+1)))
	}
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 2 })
	grant, err := s.svc.Download(s.ctx, access.Actor{}, v)
	if err != nil {
		t.Fatal(err)
	}
	var sources []media.File
	for _, f := range s.manifest(ref).Files {
		if f.IsUpload() && strings.HasPrefix(f.Path, "originals/") {
			sources = append(sources, f)
		}
	}
	if len(sources) != 2 {
		t.Fatalf("source count: %d", len(sources))
	}
	second := sources[1]
	item, _ := s.reg.Item(ref)
	key, _ := item.Blob(second.Blob)
	if err := s.Store.Delete(s.ctx, key); err != nil {
		t.Fatal(err)
	}
	url := s.srv.URL + "/v1/contents/" + c + "/versions/" + v.Version + "/" + grant.URL
	resp, err := http.Get(url)
	if err == nil {
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
	}
	if err == nil {
		t.Fatal("truncated ZIP completed its HTTP transport successfully")
	}
	for _, err := range s.Store.List(s.ctx, item.TempPrefix()+"zip-cache-") {
		if err != nil && !errors.Is(err, media.ErrNotFound) {
			t.Fatal(err)
		}
		t.Fatal("failed transfer published a cache")
	}
}
