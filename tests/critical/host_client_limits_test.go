package critical_test

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
)

// Engine.Client is the host, not a viewer: per-viewer read and interaction
// limits never throttle it. Every credential stays limited, including a staff
// key holding the same authority over HTTP.
func TestHostClientSkipsViewerLimits(t *testing.T) {
	t.Run("CK-J03-S74_host_reads_past_the_media_read_limit", func(t *testing.T) {
		w := newWorld(t)
		a := w.apps[0]
		attachMedia(t, w, a, func(c *ck.Config) {
			c.Media.ReadLimit = ck.RateLimitConfig{PerSecond: 0.01, Burst: 2}
		})
		host := a.engine.Client()
		ref := galleryRef(t, host, "Host read limit")
		_, err := host.IngestMedia(t.Context(), nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}})
		must(t, err)
		awaitMediaReady(t, host, ref)
		_, err = host.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
		must(t, err)
		awaitEditionPublication(t, a, host, ref.ID, ref.Version)
		for range 10 {
			read, err := host.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
			must(t, err)
			requireMediaPaths(t, read, "reader/one.webp")
		}
		persona, err := w.auth.Persona("app")
		must(t, err)
		key, err := w.tenant[0].CreateAPIKey(t.Context(), iam.SystemIdentity(), a.group, iam.NewAPIKey{Name: "Limited staff reader", Role: persona.OwnerRole()})
		must(t, err)
		staff := a.client(key.Secret)
		for range 2 {
			_, err := staff.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
			must(t, err)
		}
		_, err = staff.GetMediaVersion(t.Context(), ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{})
		requireError(t, err, 429, "rate_limited")
	})
	t.Run("CK-J03-S74_host_edits_past_the_comment_limit", func(t *testing.T) {
		w := newWorld(t, func(cfg *ck.Config) {
			cfg.Interactions.Limits.Disabled = false
			cfg.Interactions.Limits.Comment = []ck.Rate{{Count: 2, Per: time.Hour}}
		})
		a := w.apps[0]
		_, token := w.person(t, 0)
		person := a.client(token)
		item := publishedArticle(t, a, "Host comment edits")
		cm := postComment(t, person, item.ID, "Original")
		host := a.engine.Client()
		for i := range 4 {
			_, err := host.PatchComment(t.Context(), cm.ID, content.CommentEdit{Body: fmt.Sprintf("Moderated %d", i)})
			must(t, err)
		}
		postComment(t, person, item.ID, "Second allowance")
		_, err := person.PostContentComments(t.Context(), item.ID, content.CommentInput{Body: "No third allowance"}, ck.PostContentCommentsQuery{})
		requireError(t, err, 429, "rate_limited")
	})
}
