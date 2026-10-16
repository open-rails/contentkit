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

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/gateway"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/versions"
)

func TestPageReviewCollectsEveryURLWindow(t *testing.T) {
	s := newSite(t, siteOptions{reader: func(o *media.ReaderOptions) {
		o.MaxLimit = 2
		o.Queue = nil // stored editor views and fallback previews stay deterministic
	}})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	wantSource, wantPreview := map[string][]byte{}, map[string][]byte{}
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("%03d", i)
		wantSource[id] = pngOf(t, 30, 40, uint8(i))
		s.mustUpload(s.creator, ref, "originals/"+id, "image/png", wantSource[id])
	}
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 5 })
	item, _ := s.reg.Item(ref)
	eventually(t, "completed source outputs", time.Minute, func() bool {
		return item.Kind().Readiness(s.manifest(ref)).State == media.StateReady
	})
	// First, middle and last pages have recorded editor views. The other
	// pages use existing readable renditions, testing both preview sources.
	views := map[string]string{}
	for _, i := range []int{1, 3, 5} {
		id := fmt.Sprintf("%03d", i)
		wantPreview[id] = pngOf(t, 20, 25, uint8(100+i))
		sum := sha256.Sum256(wantPreview[id])
		names, err := s.ms.NewBlobs(s.ctx, ref, layout.AreaPrivate, [][]byte{sum[:]})
		if err != nil {
			t.Fatal(err)
		}
		key, _ := item.Blob(names[0])
		if _, err := s.Store.Put(s.ctx, key, bytes.NewReader(wantPreview[id]), int64(len(wantPreview[id])), media.PutOptions{ContentType: "image/png"}); err != nil {
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
	g, err := s.reader.Grant(s.ctx, ref, s.creator)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range g.Manifest.Files {
		id := strings.TrimSuffix(strings.TrimPrefix(f.From, "originals/"), ".png")
		if wantSource[id] == nil || wantPreview[id] != nil || !strings.HasPrefix(f.Type, "image/") || !g.Allowed(f) {
			continue
		}
		key, _ := item.Blob(f.Blob)
		body, _, err := s.Store.Get(s.ctx, key, media.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		wantPreview[id], err = io.ReadAll(body)
		body.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
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
	response, body := s.call(s.creator, "GET", "/v1/contents/"+v.Content+"/versions/"+v.Version+"/pages", nil)
	var review versions.PageReview
	if response.StatusCode != 200 || json.Unmarshal(body, &review) != nil || len(review.Pages) != 5 {
		t.Fatalf("review: status %d, pages %d", response.StatusCode, len(review.Pages))
	}
	for _, page := range review.Pages {
		for _, asset := range []struct {
			name, url string
			want      []byte
		}{{"source", page.SourceURL, wantSource[page.ID]}, {"preview", page.PreviewURL, wantPreview[page.ID]}} {
			if len(asset.want) == 0 {
				t.Fatalf("fixture has no expected %s bytes for page %s", asset.name, page.ID)
			}
			if asset.url == "" {
				t.Errorf("page %s has no %s capability", page.ID, asset.name)
				continue
			}
			u, err := url.Parse(asset.url)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest("GET", gw.URL+u.RequestURI(), nil)
			req.Host = u.Host
			resp, err := gw.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != 200 || !bytes.Equal(got, asset.want) {
				t.Errorf("page %s %s: status %d, expected bytes match %t, error %v", page.ID, asset.name, resp.StatusCode, bytes.Equal(got, asset.want), err)
			}
		}
	}
}
