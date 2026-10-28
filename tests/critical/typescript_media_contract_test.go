package critical_test

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
)

func typeScriptMediaWorld(t *testing.T) (*world, *application) {
	t.Helper()
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a, func(c *ck.Config) {
		c.Media.Delivery.TTL = 5 * time.Minute
		kind := &c.Media.Config.Kinds[0]

		kind.Private = append(kind.Private,
			media.Private{Name: "archive-pages", From: "pages/{name}", To: "download-pages/{name}{ext}", HostOnly: true, Image: &media.Image{Format: media.ImageFormatSource}},
			media.Private{Name: "archive", To: "pages.zip", Zip: "download-pages/", HostOnly: true})
		kind.Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}, Download: "archive"}
		c.Media.Config.Kinds = append(c.Media.Config.Kinds, media.Kind{Name: "article", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "named/{name}", Named: true, Max: 4, Types: []string{"image/png"}, MaxBytes: 1 << 20}, {Path: "body/{name}", Named: true, Body: true, Max: 4, Types: []string{"image/png"}, MaxBytes: 1 << 20}},
			Private: []media.Private{{Name: "named-reader", From: "named/{name}", To: "named-reader/{name}.webp", Image: media.Fit(48)}, {Name: "body-reader", From: "body/{name}", To: "body-reader/{name}.webp", Image: media.Fit(48)}},
		})
	})
	return w, a
}

func typeScriptMediaFixture(t *testing.T, w *world, a *application) map[string]any {
	t.Helper()
	owner, token := w.person(t, 0)
	_, reader := w.person(t, 0)
	persona, err := w.auth.Persona("app")
	must(t, err)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(owner.ID), persona.OwnerRole())
	ref := galleryRef(t, a.client(token), "TypeScript image contracts")
	data := imageBytes(t)
	return map[string]any{"base": a.server.URL + "/content", "owner": token, "reader": reader, "ref": ref, "png": base64.StdEncoding.EncodeToString(data), "sha256": digestBytes(data)}
}

func TestTypeScriptMediaContracts(t *testing.T) {
	w, a := typeScriptMediaWorld(t)
	t.Run("media_image_io", func(t *testing.T) {
		fixture := typeScriptMediaFixture(t, w, a)
		runTypeScriptContract(t, "media_image_io", fixture, []string{"MediaApi.commit", "MediaApi.presets", "MediaApi.itemURL", "MediaClient.upload", "MediaClient.put", "MediaClient.commit", "MediaClient.waitFor", "MediaClient.editorView", "MediaClient.presets", "MediaClient.presetPath", "MediaClient.pollRef", "defaultTransport", "fetchTransport"})
	})
	t.Run("media_named_inline", func(t *testing.T) {
		fixture := typeScriptMediaFixture(t, w, a)
		item, err := a.client(fixture["owner"].(string)).PostContents(t.Context(), catalog.ContentInput{Kind: "article", Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Inline image post"}}}}})
		must(t, err)
		fixture["inlineRef"] = media.RefBody{Kind: "article", ID: item.ID, Version: item.Versions[0].ID}
		runTypeScriptContract(t, "media_named_inline", fixture, []string{"MediaClient.uploadNamed", "MediaClient.uploadInline", "VersionsClient.download"})
	})
	t.Run("media_abort", func(t *testing.T) {
		fixture := typeScriptMediaFixture(t, w, a)
		runTypeScriptContract(t, "media_abort", fixture, []string{"MediaApi.abort", "MediaClient.discard"})
	})

}
