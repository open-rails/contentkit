package critical_test

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
)

func TestTypeScriptVideoContracts(t *testing.T) {
	t.Run("media_hls_transport", func(t *testing.T) {
		f := newTinyHLSFixture(t)
		owner, token := f.world.person(t, 0)
		_, reader := f.world.person(t, 0)
		persona, err := f.world.auth.Persona("app")
		must(t, err)
		authtest.GrantRole(t, f.world.tenant[0], f.app.group, iam.UserSubject(owner.ID), persona.OwnerRole())
		_, err = f.app.engine.Client().PatchContent(t.Context(), f.Ref.ID, catalog.ContentPatch{Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}}})
		must(t, err)
		upstream, err := url.Parse(f.storage.gateway.URL)
		must(t, err)
		forward := httputil.NewSingleHostReverseProxy(upstream)
		var leaked atomic.Bool
		segment := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				leaked.Store(true)
			}
			forward.ServeHTTP(w, r)
		}))
		t.Cleanup(segment.Close)
		source, err := url.Parse(f.SegmentURL)
		must(t, err)
		segmentURL := segment.URL + source.RequestURI()
		frameFile := filepath.Join(t.TempDir(), "frame.jpg")
		runTypeScriptContract(t, "media_hls_transport", map[string]any{"base": f.app.server.URL + "/content", "owner": token, "reader": reader, "ref": f.Ref, "master": f.MasterPath, "child": f.ChildPath, "segment": segmentURL, "source": base64.StdEncoding.EncodeToString(f.Source), "sourceSHA": digestBytes(f.Source), "frameFile": frameFile}, []string{"MediaClient.getFrame", "MediaClient.hlsBase", "MediaClient.hls", "xhrTransport"})
		if leaked.Load() {
			t.Fatal("API bearer leaked to off-origin media transport")
		}
		frame, err := os.ReadFile(frameFile)
		must(t, err)
		dimensions, codec, err := image.DecodeConfig(bytes.NewReader(frame))
		must(t, err)
		if codec != "jpeg" || dimensions.Width != 128 || dimensions.Height != 72 {
			t.Fatalf("actual video frame is %s %dx%d, want JPEG128x72", codec, dimensions.Width, dimensions.Height)
		}
	})
}
