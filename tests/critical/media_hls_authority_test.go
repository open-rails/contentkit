package critical_test

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"testing"

	ck "github.com/open-rails/contentkit"
)

func TestMediaHLSAuthorization(t *testing.T) {
	t.Run("CK-J13-S07_HLS_children_and_segment_capabilities_remain_content_scoped", func(t *testing.T) {
		f := newTinyHLSFixture(t)
		readSegment := func(address string, want int) []byte {
			t.Helper()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, address, nil)
			must(t, err)
			req.Header.Set("Range", "bytes=0-31")
			response, err := http.DefaultClient.Do(req)
			must(t, err)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			must(t, err)
			if response.StatusCode != want {
				t.Fatalf("segment response %d want%d: %s", response.StatusCode, want, body)
			}
			if want == 206 && (len(body) != 32 || !bytes.Contains(body, []byte("ftyp"))) {
				t.Fatalf("HLS initialization bytes are not real MP4: %q", body)
			}
			if want == 404 && response.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("private refusal was cacheable: %v", response.Header)
			}
			return body
		}
		readSegment(f.SegmentURL, 206)
		// The unversioned API serves non-content media kinds. It is not a
		// parent/default-edition alias for content kinds that require versions.
		site := f.addSiteClip(t)
		for _, playlist := range []string{f.MasterPath, f.ChildPath} {
			read, err := f.app.engine.Client().GetMediaHls(t.Context(), site.Kind, site.ID, playlist, ck.GetMediaHlsQuery{})
			must(t, err)
			if read.Status != 200 || !bytes.HasPrefix(read.Body, []byte("#EXTM3U")) {
				t.Fatalf("unversioned HLS contract failed: %+v", read)
			}
			_, err = f.app.client("").GetMediaHls(t.Context(), site.Kind, site.ID, playlist, ck.GetMediaHlsQuery{})
			requireError(t, err, 404, "not_found")
		}
		_, err := f.app.engine.Client().GetMediaHls(t.Context(), f.Ref.Kind, f.Ref.ID, f.MasterPath, ck.GetMediaHlsQuery{})
		requireError(t, err, 404, "not_found")
		firstURL, err := url.Parse(f.SegmentURL)
		must(t, err)
		unsigned := *firstURL
		unsigned.RawQuery = ""
		readSegment(unsigned.String(), 404)
		other := f.addClip(t, "Another real HLS item")
		child, err := f.app.client("").GetMediaVersionHls(t.Context(), other.Kind, other.ID, other.Version, f.ChildPath, ck.GetMediaVersionHlsQuery{})
		must(t, err)
		otherURL, err := url.Parse(playlistURIForTest(t, child.Body))
		must(t, err)
		readSegment(otherURL.String(), 206)
		otherURL.RawQuery = firstURL.RawQuery
		readSegment(otherURL.String(), 404)
		_, err = f.app.client("").GetMediaVersionHls(t.Context(), f.Ref.Kind, f.Ref.ID, other.Version, f.ChildPath, ck.GetMediaVersionHlsQuery{})
		requireError(t, err, 404, "not_found")
		_, err = f.app.engine.Client().PostContentUnpublish(t.Context(), f.Ref.ID)
		must(t, err)
		for _, playlist := range []string{f.MasterPath, f.ChildPath} {
			_, err = f.app.client("").GetMediaVersionHls(t.Context(), f.Ref.Kind, f.Ref.ID, f.Ref.Version, playlist, ck.GetMediaVersionHlsQuery{})
			requireError(t, err, 404, "not_found")
		}
		// The other item's positive control stays readable after this item's
		// availability changes. Previously issued segment tokens are bounded
		// capabilities; this case does not promise immediate token revocation.
		_, err = f.app.client("").GetMediaVersionHls(t.Context(), other.Kind, other.ID, other.Version, f.ChildPath, ck.GetMediaVersionHlsQuery{})
		must(t, err)
	})
}
