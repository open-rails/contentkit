package versions_test

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

func TestStagedArchiveRenditionsRunBeforeAttachment(t *testing.T) {
	s := newSite(t, siteOptions{})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	body := zipOf(t, map[string][]byte{"001.png": pngOf(t, 30, 40, 21), "002.png": pngOf(t, 30, 40, 22)})
	sum := sha256.Sum256(body)
	presigned, err := s.uploads.Presign(s.ctx, s.creator, media.PresignRequest{Ref: ref, Path: "import/staged", Type: "application/zip", Size: int64(len(body)), SHA256: sum[:]})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(presigned.Put.Method, presigned.Put.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header = presigned.Put.Header.Clone()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PUT: %d", response.StatusCode)
	}
	if _, err := s.uploads.Commit(s.ctx, s.creator, ref, uuid.NewString(), []media.Op{{Op: media.OpPut, Path: presigned.Path, Blob: presigned.Blob, Unattached: true}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "staged ZIP private renditions", time.Minute, func() bool {
		manifest := s.manifest(ref)
		pages := 0
		for _, file := range manifest.Files {
			if !file.IsUpload() || !strings.HasPrefix(file.Path, "originals/") {
				continue
			}
			if !file.Unattached {
				t.Fatal("ingestion attached an unsaved page")
			}
			if len(manifest.Outputs(file.Path, "low")) == 1 && len(manifest.Outputs(file.Path, "thumb")) == 1 {
				pages++
			}
		}
		return pages == 2
	})
	read, err := s.reader.Read(s.ctx, ref, s.fan, media.ReadOptions{Prefix: "originals/"})
	if err != nil || len(read.Files) != 0 {
		t.Fatalf("unsaved pages became readable: %+v / %v", read, err)
	}
	s.commit(s.creator, ref, media.Op{Op: media.OpAttach, Path: presigned.Path})
	s.plan(s.fan, v, func(plan *versions.ReaderPlan) bool { return plan.Pages != nil && *plan.Pages == 2 })
}
