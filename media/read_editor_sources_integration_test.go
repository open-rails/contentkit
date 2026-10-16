package media_test

import (
	"archive/zip"
	"bytes"
	"encoding/hex"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/layout"
)

// ServeOriginals is a viewer policy. An explicit, authorized editor read
// must still let the editor verify retained source bytes, including exclusions
// and completed archives, without opening host-only derived downloads.
func TestEditorReadRetainedSourcesWithoutServeOriginals(t *testing.T) {
	for _, downloadArea := range []bool{false, true} {
		t.Run(map[bool]string{false: "private", true: "download"}[downloadArea], func(t *testing.T) {
			f := newFixtureOn(t, s3test.Open(t), func(c *media.Config) {
				c.Kinds[0].Private[2].HostOnly = true
				if downloadArea {
					c.Kinds[0].Downloads = []string{"originals/{name}", "high", "zip"}
				}
			})
			f.visible(1)
			ref := f.gallery(1, 2)
			var archive bytes.Buffer
			zw := zip.NewWriter(&archive)
			entry, err := zw.Create("000.png")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := entry.Write(png(100)); err != nil {
				t.Fatal(err)
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			f.put(ref, "import/book.zip", "application/zip", archive.Bytes())
			pending, blob := f.upload(ref, "originals/pending.png", "image/png", png(102))
			if _, err := f.up.Commit(t.Context(), f.editor, ref, uuid.NewString(), []media.Op{{Op: media.OpPut, Path: pending, Blob: blob}}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.ms.EditExisting(t.Context(), ref, func(m *media.Manifest) error {
				m.Files[m.Find("originals/001.png")].Meta = map[string]any{"page": map[string]any{"included": false}}
				i := m.Find("import/book.zip")
				sum, _ := layout.BlobDigest(m.Files[i].Blob)
				m.Files[i].Meta = map[string]any{"archive_completed_sha256": hex.EncodeToString(sum)}
				m.Files[m.Find("cover.png")].Gone = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"originals/000.png": string(png(100)), "originals/001.png": string(png(101)), "import/book.zip": archive.String()}
			for _, mode := range []media.DeliveryMode{media.DeliverURL, media.DeliverCookie} {
				rd, err := media.NewReader(media.ReaderOptions{Manifests: f.ms,
					Delivery: media.Delivery{Mode: mode, CookieDomain: "doujins.test", SigningKey: signKey}})
				if err != nil {
					t.Fatal(err)
				}
				read, err := rd.Read(t.Context(), ref, f.editor, media.ReadOptions{Editor: true})
				if err != nil {
					t.Fatal(err)
				}
				var hdr []string
				if read.Cookie != nil {
					hdr = []string{"Cookie", media.CookieName + "=" + read.Cookie.Value}
				}
				got := urls(read)
				for path, body := range want {
					if got[path] == "" {
						t.Errorf("%s editor source %s has no URL", mode, path)
						continue
					}
					if status, actual, _ := f.fetch(got[path], hdr...); status != http.StatusOK || actual != body {
						t.Errorf("%s editor source %s: status %d, original bytes match %t", mode, path, status, actual == body)
					}
				}
				for _, path := range []string{"cover.png", "originals/pending.png", "download/pages.zip"} {
					if got[path] != "" {
						t.Errorf("%s unavailable or host-only %s received a URL", mode, path)
					}
				}
				if downloadArea && got["originals/000.png"] != "" {
					// This source capability must not open a different original.
					u, _ := url.Parse(got["originals/000.png"])
					item, _ := f.reg.Item(ref)
					key, _ := item.Blob(f.fileBlob(ref, "originals/001.png"))
					u.Path = "/v1/" + key
					if status, _, _ := f.fetch(u.String(), hdr...); status != http.StatusNotFound {
						t.Errorf("%s source token opened another original: %d", mode, status)
					}
				}
				page, err := rd.Read(t.Context(), ref, f.editor, media.ReadOptions{Editor: true, Offset: read.Total, Limit: 1})
				if err != nil {
					t.Fatal(err)
				}
				for _, file := range page.Files {
					if file.URL != "" {
						t.Errorf("out-of-range file %s received a URL", file.Path)
					}
				}
				for _, actor := range []access.Actor{f.editor, {Anonymous: true, IP: "203.0.113.1"}} {
					ordinary, err := rd.Read(t.Context(), ref, actor, media.ReadOptions{})
					if err != nil {
						t.Fatal(err)
					}
					for _, file := range ordinary.Files {
						if _, source := want[file.Path]; source {
							t.Errorf("ordinary read listed source %s", file.Path)
						}
					}
				}
				f.res.set(cid(1), resolve.Resolution{Visible: true, Accessible: true})
				noneditor, err := rd.Read(t.Context(), ref, f.editor, media.ReadOptions{Editor: true})
				if err != nil {
					t.Fatal(err)
				}
				for _, file := range noneditor.Files {
					if _, source := want[file.Path]; source {
						t.Errorf("noneditor requesting editor mode saw source %s", file.Path)
					}
				}
				f.visible(1)
			}
		})
	}
}
