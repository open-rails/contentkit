package critical_test

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
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
		_, err = f.app.engine.Client().PatchContentVersion(t.Context(), f.Ref.ID, f.Ref.Version, catalog.VersionInput{Access: &access.Rule{Level: access.TierLevel, Tiers: []string{"gold"}}})
		must(t, err)
		// Policy acceptance leaves the prior public release usable until the
		// durable worker copies, activates and retires its private replacement.
		waitForChapterPublication(t, func() bool {
			var completed bool
			must(t, f.world.pool.QueryRow(t.Context(), `SELECT v.released_access->>'level'='tier' AND r.state='completed'
 FROM `+f.world.schema+`.content_versions v JOIN `+f.world.schema+`.content_publication_requests r
 ON r.tenant_id=v.tenant_id AND r.id=v.release_id WHERE v.tenant_id=$1 AND v.id=$2`,
				f.app.cfg.Scope.AppID.String(), f.Ref.Version).Scan(&completed))
			return completed
		})
		child, err := f.app.client(token).GetMediaVersionHls(t.Context(), f.Ref.Kind, f.Ref.ID, f.Ref.Version, f.ChildPath, ck.GetMediaVersionHlsQuery{})
		must(t, err)
		if child.Status != http.StatusOK {
			t.Fatalf("completed private HLS child: %d %s", child.Status, child.Body)
		}
		freshSegment := playlistURIForTest(t, child.Body)
		if !strings.Contains(freshSegment, "/private/") {
			t.Fatalf("completed tier release did not select private bytes: %s", freshSegment)
		}
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
		source, err := url.Parse(freshSegment)
		must(t, err)
		segmentURL := segment.URL + source.RequestURI()
		runTypeScriptContract(t, "media_hls_transport", map[string]any{"base": f.app.server.URL + "/content", "owner": token, "reader": reader, "ref": f.Ref, "master": f.MasterPath, "child": f.ChildPath, "segment": segmentURL, "source": base64.StdEncoding.EncodeToString(f.Source), "sourceSHA": digestBytes(f.Source)}, []string{"MediaClient.hlsBase", "MediaClient.hls", "xhrTransport"})
		if leaked.Load() {
			t.Fatal("API bearer leaked to off-origin media transport")
		}
	})
}
