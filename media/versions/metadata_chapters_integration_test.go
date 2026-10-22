package versions_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

func TestChaptersAreMetadataAndArchivesFollowReleasedPrefix(t *testing.T) {
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
	for i := 1; i <= 3; i++ {
		s.mustUpload(s.creator, ref, fmt.Sprintf("originals/%03d", i), "image/png", pngOf(t, 30, 40, uint8(i)))
	}
	initial := s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 3 })
	s.settles()
	objects := func() []string {
		var out []string
		for obj, err := range s.Store.List(s.ctx, s.tenant+"/") {
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, obj.Key+":"+obj.ETag)
		}
		sort.Strings(out)
		return out
	}
	before := objects()
	first, second := sourcePageID(t, s, ref, "originals/001.png"), sourcePageID(t, s, ref, "originals/002.png")
	s.exec(`INSERT INTO `+s.table("content_segments")+`(tenant_id,version_id,position,start_page,title,release_at) VALUES($1,$2,0,$3,'Now',NULL),($1,$2,1,$4,'Friday',now()+interval '1 day')`, s.tenant, v.Version, first, second)
	plan := s.plan(s.fan, v, func(p *versions.ReaderPlan) bool { return len(p.Segments) == 2 })
	if plan.Publication != initial.Publication || plan.Segments[0].Item == nil || plan.Segments[1].Item != nil {
		t.Fatalf("chapter metadata rebuilt or widened snapshot: %+v", plan)
	}
	if !reflect.DeepEqual(before, objects()) {
		t.Fatal("chapter metadata copied or rewrote media objects")
	}
	downloadPath := "/v1/contents/" + c + "/versions/" + v.Version + "/download"
	getGrant := func() versions.FileDownload {
		t.Helper()
		resp, body := s.call(access.Actor{}, "GET", downloadPath, nil)
		var grant versions.FileDownload
		if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &grant) != nil {
			t.Fatalf("partial grant: %d %s", resp.StatusCode, body)
		}
		return grant
	}
	entries := func(grant versions.FileDownload) []string {
		t.Helper()
		resp, body := s.call(access.Actor{}, "GET", "/v1/contents/"+c+"/versions/"+v.Version+"/"+grant.URL, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("archive: %d %s", resp.StatusCode, body)
		}
		archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, f := range archive.File {
			names = append(names, f.Name)
		}
		return names
	}
	old := getGrant()
	if names := entries(old); !reflect.DeepEqual(names, []string{"001.png"}) {
		t.Fatalf("future pages entered ZIP: %v", names)
	}
	s.exec(`UPDATE `+s.table("content_segments")+` SET release_at=now()-interval '1 second' WHERE tenant_id=$1 AND version_id=$2 AND position=1`, s.tenant, v.Version)
	plan = s.plan(s.fan, v, func(p *versions.ReaderPlan) bool { return len(p.Segments) == 2 && !p.Segments[1].Locked })
	if plan.Publication != initial.Publication {
		t.Fatal("elapsed chapter date rebuilt snapshot")
	}
	newer := getGrant()
	if newer.URL == old.URL {
		t.Fatal("released ZIP reused old selection identity")
	}
	if names := entries(newer); !reflect.DeepEqual(names, []string{"001.png", "002.png", "003.png"}) {
		t.Fatalf("released manifest order: %v", names)
	}
	if names := entries(old); !reflect.DeepEqual(names, []string{"001.png"}) {
		t.Fatalf("old ticket widened after release: %v", names)
	}
	legacyURL, err := url.Parse(newer.URL)
	if err != nil {
		t.Fatal(err)
	}
	legacyQuery := legacyURL.Query()
	legacyRequest := versions.ArchiveRequest{Target: v, Mark: legacyQuery.Get("mark"), Name: legacyQuery.Get("name"), Revision: legacyQuery.Get("revision")}
	legacyQuery.Del("pages")
	legacyQuery.Set("ticket", s.reader.SignTicket(versions.LegacyArchiveScope(s.tenant, legacyRequest), newer.ExpiresAt))
	legacyURL.RawQuery = legacyQuery.Encode()
	legacyGrant := newer
	legacyGrant.URL = legacyURL.String()
	if names := entries(legacyGrant); len(names) != 3 {
		t.Fatalf("old whole-version ticket lost before expiry: %v", names)
	}
	s.exec(`UPDATE `+s.table("content_segments")+` SET release_at=now()+interval '1 day' WHERE tenant_id=$1 AND version_id=$2 AND position=1`, s.tenant, v.Version)
	if resp, _ := s.call(access.Actor{}, "GET", "/v1/contents/"+c+"/versions/"+v.Version+"/"+legacyGrant.URL, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("old whole-version ticket became partial: %d", resp.StatusCode)
	}
	s.exec(`UPDATE `+s.table("content_segments")+` SET release_at=now()-interval '1 second' WHERE tenant_id=$1 AND version_id=$2 AND position=1`, s.tenant, v.Version)
	// Seeing an image preview alone does not authorize downloading it.
	s.exec(`UPDATE `+s.table("content_items")+` SET access='{"level":"tier","tiers":["premium"],"preview":{"pages":1}}' WHERE tenant_id=$1 AND id=$2`, s.tenant, c)
	if _, err := s.svc.Download(s.ctx, access.Actor{}, v); !errors.Is(err, versions.ErrForbidden) {
		t.Fatalf("image preview granted archive: %v", err)
	}
	s.exec(`UPDATE `+s.table("content_items")+` SET access='{"level":"tier","tiers":["premium"],"preview":{"pages":1,"segments":1}}' WHERE tenant_id=$1 AND id=$2`, s.tenant, c)
	if names := entries(getGrant()); !reflect.DeepEqual(names, []string{"001.png"}) {
		t.Fatalf("free chapter archive exceeded permission: %v", names)
	}
}
