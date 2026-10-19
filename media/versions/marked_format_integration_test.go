package versions_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"image/png"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

type countingArchiveMarker struct{ calls atomic.Int32 }

func (m *countingArchiveMarker) Decide(ctx context.Context, r versions.DownloadRequest) (versions.DownloadDecision, error) {
	return (marker{}).Decide(ctx, r)
}

func (m *countingArchiveMarker) Mark(ctx context.Context, actor access.Actor, target versions.Target, page int, data []byte) ([]byte, string, error) {
	m.calls.Add(1)
	return (marker{}).Mark(ctx, actor, target, page, data)
}

func TestMarkedArchiveKeepsFormatsAndStreamsEveryRequest(t *testing.T) {
	policy := &countingArchiveMarker{}
	s := newSite(t, siteOptions{policy: policy, mediaGallery: func(k *media.Kind) {
		for i := range k.Uploads {
			if k.Uploads[i].Path == "originals/{name}" {
				k.Uploads[i].Types = append(k.Uploads[i].Types, "image/jpeg")
			}
		}
		for i := range k.Private {
			if k.Private[i].Name == "zip" {
				k.Private[i].Zip = "originals/"
			}
		}
	}})
	c := s.content("gallery", `{"level":"tier","tiers":["premium"]}`)
	v := s.version(c)
	ref := s.ref("gallery", v)
	image, err := png.Decode(bytes.NewReader(pngOf(t, 30, 40, 7)))
	if err != nil {
		t.Fatal(err)
	}
	var first bytes.Buffer
	if err := jpeg.Encode(&first, image, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	second := pngOf(t, 30, 40, 8)
	s.mustUpload(s.creator, ref, "originals/900", "image/jpeg", first.Bytes())
	s.mustUpload(s.creator, ref, "originals/100", "image/png", second)
	index := 0
	s.commit(s.creator, ref, media.Op{Op: media.OpMove, Path: "originals/900.jpg", Index: &index})
	eventually(t, "original-format archive", time.Minute, func() bool {
		f, ok := s.manifest(ref).Get("download/pages.zip")
		return ok && f.Blob != ""
	})
	path := "/v1/contents/" + c + "/versions/" + v.Version + "/download"
	var download versions.FileDownload
	// The ZIP may finish before the independently queued pages publication.
	// Wait for the real download grant, retaining failures other than pending.
	eventually(t, "ready archive download", time.Minute, func() bool {
		response, data := s.call(s.fan, "GET", path, nil)
		if response.StatusCode == 202 {
			return false
		}
		if response.StatusCode != 200 || json.Unmarshal(data, &download) != nil || !strings.HasPrefix(download.URL, "archive?") {
			t.Fatalf("download: %d %s", response.StatusCode, data)
		}
		return true
	})
	for range 2 {
		response, data := s.call(access.Actor{}, "GET", strings.TrimSuffix(path, "download")+download.URL, nil)
		if response.StatusCode != 200 || response.Header.Get("Cache-Control") != "private, no-store" {
			t.Fatalf("private fresh archive: %d %s", response.StatusCode, response.Header.Get("Cache-Control"))
		}
		archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil || len(archive.File) != 2 {
			t.Fatalf("archive: %v", err)
		}
		if archive.File[0].Name != "01.jpg" || archive.File[1].Name != "02.png" {
			t.Fatalf("virtual order/format: %s %s", archive.File[0].Name, archive.File[1].Name)
		}
		file, _ := archive.File[0].Open()
		got, err := io.ReadAll(file)
		file.Close()
		if err != nil || !bytes.Equal(got, first.Bytes()) {
			t.Fatal("unmarked JPEG bytes changed")
		}
	}
	if policy.calls.Load() != 2 {
		t.Fatalf("personalized output reused: %d marks", policy.calls.Load())
	}
}
