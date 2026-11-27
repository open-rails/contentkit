package critical_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

// A download link is a ticket rechecked on every transfer. Deleting its
// version closes it at once with 404, never 202 pending: the release a
// deletion keeps for its restore never becomes ready. Staff asking for the
// deleted version's download or reader plan get 404 too. A restore opens the
// same ticket again; deleting one version closes only its own links.
func TestDeletedVersionDownloads(t *testing.T) {
	t.Run("CK-J08-S13_deleted_pages_ZIP_links_are_not_found_until_restored", func(t *testing.T) {
		w := newWorld(t)
		a := w.apps[0]
		attachMedia(t, w, a, func(c *ck.Config) {
			c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
			c.Media.Downloads.TTL = time.Hour // tickets outlive every deletion and restore below
		})
		admin := a.engine.Client()
		item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery", Access: &access.Rule{Level: access.Public}, Versions: []catalog.VersionInput{
			{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Deleted downloads"}}},
			{Languages: []string{"de"}, Text: map[string]catalog.VersionText{"en": {Title: "Deleted downloads"}}},
		}})
		must(t, err)
		first, second := item.Versions[0].ID, item.Versions[1].ID
		for i, version := range []string{first, second} {
			ref := media.RefBody{Kind: "gallery", ID: item.ID, Version: version}
			_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{
				{Path: "pages/01.png", Type: "image/png", Body: bytes.NewReader(chapterImage(t, uint8(31+i)))},
			})
			must(t, err)
			awaitMediaReady(t, admin, ref)
		}
		_, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
		must(t, err)
		for _, version := range []string{first, second} {
			awaitEditionPublication(t, a, admin, item.ID, version)
		}
		d := versionDownloads{t: t, app: a, content: item.ID}
		reader := a.client("")
		links := map[string]string{first: d.grant(reader, first), second: d.grant(reader, second)}
		editor := d.grant(admin, first)
		zips := map[string][]byte{first: d.open(first, links[first]), second: d.open(second, links[second])}
		d.serves(first, editor, zips[first])
		closed := func(version string) {
			t.Helper()
			d.notFound(version, links[version], "")
			d.notFound(version, links[version], "bytes=0-9")
			d.refused(reader, version)
			d.refused(admin, version)
		}

		// Deleting one version closes only its links, at once and after its jobs.
		must(t, admin.DeleteContentVersion(t.Context(), item.ID, first))
		closed(first)
		d.notFound(first, editor, "")
		d.serves(second, links[second], zips[second])
		awaitPublicationJobsIdle(t, w, a)
		closed(first)
		d.serves(second, links[second], zips[second])

		// A restore opens the same ticket again.
		_, err = admin.PatchContentVersion(t.Context(), item.ID, first, catalog.VersionInput{State: "published"})
		must(t, err)
		awaitEditionPublication(t, a, admin, item.ID, first)
		d.reopens(first, links[first], zips[first])
		d.serves(first, editor, zips[first])
		d.serves(first, d.grant(reader, first), zips[first])

		// Deleting the last version deletes the content and closes every link.
		must(t, admin.DeleteContentVersion(t.Context(), item.ID, second))
		must(t, admin.DeleteContentVersion(t.Context(), item.ID, first))
		deleted, err := admin.GetManage(t.Context(), ck.GetManageQuery{State: "deleted", Ids: []string{item.ID}})
		must(t, err)
		if len(deleted) != 1 {
			t.Fatal("deleting the last version did not delete the content")
		}
		closed(first)
		closed(second)
		d.notFound(first, editor, "")
		awaitPublicationJobsIdle(t, w, a)
		closed(first)
		closed(second)

		// Restoring one version restores the content and reopens only it.
		_, err = admin.PatchContentVersion(t.Context(), item.ID, first, catalog.VersionInput{State: "published"})
		must(t, err)
		awaitEditionPublication(t, a, admin, item.ID, first)
		d.reopens(first, links[first], zips[first])
		closed(second)
	})
	t.Run("CK-J08-S13_a_deleted_last_version's_original_link_is_not_found_until_restored", func(t *testing.T) {
		f := newTinyHLSConfiguredFixture(t, false, func(c *ck.Config) { c.Media.Downloads.TTL = time.Hour })
		a, admin, reader := f.app, f.app.engine.Client(), f.app.client("")
		version := f.Ref.Version
		d := versionDownloads{t: t, app: a, content: f.Ref.ID}
		link := d.grant(reader, version)
		d.serves(version, link, f.Source)
		closed := func() {
			t.Helper()
			d.notFound(version, link, "")
			d.notFound(version, link, "bytes=10-99")
			d.refused(reader, version)
			d.refused(admin, version)
		}
		// A clip has one version: deleting it deletes the clip.
		must(t, admin.DeleteContentVersion(t.Context(), f.Ref.ID, version))
		closed()
		awaitDuePublications(t, a)
		closed()
		_, err := admin.PatchContentVersion(t.Context(), f.Ref.ID, version, catalog.VersionInput{State: "published"})
		must(t, err)
		awaitEditionPublication(t, a, admin, f.Ref.ID, version)
		d.reopens(version, link, f.Source)
	})
}

