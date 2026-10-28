package critical_test

import (
	"bytes"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	catalogstore "github.com/open-rails/contentkit/internal/catalogstore"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/openrails/billing"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

// The gateway fence: a deleted version serves none of its files, public or
// private, to anyone, its buyer included, from the moment the deletion
// commits. Bytes stay in place; a restore reopens them to the same purchase.
// Purge erases them, and its permanent fence outlives any restore attempt.
func TestDeletedMediaFence(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	b := attachBilling(t, w, a)
	m := attachMedia(t, w, a, func(c *ck.Config) {
		c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
	admin := a.engine.Client()
	rule := access.Rule{Level: access.PPV}
	item, err := admin.PostContents(t.Context(), catalog.ContentInput{Kind: "gallery", Access: &rule, Versions: []catalog.VersionInput{
		{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Fenced gallery"}}},
		{Languages: []string{"de"}, Text: map[string]catalog.VersionText{"en": {Title: "Fenced gallery"}}},
	}})
	must(t, err)
	first, second := item.Versions[0].ID, item.Versions[1].ID
	for _, version := range []string{first, second} {
		ref := media.RefBody{Kind: "gallery", ID: item.ID, Version: version}
		_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{
			{Path: "pages/01.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))},
			{Path: "cover.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))},
		})
		must(t, err)
		awaitMediaReady(t, admin, ref)
	}
	_, err = admin.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	for _, version := range []string{first, second} {
		awaitEditionPublication(t, a, admin, item.ID, version)
	}

	key, err := access.VersionKey(a.cfg.Scope.AppID.String(), item.ID, first)
	must(t, err)
	person, credential := w.person(t, 0)
	customer, err := billing.ParseCustomerID(person.ID)
	must(t, err)
	_, err = b.merchant.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
	must(t, err)
	product, err := b.merchant.CreateProduct(t.Context(), billing.CreateProductParams{Key: uuid.NewString(), DisplayName: "Fenced edition", Entitlements: []string{key.String()}})
	must(t, err)
	purchase, err := b.merchant.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: uuid.NewString(), Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID}}})
	must(t, err)
	if len(purchase) != 1 {
		t.Fatalf("purchase returned %d grants", len(purchase))
	}
	buyer := a.client(credential)

	root := layout.Scope{TenantID: a.cfg.Scope.TenantID.String(), AppID: a.cfg.Scope.AppID.String()}.Root()
	folder := func(version string) string { return root + "gallery/" + version + "/" }
	// served lists a version's public/ and private/ keys, without its fence.
	served := func(version string) []string {
		t.Helper()
		var keys []string
		for _, area := range []string{layout.AreaPublic, layout.AreaPrivate} {
			out, err := m.management.ListObjectsV2(t.Context(), &awss3.ListObjectsV2Input{Bucket: &m.bucket, Prefix: aws.String(folder(version) + area + "/")})
			must(t, err)
			for _, object := range out.Contents {
				if k := aws.ToString(object.Key); !strings.HasSuffix(k, "/"+layout.FenceName) {
					keys = append(keys, k)
				}
			}
		}
		slices.Sort(keys)
		return keys
	}
	cover := func(version string) string {
		t.Helper()
		for _, k := range served(version) {
			if strings.HasPrefix(k, folder(version)+"public/") {
				return m.gateway.URL + layout.URLPrefix + k
			}
		}
		t.Fatalf("version %s has no public cover", version)
		return ""
	}
	ring, err := token.NewRing(m.key, nil)
	must(t, err)
	// private is a version's private page under a fresh item token, so only
	// the fence can refuse it.
	private := func(version string) string {
		t.Helper()
		for _, k := range served(version) {
			if strings.HasPrefix(k, folder(version)+"private/") {
				return m.gateway.URL + layout.URLPrefix + k + "?t=" + url.QueryEscape(ring.Sign(strings.TrimSuffix(folder(version), "/"), time.Now().Add(time.Minute)))
			}
		}
		t.Fatalf("version %s has no private page", version)
		return ""
	}
	page := func() string {
		t.Helper()
		got, err := buyer.GetMediaVersion(t.Context(), "gallery", item.ID, first, ck.GetMediaVersionQuery{})
		must(t, err)
		if len(got.Files) != 1 || got.Files[0].URL == "" || !strings.Contains(got.Files[0].URL, "/private/") {
			t.Fatalf("buyer lacks the private page: %+v", got)
		}
		return got.Files[0].URL
	}
	status := func(address string) int {
		t.Helper()
		response, err := http.Get(address)
		must(t, err)
		response.Body.Close()
		return response.StatusCode
	}
	expect := func(want int, addresses ...string) {
		t.Helper()
		for _, address := range addresses {
			if got := status(address); got != want {
				t.Fatalf("%s returned HTTP %d, want %d", address, got, want)
			}
		}
	}
	purchased := func() {
		t.Helper()
		rights, err := b.merchant.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{key.String()}})
		must(t, err)
		if !rights.Entitlements[key.String()] {
			t.Fatal("deletion changed the purchase")
		}
		kept, err := b.merchant.GetProduct(t.Context(), product.ID)
		must(t, err)
		if kept.ID != product.ID {
			t.Fatal("deletion removed the purchased product")
		}
	}
	fence := func(version string) (string, bool) {
		t.Helper()
		out, err := m.management.HeadObject(t.Context(), &awss3.HeadObjectInput{Bucket: &m.bucket, Key: aws.String(folder(version) + "public/" + layout.FenceName)})
		if err != nil {
			return "", false
		}
		return out.Metadata[layout.FenceMeta], true
	}

	firstCover, secondCover := cover(first), cover(second)
	before := served(first)

	t.Run("CK-J08-S08_deleting_a_version_closes_its_files_to_everyone_at_once", func(t *testing.T) {
		// The buyer's own freshly signed URL, so only the fence can refuse it.
		bought := page()
		expect(http.StatusOK, firstCover, secondCover, bought, private(first))
		must(t, admin.DeleteContentVersion(t.Context(), item.ID, first))
		// No job has run yet: the deletion's own commit closed the gateway.
		expect(http.StatusNotFound, firstCover, bought, private(first))
		expect(http.StatusOK, secondCover, private(second))
		if kind, ok := fence(first); !ok || kind != layout.FenceDeleted {
			t.Fatalf("deleted version has fence %q (present %v)", kind, ok)
		}
		_, err := buyer.GetVersion(t.Context(), first, ck.GetVersionQuery{})
		requireError(t, err, 404, "not_found")
		_, err = buyer.GetMediaVersion(t.Context(), "gallery", item.ID, first, ck.GetMediaVersionQuery{})
		requireError(t, err, 404, "not_found")
		awaitPublicationJobsIdle(t, w, a)
		expect(http.StatusNotFound, firstCover, private(first))
		if after := served(first); !slices.Equal(after, before) {
			t.Fatalf("deletion moved served bytes: %v, was %v", after, before)
		}
		purchased()
	})
	t.Run("CK-J08-S09_restoring_a_version_reopens_it_to_the_same_purchase", func(t *testing.T) {
		_, err := admin.PatchContentVersion(t.Context(), item.ID, first, catalog.VersionInput{State: "published"})
		must(t, err)
		expect(http.StatusOK, firstCover, private(first))
		if kind, ok := fence(first); ok {
			t.Fatalf("restore kept the %q fence", kind)
		}
		awaitEditionPublication(t, a, admin, item.ID, first)
		awaitPublicationJobsIdle(t, w, a)
		// No new grant: the original purchase opens it again.
		expect(http.StatusOK, firstCover, page())
		got, err := buyer.GetVersion(t.Context(), first, ck.GetVersionQuery{})
		must(t, err)
		if !got.Decision.Allowed {
			t.Fatalf("restored edition refused its buyer: %+v", got.Decision)
		}
		if after := served(first); !slices.Equal(after, before) {
			t.Fatalf("restore moved served bytes: %v, was %v", after, before)
		}
		purchased()
	})
	t.Run("CK-J08-S10_content_deletion_closes_every_version_and_restore_only_the_restored", func(t *testing.T) {
		must(t, admin.DeleteContentVersion(t.Context(), item.ID, second))
		must(t, admin.DeleteContentVersion(t.Context(), item.ID, first))
		deleted, err := admin.GetManage(t.Context(), ck.GetManageQuery{State: "deleted", Ids: []string{item.ID}})
		must(t, err)
		if len(deleted) != 1 {
			t.Fatal("deleting the last version did not delete the content")
		}
		expect(http.StatusNotFound, firstCover, secondCover, private(first), private(second))
		// Restoring one version restores the content; the other stays closed.
		_, err = admin.PatchContentVersion(t.Context(), item.ID, first, catalog.VersionInput{State: "published"})
		must(t, err)
		expect(http.StatusOK, firstCover, private(first))
		expect(http.StatusNotFound, secondCover, private(second))
		awaitPublicationJobsIdle(t, w, a)
		expect(http.StatusOK, firstCover, private(first))
		expect(http.StatusNotFound, secondCover, private(second))
		purchased()
	})
	t.Run("CK-J08-S11_purge_erases_and_its_fence_outlives_restore", func(t *testing.T) {
		var deletedAt time.Time
		must(t, w.pool.QueryRow(t.Context(), "SELECT deleted_at FROM "+pgx.Identifier{w.schema, "content_versions"}.Sanitize()+" WHERE tenant_id=$1 AND id=$2",
			a.cfg.Scope.AppID.String(), second).Scan(&deletedAt))
		w.clock.set(deletedAt.Add(catalog.DefaultDeletedRetention + time.Microsecond))
		producer, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
		must(t, err)
		job, err := producer.Insert(t.Context(), scheduledContentJob{kind: "contentkit_content_purge_deleted"}, &river.InsertOpts{Queue: catalogstore.QueueFor(a.cfg.Scope.AppID.String()), MaxAttempts: 1})
		must(t, err)
		waitForWithin(t, 45*time.Second, func() bool {
			row, err := producer.JobGet(t.Context(), job.Job.ID)
			must(t, err)
			if row.State == rivertype.JobStateDiscarded {
				t.Fatalf("purge failed: %+v", row.Errors)
			}
			return row.State == rivertype.JobStateCompleted
		})
		if left := served(second); len(left) != 0 {
			t.Fatalf("purge left served files: %v", left)
		}
		if kind, ok := fence(second); !ok || kind != layout.FenceErased {
			t.Fatalf("purged version has fence %q (present %v)", kind, ok)
		}
		_, err = admin.PatchContentVersion(t.Context(), item.ID, second, catalog.VersionInput{State: "published"})
		requireError(t, err, 404, "not_found")
		// Even a stale restore's fence sync leaves an erasure in place.
		config, err := a.cfg.MediaKinds()
		must(t, err)
		registry, err := mediastore.NewRegistry(config, mediastore.Hooks{})
		must(t, err)
		journal, err := mediastore.NewPGJournal(w.pool, w.schema, nil)
		must(t, err)
		jobs, err := mediastore.NewJobs(mediastore.JobsConfig{Store: m.store, Registry: registry, Locker: mediastore.PGLocker(w.pool), Journal: journal})
		must(t, err)
		ref, err := registry.VersionRef("gallery", item.ID, second)
		must(t, err)
		must(t, jobs.FenceDeleted(t.Context(), ref, false))
		if kind, ok := fence(second); !ok || kind != layout.FenceErased {
			t.Fatalf("a restore sync lifted the erasure fence: %q (present %v)", kind, ok)
		}
		expect(http.StatusNotFound, secondCover)
		expect(http.StatusOK, firstCover, page())
		purchased()
	})
}
