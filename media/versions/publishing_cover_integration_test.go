package versions_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

func TestPublishingArchiveCoverRejection(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "without default"
		if fallback {
			name = "configured default"
		}
		t.Run(name, func(t *testing.T) {
			s := newSite(t, siteOptions{mediaGallery: func(k *media.Kind) {
				for i := range k.Uploads {
					if k.Uploads[i].Path == "cover" {
						k.Uploads[i].MaxBytes = 1 // every valid page exceeds this cover-only limit
					}
				}
				if fallback {
					k.Defaults = fstest.MapFS{"fallback.png": &fstest.MapFile{Data: pngOf(t, 64, 64, 5)}}
					k.Public[0].Default = "fallback.png"
				}
			}})
			v := s.version(s.content("gallery", `{"level":"public"}`))
			ref := s.ref("gallery", v)
			archive := zipOf(t, map[string][]byte{"page.png": pngOf(t, 128, 192, 7)})
			sum := sha256.Sum256(archive)
			digest := hex.EncodeToString(sum[:])
			operation := uuid.NewString()
			_, err := s.uploads.Ingest(s.ctx, s.creator, media.IngestRequest{Ref: ref, Path: "import/book", Type: "application/zip", Body: bytes.NewReader(archive), Size: int64(len(archive)), Meta: map[string]any{versions.ArchiveOperation: operation, versions.ArchiveSHA256: digest}})
			if err != nil {
				t.Fatal(err)
			}
			s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 1 })
			if err = versions.Publish(s.svc, s.ctx, v); err != nil {
				t.Fatal(err)
			}
			ready, err := s.svc.ArchiveStatus(s.ctx, v, operation, digest)
			if fallback {
				if err != nil || !ready.Ready {
					t.Fatalf("configured fallback should finish: %+v %v", ready, err)
				}
			} else if !errors.Is(err, versions.ErrArchiveFailed) {
				t.Fatalf("permanent cover rejection must fail instead of waiting forever: %+v %v", ready, err)
			}
		})
	}
}

func TestOrdinaryArchiveCoverRejectionKeepsFallbackBehavior(t *testing.T) {
	s := newSite(t, siteOptions{mediaGallery: func(k *media.Kind) {
		for i := range k.Uploads {
			if k.Uploads[i].Path == "cover" {
				k.Uploads[i].MaxBytes = 1
			}
		}
	}})
	v := s.version(s.content("gallery", `{"level":"public"}`))
	ref := s.ref("gallery", v)
	s.mustUpload(s.creator, ref, "originals/page", "image/png", pngOf(t, 128, 192, 8))
	s.plan(s.creator, v, func(p *versions.ReaderPlan) bool { return p.Pages != nil && *p.Pages == 1 })
	if err := versions.Publish(s.svc, s.ctx, v); err != nil {
		t.Fatal(err)
	}
	if s.manifest(ref).Meta["publishing_cover_errors"] != nil {
		t.Fatal("ordinary content acquired a publishing-operation failure")
	}
}