// versionDownloads grants and transfers one content's version downloads.
type versionDownloads struct {
	t       *testing.T
	app     *application
	content string
}

// grant is c's download of version, once it is prepared: its ticketed link.
func (d versionDownloads) grant(c *ck.Client, version string) string {
	d.t.Helper()
	var grant versions.FileDownload
	waitForWithin(d.t, 45*time.Second, func() bool {
		reply, err := c.GetContentVersionDownload(d.t.Context(), d.content, version)
		must(d.t, err)
		if reply.Status == http.StatusAccepted {
			return false
		}
		must(d.t, reply.Decode(&grant))
		return grant.URL != ""
	})
	return grant.URL
}

// transfer fetches link, relative to version's download route.
func (d versionDownloads) transfer(version, link, rangeHeader string) (int, []byte) {
	d.t.Helper()
	base, err := url.Parse(d.app.server.URL + "/content/v1/contents/" + d.content + "/versions/" + version + "/download")
	must(d.t, err)
	target, err := base.Parse(link)
	must(d.t, err)
	req, err := http.NewRequestWithContext(d.t.Context(), http.MethodGet, target.String(), nil)
	must(d.t, err)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	res, err := http.DefaultClient.Do(req)
	must(d.t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	must(d.t, err)
	return res.StatusCode, body
}

func (d versionDownloads) open(version, link string) []byte {
	d.t.Helper()
	status, body := d.transfer(version, link, "")
	if status != http.StatusOK || len(body) == 0 {
		d.t.Fatalf("version %s download: HTTP %d with %d bytes", version, status, len(body))
	}
	return body
}

func (d versionDownloads) serves(version, link string, want []byte) {
	d.t.Helper()
	if got := d.open(version, link); !bytes.Equal(got, want) {
		d.t.Fatalf("version %s download changed: %d bytes, want %d", version, len(got), len(want))
	}
}

// notFound requires link refused outright, not pending. Closed checks
// report every miss before the test moves on.
func (d versionDownloads) notFound(version, link, rangeHeader string) {
	d.t.Helper()
	if status, body := d.transfer(version, link, rangeHeader); status != http.StatusNotFound {
		d.t.Errorf("deleted version %s download (range %q): HTTP %d, want 404: %.200s", version, rangeHeader, status, body)
	}
}

// refused requires c's download grant and reader plan of version not found.
func (d versionDownloads) refused(c *ck.Client, version string) {
	d.t.Helper()
	for _, route := range []struct {
		name string
		call func() (*ck.Reply, error)
	}{
		{"download grant", func() (*ck.Reply, error) { return c.GetContentVersionDownload(d.t.Context(), d.content, version) }},
		{"reader plan", func() (*ck.Reply, error) { return c.GetContentVersionReader(d.t.Context(), d.content, version) }},
	} {
		reply, err := route.call()
		var got *ck.Error
		switch {
		case err == nil:
			d.t.Errorf("deleted version %s %s: HTTP %d, want 404: %.200s", version, route.name, reply.Status, reply.Body)
		case !errors.As(err, &got) || got.Status != http.StatusNotFound || got.Code != "not_found":
			d.t.Errorf("deleted version %s %s: %v, want 404 not_found", version, route.name, err)
		}
	}
}

// reopens waits for a restored version's old link to serve want again.
func (d versionDownloads) reopens(version, link string, want []byte) {
	d.t.Helper()
	waitForWithin(d.t, 45*time.Second, func() bool {
		status, _ := d.transfer(version, link, "")
		return status == http.StatusOK
	})
	d.serves(version, link, want)
}
