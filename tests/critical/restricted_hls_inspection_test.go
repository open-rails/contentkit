package critical_test

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"testing"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/media"
)

func TestRestrictedDraftHLSInspection(t *testing.T) {
	f := newTinyHLSConfiguredFixture(t, false, func(c *ck.Config) {
		for i := range c.Media.Kinds {
			if c.Media.Kinds[i].Name == "clip" {
				c.Media.Kinds[i].Private = append(c.Media.Kinds[i].Private, media.Private{Name: "export", From: "source", To: "export/{name}.mp4", HostOnly: true, MP4: media.Rung(144)})
			}
		}
	})
	ref := f.addClip(t, "Auditor prepared HLS", false)
	auditor, credential := f.person(t, 0)
	authtest.GrantRole(t, f.tenant[0], f.app.group, iam.UserSubject(auditor.ID), f.viewer)
	reader := f.app.client(credential)
	_, err := f.app.client("").GetMediaVersionHls(t.Context(), ref.Kind, ref.ID, ref.Version, f.MasterPath, ck.GetMediaVersionHlsQuery{Editor: true})
	requireError(t, err, 404, "not_found")
	_, err = reader.GetMediaVersionHls(t.Context(), ref.Kind, ref.ID, ref.Version, f.MasterPath, ck.GetMediaVersionHlsQuery{})
	requireError(t, err, 404, "not_found")
	before := 0
	must(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+f.schema+`.content_publication_requests WHERE tenant_id=$1 AND version_id=$2`, f.app.cfg.Scope.AppID.String(), ref.Version).Scan(&before))
	read, err := reader.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
	must(t, err)
	if !read.Inspection || read.Cookie != nil || len(read.HLS) != 1 || len(read.Uploads) != 0 {
		t.Fatalf("auditor inspection lost HLS or gained editing controls: %+v", read)
	}
	for _, file := range read.Files {
		if file.Path == "source.mp4" || strings.HasPrefix(file.Path, "export/") || file.EditorURL != "" || file.Upload {
			t.Fatal("auditor HLS view exposed source or HostOnly files")
		}
	}
	master, err := reader.GetMediaVersionHls(t.Context(), ref.Kind, ref.ID, ref.Version, f.MasterPath, ck.GetMediaVersionHlsQuery{Editor: true})
	must(t, err)
	child, err := url.Parse(playlistURIForTest(t, master.Body))
	must(t, err)
	if !child.Query().Has("editor") {
		t.Fatal("inspection master lost explicit authority context on its child URL")
	}
	childPath := path.Join(path.Dir(f.MasterPath), child.Path)
	playlist, err := reader.GetMediaVersionHls(t.Context(), ref.Kind, ref.ID, ref.Version, childPath, ck.GetMediaVersionHlsQuery{Editor: true})
	must(t, err)
	segmentURL := playlistURIForTest(t, playlist.Body)
	parsed, err := url.Parse(segmentURL)
	must(t, err)
	ttl, err := strconv.Atoi(parsed.Query().Get("X-Amz-Expires"))
	must(t, err)
	if ttl <= 0 || ttl > 300 || parsed.Query().Get("X-Amz-Signature") == "" || parsed.Query().Get("t") != "" || !strings.Contains(parsed.Path, "/internal/") {
		t.Fatal("prepared HLS bytes received a broad or unbounded capability")
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, segmentURL, nil)
	must(t, err)
	request.Header.Set("Range", "bytes=0-31")
	response, err := http.DefaultClient.Do(request)
	must(t, err)
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, err)
	if response.StatusCode != 206 || len(data) != 32 || !bytes.Contains(data, []byte("ftyp")) {
		t.Fatalf("inspection did not stream actual prepared MP4 bytes: HTTP%d %q", response.StatusCode, data)
	}
	_, err = reader.GetMediaVersionHls(t.Context(), ref.Kind, ref.ID, ref.Version, childPath, ck.GetMediaVersionHlsQuery{})
	requireError(t, err, 404, "not_found")
	after := 0
	var released bool
	must(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+f.schema+`.content_publication_requests WHERE tenant_id=$1 AND version_id=$2`, f.app.cfg.Scope.AppID.String(), ref.Version).Scan(&after))
	must(t, f.pool.QueryRow(t.Context(), `SELECT released FROM `+f.schema+`.content_versions WHERE tenant_id=$1 AND id=$2`, f.app.cfg.Scope.AppID.String(), ref.Version).Scan(&released))
	if before != after || released {
		t.Fatal("auditor HLS inspection changed publication state")
	}
	authtest.RevokeRole(t, f.tenant[0], f.app.group, iam.UserSubject(auditor.ID), f.viewer)
	_, err = reader.GetMediaVersionHls(t.Context(), ref.Kind, ref.ID, ref.Version, childPath, ck.GetMediaVersionHlsQuery{Editor: true})
	requireError(t, err, 404, "not_found")
	_, err = reader.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
	requireError(t, err, 404, "not_found")
}
