package critical_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	catalogstore "github.com/open-rails/contentkit/internal/catalogstore"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/internal/jobhook"
	"github.com/open-rails/contentkit/internal/jobqueue"
	"github.com/open-rails/contentkit/internal/media/workqueue"
	"github.com/open-rails/contentkit/internal/pglock"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	medias3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/openrailstest/stripemock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

func TestDeletedContentPurgeOwnsMedia(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	b := attachBilling(t, w, a)
	m := attachMedia(t, w, a)
	admin := a.engine.Client()
	ref := galleryRef(t, admin, "Purchased gallery to purge")
	_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}})
	must(t, err)
	awaitMediaReady(t, admin, ref)
	_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, admin, ref.ID, ref.Version)
	contentKey, err := access.VersionKey(a.cfg.Scope.AppID.String(), ref.ID, ref.Version)
	must(t, err)
	product, err := b.merchant.CreateProduct(t.Context(), billing.CreateProductParams{Key: "purge-history", DisplayName: "Retained billing product", Entitlements: []string{contentKey.String()}})
	must(t, err)
	price, err := b.merchant.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "purchase", UnitAmount: 5_000_000, Currency: "USD"})
	must(t, err)
	_, credential := w.person(t, 0)
	total := int64(5_000_000)
	method := b.stripe.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
	body, err := json.Marshal(billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: &total, Payment: &billing.OrderPaymentParams{Token: method}})
	must(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, b.server.URL+"/billing/v1/me/orders", bytes.NewReader(body))
	must(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", uuid.NewString())
	authtest.Authorize(t, w.tenant[0], request, credential)
	response, err := b.server.Client().Do(request)
	must(t, err)
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, err)
	if response.StatusCode != 201 {
		t.Fatalf("purchase failed: %d %s", response.StatusCode, data)
	}
	var purchase billing.Order
	must(t, json.Unmarshal(data, &purchase))
	if purchase.Status != billing.OrderComplete || purchase.Payment.PaymentID == nil {
		t.Fatalf("purchase did not settle: %+v", purchase)
	}
	// Reopen only this app's engine through a controllable real S3 wire. The
	// storage service and bytes stay real; a failed DELETE is the only fault.
	endpoint, err := url.Parse(os.Getenv("CONTENTKIT_CRITICAL_S3_ENDPOINT"))
	must(t, err)
	proxy := httputil.NewSingleHostReverseProxy(endpoint)
	var failDelete atomic.Bool
	var deletes atomic.Int64
	var pauseErasure atomic.Pointer[string]
	erasureEntered, releaseErasure := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	wire := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deletion := r.Method == http.MethodDelete || r.Method == http.MethodPost && r.URL.Query().Has("delete")
		// Earlier purges can still have recorded late-write cleanup due. Only
		// this version's erasure fence proves its own durable claim committed.
		if target := pauseErasure.Load(); target != nil && r.Method == http.MethodPut && r.URL.Path == *target && pauseErasure.CompareAndSwap(target, nil) {
			close(erasureEntered)
			select {
			case <-releaseErasure:
			case <-r.Context().Done():
				return
			}
		}
		if deletion && failDelete.Load() {
			deletes.Add(1)
			http.Error(w, "owned fixture storage deletion unavailable", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer func() {
		// A failed assertion must release the handler before Close waits for it.
		releaseOnce.Do(func() { close(releaseErasure) })
		wire.Close()
	}()
	store, err := medias3.New(medias3.Config{Bucket: m.bucket, Endpoint: wire.URL, PublicEndpoint: endpoint.String(), AccessKeyID: os.Getenv("CONTENTKIT_CRITICAL_S3_USER"), SecretAccessKey: os.Getenv("CONTENTKIT_CRITICAL_S3_PASSWORD"), UsePathStyle: true})
	must(t, err)
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.deps.Store = store
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	must(t, a.engine.StartJobs(t.Context()))
	admin = a.engine.Client()
	producer, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
	must(t, err)
	purge := func(t *testing.T, want rivertype.JobState) {
		t.Helper()
		job, err := producer.Insert(t.Context(), scheduledContentJob{kind: "contentkit_content_purge_deleted"}, &river.InsertOpts{Queue: catalogstore.QueueFor(a.cfg.Scope.AppID.String()), MaxAttempts: 1})
		must(t, err)
		waitForWithin(t, 45*time.Second, func() bool {
			current, err := producer.JobGet(t.Context(), job.Job.ID)
			must(t, err)
			if current.State == rivertype.JobStateCompleted || current.State == rivertype.JobStateDiscarded {
				if current.State != want {
					t.Fatalf("purge finished %s, expected %s: %+v", current.State, want, current.Errors)
				}
				return true
			}
			return false
		})
	}
	root := layout.Scope{TenantID: a.cfg.Scope.TenantID.String(), AppID: a.cfg.Scope.AppID.String()}.Root()
	objectCount := func() int {
		objects, err := m.management.ListObjectsV2(t.Context(), &awss3.ListObjectsV2Input{Bucket: &m.bucket, Prefix: aws.String(root)})
		must(t, err)
		count := 0
		for _, object := range objects.Contents {
			// The permanent gateway erasure fence contains no media bytes and
			// deliberately survives the item it prevents from reappearing.
			if !strings.HasSuffix(aws.ToString(object.Key), "/"+layout.FenceName) && !strings.HasSuffix(aws.ToString(object.Key), "/"+layout.ManifestName) {
				count++
			}
		}
		return count
	}
	if objectCount() == 0 {
		t.Fatal("purge positive control has no durable media")
	}
	must(t, admin.DeleteContentVersion(t.Context(), ref.ID, ref.Version))
	awaitPublicationJobsIdle(t, w, a)
	deleted, err := admin.GetManage(t.Context(), ck.GetManageQuery{State: "deleted", Ids: []string{ref.ID}})
	must(t, err)
	if len(deleted) != 1 || deleted[0].DeletedAt == nil {
		t.Fatalf("missing soft-deleted item: %+v", deleted)
	}
	// The public version does not expose its deletion timestamp. Reading it
	// only positions the controlled clock; deletion assertions remain public
	// API reads and actual object-store bytes.
	var versionDeletedAt time.Time
	must(t, w.pool.QueryRow(t.Context(), "SELECT deleted_at FROM "+pgx.Identifier{w.schema, "content_versions"}.Sanitize()+" WHERE tenant_id=$1 AND id=$2", a.cfg.Scope.AppID.String(), ref.Version).Scan(&versionDeletedAt))
	deadline := versionDeletedAt.Add(30 * 24 * time.Hour)
	parentDeadline := deleted[0].DeletedAt.Add(30 * 24 * time.Hour)
	t.Run("CK-J08-S04_exact_retention_boundary_preserves_soft_deleted_records_and_media", func(t *testing.T) {
		w.clock.set(deadline)
		purge(t, rivertype.JobStateCompleted)
		rows, err := admin.GetManage(t.Context(), ck.GetManageQuery{State: "deleted", Ids: []string{ref.ID}})
		must(t, err)
		if len(rows) != 1 || objectCount() == 0 {
			t.Fatal("retention equality purged before its strict deadline")
		}
	})
	t.Run("CK-J08-S05_storage_failure_retains_database_records_and_retry_obligation", func(t *testing.T) {
		w.clock.set(parentDeadline.Add(time.Microsecond))
		failDelete.Store(true)
		purge(t, rivertype.JobStateDiscarded)
		if deletes.Load() == 0 {
			t.Fatal("storage deletion fault was not exercised")
		}
		rows, err := admin.GetManage(t.Context(), ck.GetManageQuery{State: "deleted", Ids: []string{ref.ID}})
		must(t, err)
		if len(rows) != 1 || objectCount() == 0 {
			t.Fatal("storage failure lost source records or erased media")
		}
	})
	t.Run("CK-J08-S04_CK-J08-S05_retried_same_sweep_erases_media_and_records_but_retains_billing", func(t *testing.T) {
		failDelete.Store(false)
		purge(t, rivertype.JobStateCompleted)
		rows, err := admin.GetManage(t.Context(), ck.GetManageQuery{State: "deleted", Ids: []string{ref.ID}})
		must(t, err)
		if len(rows) != 0 || objectCount() != 0 {
			objects, err := m.management.ListObjectsV2(t.Context(), &awss3.ListObjectsV2Input{Bucket: &m.bucket, Prefix: aws.String(root)})
			must(t, err)
			for _, object := range objects.Contents {
				t.Logf("retained object after purge: %s", aws.ToString(object.Key))
			}
			t.Fatalf("completed purge left content=%d or media=%d", len(rows), objectCount())
		}
		objects, err := m.management.ListObjectsV2(t.Context(), &awss3.ListObjectsV2Input{Bucket: &m.bucket, Prefix: aws.String(root)})
		must(t, err)
		for _, object := range objects.Contents {
			if !strings.HasSuffix(aws.ToString(object.Key), "/"+layout.ManifestName) {
				continue
			}
			stored, err := m.management.GetObject(t.Context(), &awss3.GetObjectInput{Bucket: &m.bucket, Key: object.Key})
			must(t, err)
			encoded, err := io.ReadAll(stored.Body)
			stored.Body.Close()
			must(t, err)
			if bytes.HasPrefix(encoded, []byte{0x1f, 0x8b}) {
				compressed, err := gzip.NewReader(bytes.NewReader(encoded))
				must(t, err)
				encoded, err = io.ReadAll(compressed)
				compressed.Close()
				must(t, err)
			}
			var tombstone mediastore.Manifest
			must(t, json.Unmarshal(encoded, &tombstone))
			if !tombstone.Deleted || len(tombstone.Files) != 0 {
				t.Fatal("retained manifest still references erased media")
			}
		}
		_, err = admin.GetContentVersion(t.Context(), ref.ID, ref.Version, ck.GetContentVersionQuery{})
		requireError(t, err, 404, "not_found")
		payment, err := b.merchant.GetPayment(t.Context(), *purchase.Payment.PaymentID)
		must(t, err)
		if payment.Status != billing.PaymentSucceeded || payment.Amount != 5_000_000 {
			t.Fatalf("content purge changed settled payment: %+v", payment)
		}
		order, err := b.merchant.GetOrder(t.Context(), purchase.ID)
		must(t, err)
		if order.ID != purchase.ID || order.Status != billing.OrderComplete {
			t.Fatal("content purge erased purchase history")
		}
		_, err = b.merchant.GetProduct(t.Context(), product.ID)
		must(t, err)
	})
	t.Run("CK-J08-S06_restore_and_durable_purge_claim_have_one_winner", func(t *testing.T) {
		makeDeleted := func(title string) media.RefBody {
			ref := galleryRef(t, admin, title)
			_, err := admin.IngestMedia(t.Context(), http.DefaultClient, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}})
			must(t, err)
			awaitMediaReady(t, admin, ref)
			_, err = admin.PostContentPublish(t.Context(), ref.ID, catalog.PublishInput{})
			must(t, err)
			awaitEditionPublication(t, a, admin, ref.ID, ref.Version)
			must(t, admin.DeleteContentVersion(t.Context(), ref.ID, ref.Version))
			awaitPublicationJobsIdle(t, w, a)
			rows, err := admin.GetManage(t.Context(), ck.GetManageQuery{State: "deleted", Ids: []string{ref.ID}})
			must(t, err)
			if len(rows) != 1 || rows[0].DeletedAt == nil {
				t.Fatal("race fixture lacks deletion timestamp")
			}
			w.clock.set(rows[0].DeletedAt.Add(30*24*time.Hour + time.Microsecond))
			return ref
		}
		restored := makeDeleted("Restore wins")
		qs := pgx.Identifier{w.schema}.Sanitize()
		const barrier int64 = 612900086
		// The deliberate pause must not reserve an application connection:
		// restore and two queued metadata workers already need three of four
		// CI pool slots. Keep the observer and purger on the application pool.
		holder, err := pgx.ConnectConfig(t.Context(), w.pool.Config().ConnConfig.Copy())
		must(t, err)
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = holder.Exec(cleanup, "SELECT pg_advisory_unlock($1)", barrier)
			_ = holder.Close(cleanup)
		}()
		_, err = holder.Exec(t.Context(), "SELECT pg_advisory_lock($1)", barrier)
		must(t, err)
		_, err = w.pool.Exec(t.Context(), fmt.Sprintf(`CREATE FUNCTION %s.critical_pause_restore() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.id=TG_ARGV[0] AND NEW.state='published' THEN PERFORM pg_advisory_xact_lock(%d); END IF; RETURN NEW; END$$;
CREATE TRIGGER critical_pause_restore BEFORE UPDATE ON %s.content_versions FOR EACH ROW EXECUTE FUNCTION %s.critical_pause_restore('%s')`, qs, barrier, qs, qs, restored.Version))
		must(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := admin.PatchContentVersion(ctx, restored.ID, restored.Version, catalog.VersionInput{State: "published"})
			result <- err
		}()
		waitFor(t, func() bool {
			var n int
			must(t, w.pool.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid=$1 AND NOT granted", barrier).Scan(&n))
			return n == 1
		})
		purge(t, rivertype.JobStateCompleted)
		_, err = holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", barrier)
		must(t, err)
		must(t, <-result)
		live, err := admin.GetContent(t.Context(), restored.ID, ck.GetContentQuery{})
		must(t, err)
		if live.DeletedAt != nil || !live.Live {
			t.Fatalf("restore lost after holding the row first: %+v", live)
		}
		read, err := admin.GetMediaVersion(t.Context(), restored.Kind, restored.ID, restored.Version, ck.GetMediaVersionQuery{Editor: true})
		must(t, err)
		if len(read.Files) == 0 {
			t.Fatal("purger erased media of the winning restore")
		}
		claimed := makeDeleted("Purge claim wins")
		fencePath := "/" + m.bucket + "/" + root + claimed.Kind + "/" + claimed.Version + "/public/" + layout.FenceName
		pauseErasure.Store(&fencePath)
		job, err := producer.Insert(t.Context(), scheduledContentJob{kind: "contentkit_content_purge_deleted"}, &river.InsertOpts{Queue: catalogstore.QueueFor(a.cfg.Scope.AppID.String()), MaxAttempts: 1})
		must(t, err)
		select {
		case <-erasureEntered:
		case <-time.After(30 * time.Second):
			t.Fatal("purge never reached the physical storage boundary")
		}
		_, err = admin.PatchContentVersion(t.Context(), claimed.ID, claimed.Version, catalog.VersionInput{State: "published"})
		requireError(t, err, 409, "conflict")
		releaseOnce.Do(func() { close(releaseErasure) })
		waitForWithin(t, 30*time.Second, func() bool {
			row, err := producer.JobGet(t.Context(), job.Job.ID)
			must(t, err)
			if row.State == rivertype.JobStateDiscarded {
				t.Fatalf("claimed purge failed: %+v", row.Errors)
			}
			return row.State == rivertype.JobStateCompleted
		})
		_, err = admin.GetContent(t.Context(), claimed.ID, ck.GetContentQuery{})
		requireError(t, err, 404, "not_found")
		// The refused restore never lifted the fence: erasure replaced the
		// deletion fence for good.
		head, err := m.management.HeadObject(t.Context(), &awss3.HeadObjectInput{Bucket: &m.bucket, Key: aws.String(strings.TrimPrefix(fencePath, "/"+m.bucket+"/"))})
		must(t, err)
		if head.Metadata[layout.FenceMeta] != layout.FenceErased {
			t.Fatalf("erased version kept a removable fence: %v", head.Metadata)
		}
		_, err = admin.GetContent(t.Context(), restored.ID, ck.GetContentQuery{})
		must(t, err)
	})
}

func newPurgeJobs(t *testing.T, w *world, a *application, store media.Store) (*mediastore.Registry, *mediastore.Jobs) {
	t.Helper()
	config, err := a.cfg.MediaKinds()
	must(t, err)
	registry, err := mediastore.NewRegistry(config, mediastore.Hooks{})
	must(t, err)
	queue, err := workqueue.New(w.pool, registry, a.cfg.Media.WorkerSchema)
	must(t, err)
	journal, err := mediastore.NewPGJournal(w.pool, w.schema, queue)
	must(t, err)
	jobs, err := mediastore.NewJobs(mediastore.JobsConfig{Store: store, Registry: registry, Locker: mediastore.PGLocker(w.pool), Journal: journal, Now: w.clock.now})
	must(t, err)
	return registry, jobs
}

func TestMediaPurgeClosesSweepRevision(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	m := attachMedia(t, w, a)
	registry, jobs := newPurgeJobs(t, w, a, m.store)
	ref := galleryRef(t, a.engine.Client(), "Unadopted allocation")
	target, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
	must(t, err)
	item, err := registry.Item(target)
	must(t, err)
	sum := sha256.Sum256([]byte("never adopted"))
	_, err = jobs.Manifests().NewBlobs(t.Context(), target, layout.AreaInternal, [][]byte{sum[:]})
	must(t, err)
	w.clock.set(w.clock.now().Add(25 * time.Hour))
	cutoff := w.clock.now()
	cleanup := pgx.Identifier{w.schema, "content_media_cleanup"}.Sanitize()
	var due time.Time
	must(t, w.pool.QueryRow(t.Context(), `SELECT due_at FROM `+cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2`, target.TenantID, item.Prefix()).Scan(&due))
	if due.After(cutoff) {
		t.Fatal("allocation cleanup is not due")
	}
	processed, err := jobs.PurgeRetired(t.Context(), 1)
	must(t, err)
	if processed != 1 {
		t.Fatalf("purge attempted %d folders, expected 1", processed)
	}
	var retired bool
	must(t, w.pool.QueryRow(t.Context(), `SELECT retired_at IS NOT NULL FROM `+pgx.Identifier{w.schema, "content_media_allocations"}.Sanitize()+` WHERE tenant_id=$1 AND folder_prefix=$2`, target.TenantID, item.Prefix()).Scan(&retired))
	if !retired {
		t.Fatal("purge did not retire the expired allocation")
	}
	var overdue int
	must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2 AND due_at<=$3`, target.TenantID, item.Prefix(), cutoff).Scan(&overdue))
	if overdue != 0 {
		t.Fatal("purge left its swept folder overdue")
	}
}

type purgeDeleteHook struct {
	media.Store
	beforeDelete func(context.Context, string) error
}

func (s *purgeDeleteHook) Delete(ctx context.Context, key string) error {
	if err := s.beforeDelete(ctx, key); err != nil {
		return err
	}
	return s.Store.Delete(ctx, key)
}

func TestMediaPurgeBoundsTurns(t *testing.T) {
	for _, name := range []string{"settled", "future", "sweep", "missing", "interrupted", "cancelled", "contended"} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			a := w.apps[0]
			m := attachMedia(t, w, a)
			registry, jobs := newPurgeJobs(t, w, a, m.store)
			cleanup := pgx.Identifier{w.schema, "content_media_cleanup"}.Sanitize()
			commits := pgx.Identifier{w.schema, "content_media_commits"}.Sanitize()
			cutoff := w.clock.now()
			cleanupAt := cutoff.Add(-time.Hour)
			future := name == "future"
			if future {
				cleanupAt = cutoff.Add(time.Hour)
			}
			seed := func(title string, count int, due, deadline time.Time) (mediastore.Item, []uuid.UUID) {
				t.Helper()
				ref := galleryRef(t, a.engine.Client(), title)
				target, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
				must(t, err)
				item, err := registry.Item(target)
				must(t, err)
				sum := sha256.Sum256([]byte(title))
				key := item.Prefix() + layout.AreaInternal + "/" + layout.BlobName(sum[:], "")
				_, err = w.pool.Exec(t.Context(), `WITH receipts AS (
 INSERT INTO `+commits+`
 (tenant_id,operation_id,content_kind,content_id,content_version_id,folder_prefix,actor_id,fingerprint,lease_id,state,effects)
 SELECT $1,id,$2,$3,$4,$5,'',decode(repeat('00',32),'hex'),gen_random_uuid(),'applied',
 jsonb_build_object('cleanup_delete',true,'cleanup_at',$6::timestamptz,'private',jsonb_build_array($7::text || id::text))
 FROM (SELECT gen_random_uuid() AS id FROM generate_series(1,$8::int)) ids RETURNING operation_id
)
INSERT INTO `+cleanup+` (tenant_id,folder_prefix,content_kind,due_at,deletion_operations)
VALUES ($1,$5,$2,$9,ARRAY(SELECT operation_id FROM receipts ORDER BY operation_id))`,
					target.TenantID, target.ContentKind, target.ContentID, target.ContentVersionID, item.Prefix(), deadline, key, count, due)
				must(t, err)
				var operations []uuid.UUID
				must(t, w.pool.QueryRow(t.Context(), `SELECT deletion_operations FROM `+cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2`, target.TenantID, item.Prefix()).Scan(&operations))
				return item, operations
			}
			count := 1500
			if name == "missing" || name == "interrupted" || name == "cancelled" {
				count = 96
			}
			huge, original := seed("Huge cleanup row", count, cutoff.Add(-2*time.Hour), cleanupAt)
			if name == "contended" {
				unlock, err := mediastore.PGLocker(w.pool).Lock(t.Context(), huge.ManifestKey())
				must(t, err)
				defer unlock()
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				defer cancel()
				processed, err := jobs.PurgeRetired(ctx, 1)
				if processed != 1 || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("purge attempted %d folders with error %v", processed, err)
				}
				if err.Error() != context.DeadlineExceeded.Error() || errors.Is(err, pglock.ErrContended) {
					t.Fatalf("lock deadline became a row failure: %v", err)
				}
				var operations []uuid.UUID
				must(t, w.pool.QueryRow(t.Context(), `SELECT deletion_operations FROM `+cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2`, huge.Ref().TenantID, huge.Prefix()).Scan(&operations))
				if !slices.Equal(operations, original) {
					t.Fatal("contended purge changed the row's operations")
				}
				return
			}
			keyFor := func(id uuid.UUID) string {
				t.Helper()
				var key string
				must(t, w.pool.QueryRow(t.Context(), `SELECT effects->'private'->>0 FROM `+commits+` WHERE tenant_id=$1 AND operation_id=$2`, huge.Ref().TenantID, id).Scan(&key))
				return key
			}
			if count == 96 {
				if name != "cancelled" {
					_, err := w.pool.Exec(t.Context(), `DELETE FROM `+commits+` WHERE tenant_id=$1 AND operation_id=$2`, huge.Ref().TenantID, original[0])
					must(t, err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				deleteFailure := errors.New("independent deletion failure")
				if name != "missing" {
					cancelKey := keyFor(original[64])
					if name == "interrupted" {
						_, err := w.pool.Exec(t.Context(), `UPDATE `+commits+` SET folder_prefix=$3 WHERE tenant_id=$1 AND operation_id=$2`, huge.Ref().TenantID, original[34], huge.Prefix()+"wrong/")
						must(t, err)
					}
					_, jobs = newPurgeJobs(t, w, a, &purgeDeleteHook{Store: m.store, beforeDelete: func(ctx context.Context, key string) error {
						if key == cancelKey {
							cancel()
							err := fmt.Errorf("%w: s3 delete %s: %w", media.ErrUnavailable, key, ctx.Err())
							if name == "interrupted" {
								return errors.Join(deleteFailure, err)
							}
							return err
						}
						return nil
					}})
				}
				processed, err := jobs.PurgeRetired(ctx, 1)
				if processed != 1 || err == nil {
					t.Fatalf("purge attempted %d folders with error %v", processed, err)
				}
				if name != "cancelled" && !strings.Contains(err.Error(), original[0].String()) {
					t.Fatalf("purge lost the missing receipt failure: %v", err)
				}
				if name == "interrupted" && !strings.Contains(err.Error(), original[34].String()) {
					t.Fatalf("purge lost validation failure from the interrupted chunk: %v", err)
				}
				if name == "interrupted" && !errors.Is(err, deleteFailure) {
					t.Fatalf("purge lost independent deletion failure: %v", err)
				}
				if errors.Is(err, media.ErrUnavailable) {
					t.Fatalf("classified cancellation became a row failure: %v", err)
				}
				if name != "missing" && !errors.Is(err, context.Canceled) {
					t.Fatalf("purge lost cancellation: %v", err)
				}
				if name == "cancelled" && err.Error() != context.Canceled.Error() {
					t.Fatalf("pure cancellation became a row failure: %v", err)
				}
				var operations []uuid.UUID
				must(t, w.pool.QueryRow(t.Context(), `SELECT deletion_operations FROM `+cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2`, huge.Ref().TenantID, huge.Prefix()).Scan(&operations))
				if name == "missing" {
					if !slices.Equal(operations, original[:1]) {
						t.Fatalf("missing receipt prevented settling the others: %v", operations)
					}
				} else {
					for _, id := range original[1:32] {
						if slices.Contains(operations, id) {
							t.Fatalf("cancellation lost settled progress for %s", id)
						}
					}
				}
				return
			}
			var tail []string
			for _, i := range []int{1024, 1200, 1499} {
				key := keyFor(original[i])
				_, err := m.store.Put(t.Context(), key, strings.NewReader("tail"), 4, media.PutOptions{ContentType: "text/plain"})
				must(t, err)
				tail = append(tail, key)
			}
			assertTailGone := func() {
				t.Helper()
				for _, key := range tail {
					if _, err := m.store.Head(t.Context(), key); !errors.Is(err, mediastore.ErrNotFound) {
						t.Fatalf("tail object %s survived cleanup: %v", key, err)
					}
				}
			}
			if name == "sweep" {
				_, err := jobs.Sweep(t.Context(), huge.Ref())
				must(t, err)
				var operations []uuid.UUID
				must(t, w.pool.QueryRow(t.Context(), `SELECT deletion_operations FROM `+cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2`, huge.Ref().TenantID, huge.Prefix()).Scan(&operations))
				if len(operations) != 0 {
					t.Fatalf("explicit sweep left %d settled operations", len(operations))
				}
				assertTailGone()
				return
			}
			small, _ := seed("Later cleanup row", 1, cutoff.Add(-time.Hour), cutoff.Add(-time.Hour))
			var revision uuid.UUID
			must(t, w.pool.QueryRow(t.Context(), `SELECT revision FROM `+cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2`, huge.Ref().TenantID, huge.Prefix()).Scan(&revision))
			processed, err := jobs.PurgeRetired(t.Context(), 1)
			must(t, err)
			if processed != 1 {
				t.Fatalf("purge attempted %d folders, expected 1", processed)
			}
			var operations []uuid.UUID
			var due time.Time
			var after uuid.UUID
			must(t, w.pool.QueryRow(t.Context(), `SELECT deletion_operations,due_at,revision FROM `+cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2`, huge.Ref().TenantID, huge.Prefix()).Scan(&operations, &due, &after))
			want := slices.Clone(original[1024:])
			if future {
				want = append(want, original[:1024]...)
			}
			if !slices.Equal(operations, want) {
				t.Fatalf("turn left %d operations in the wrong order, expected %d", len(operations), len(want))
			}
			if !due.After(cutoff) || after != revision {
				t.Fatalf("checkpoint changed revision or left row overdue: due=%s revision=%s", due, after)
			}
			processed, err = jobs.PurgeRetired(t.Context(), 1)
			must(t, err)
			if processed != 1 {
				t.Fatalf("next purge attempted %d folders, expected 1", processed)
			}
			var remaining int
			must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2`, small.Ref().TenantID, small.Prefix()).Scan(&remaining))
			if remaining != 0 {
				t.Fatal("huge row starved the later due folder")
			}
			if future {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				_, err := jobs.Sweep(ctx, huge.Ref())
				must(t, err)
				must(t, w.pool.QueryRow(t.Context(), `SELECT deletion_operations FROM `+cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2`, huge.Ref().TenantID, huge.Prefix()).Scan(&operations))
				if len(operations) != len(original) {
					t.Fatal("explicit sweep pruned future receipts")
				}
			}
			next := cutoff
			if cleanupAt.After(next) {
				next = cleanupAt
			}
			w.clock.set(next.Add(time.Second))
			for range 2 {
				processed, err = jobs.PurgeRetired(t.Context(), 1)
				must(t, err)
				if processed != 1 {
					t.Fatalf("finishing purge attempted %d folders, expected 1", processed)
				}
				must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2`, huge.Ref().TenantID, huge.Prefix()).Scan(&remaining))
				if remaining == 0 {
					break
				}
				w.clock.set(w.clock.now().Add(time.Second))
			}
			if remaining != 0 {
				t.Fatal("huge cleanup row did not finish across turns")
			}
			assertTailGone()
		})
	}
}

func TestFailedMediaRestoreSchedulesAllocations(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new row"
		if existing {
			name = "existing row"
		}
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			a := w.apps[0]
			m := attachMedia(t, w, a)
			registry, jobs := newPurgeJobs(t, w, a, m.store)
			ref := galleryRef(t, a.engine.Client(), "Failed historical copy")
			target, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
			must(t, err)
			item, err := registry.Item(target)
			must(t, err)
			cleanup := pgx.Identifier{w.schema, "content_media_cleanup"}.Sanitize()
			var before uuid.UUID
			if existing {
				must(t, w.pool.QueryRow(t.Context(), `INSERT INTO `+cleanup+` (tenant_id,folder_prefix,content_kind,due_at)
VALUES ($1,$2,$3,$4) RETURNING revision`, target.TenantID, item.Prefix(), target.ContentKind, w.clock.now().Add(48*time.Hour)).Scan(&before))
			}
			sum := sha256.Sum256([]byte("missing snapshot bytes"))
			blob := layout.BlobRef(layout.AreaOriginals, layout.BlobName(sum[:], uuid.NewString()))
			key, err := item.Blob(blob)
			must(t, err)
			snapshot := mediastore.Snapshot{Ref: target, VersionID: uuid.NewString(),
				Manifest: &mediastore.Manifest{V: media.ManifestVersion, Incarnation: uuid.NewString(),
					Files: []media.File{{Path: "pages/missing.png", Type: "image/png", Blob: blob}}},
				Objects: []media.ObjectVersion{{Key: key, VersionID: uuid.NewString()}},
			}
			_, err = jobs.Restore(t.Context(), snapshot, mediastore.RestoreOptions{OperationID: uuid.NewString()})
			if err == nil {
				t.Fatal("restore copied a nonexistent object version")
			}
			var allocations int
			must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{w.schema, "content_media_allocations"}.Sanitize()+` WHERE tenant_id=$1 AND folder_prefix=$2 AND retired_at IS NULL`, target.TenantID, item.Prefix()).Scan(&allocations))
			if allocations != 1 {
				t.Fatalf("failed restore left %d allocations, expected 1 (restore error: %v)", allocations, err)
			}
			var due time.Time
			var after uuid.UUID
			var operations []uuid.UUID
			must(t, w.pool.QueryRow(t.Context(), `SELECT due_at,revision,deletion_operations FROM `+cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2`, target.TenantID, item.Prefix()).Scan(&due, &after, &operations))
			if delta := due.Sub(w.clock.now().Add(24 * time.Hour)); delta < -time.Second || delta > time.Second {
				t.Fatalf("restore allocation cleanup due at %s, expected now + grace", due)
			}
			if after == before || len(operations) != 0 {
				t.Fatalf("restore did not refresh allocation-only scheduling: revision=%s operations=%v", after, operations)
			}
		})
	}
}

func TestMediaPurgePreservesPostSnapshotAllocation(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	m := attachMedia(t, w, a)
	registry, jobs := newPurgeJobs(t, w, a, m.store)
	ref := galleryRef(t, a.engine.Client(), "Allocation at cleanup snapshot boundary")
	target, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
	must(t, err)
	item, err := registry.Item(target)
	must(t, err)
	sum := sha256.Sum256([]byte("allocation A"))
	blobs, err := jobs.Manifests().NewBlobs(t.Context(), target, layout.AreaInternal, [][]byte{sum[:]})
	must(t, err)
	keyA, err := item.Blob(blobs[0])
	must(t, err)
	w.clock.set(w.clock.now().Add(25 * time.Hour))
	processed, err := jobs.PurgeRetired(t.Context(), 1)
	must(t, err)
	if processed != 1 {
		t.Fatalf("purge attempted %d folders, expected 1", processed)
	}
	allocations := pgx.Identifier{w.schema, "content_media_allocations"}.Sanitize()
	cleanup := pgx.Identifier{w.schema, "content_media_cleanup"}.Sanitize()
	var retired bool
	must(t, w.pool.QueryRow(t.Context(), `SELECT retired_at IS NOT NULL FROM `+allocations+` WHERE tenant_id=$1 AND folder_prefix=$2 AND object_key=$3`, target.TenantID, item.Prefix(), keyA).Scan(&retired))
	if !retired {
		t.Fatal("purge did not retire allocation A")
	}
	sum = sha256.Sum256([]byte("allocation A2"))
	blobs, err = jobs.Manifests().NewBlobs(t.Context(), target, layout.AreaInternal, [][]byte{sum[:]})
	must(t, err)
	keyA2, err := item.Blob(blobs[0])
	must(t, err)
	w.clock.set(w.clock.now().Add(25 * time.Hour))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	holder, err := w.pool.Begin(ctx)
	must(t, err)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = holder.Rollback(cleanup)
		stopped := make(chan struct{})
		go func() { workers.Wait(); close(stopped) }()
		select {
		case <-stopped:
		case <-cleanup.Done():
			t.Error("purge/allocation goroutines did not stop")
		}
	}()
	holderPID := int(holder.Conn().PgConn().PID())
	var locked int
	must(t, holder.QueryRow(ctx, `SELECT 1 FROM `+allocations+` WHERE tenant_id=$1 AND object_key=$2 FOR UPDATE`, target.TenantID, keyA2).Scan(&locked))
	done := make(chan error, 2)
	workers.Add(1)
	go func() {
		defer workers.Done()
		processed, err := jobs.PurgeRetired(ctx, 1)
		if err == nil && processed != 1 {
			err = fmt.Errorf("purge attempted %d folders, expected 1", processed)
		}
		done <- err
	}()
	waitFor(t, func() bool {
		var blocked bool
		must(t, w.pool.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM pg_stat_activity a JOIN pg_locks l ON l.pid=a.pid
WHERE a.wait_event_type='Lock' AND NOT l.granted AND $1=ANY(pg_blocking_pids(a.pid))
AND a.query LIKE $2)`, holderPID, "UPDATE "+allocations+" SET retired_at%").Scan(&blocked))
		return blocked
	})
	// B attempts while the sweep holds the folder lock between its scan and snapshot.
	sum = sha256.Sum256([]byte("allocation B"))
	var blobsB []string
	workers.Add(1)
	go func() {
		defer workers.Done()
		var err error
		blobsB, err = jobs.Manifests().NewBlobs(ctx, target, layout.AreaInternal, [][]byte{sum[:]})
		done <- err
	}()
	waitFor(t, func() bool {
		var blocked bool
		must(t, w.pool.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted
AND ((classid::bigint << 32) | objid::bigint)=hashtextextended($1,0))`,
			"contentkit:media:"+item.ManifestKey()).Scan(&blocked))
		return blocked
	})
	must(t, holder.Rollback(ctx))
	for range 2 {
		select {
		case err := <-done:
			must(t, err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	keyB, err := item.Blob(blobsB[0])
	must(t, err)
	var scheduled bool
	must(t, w.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2)`, target.TenantID, item.Prefix()).Scan(&scheduled))
	if !scheduled {
		t.Fatal("purge dropped cleanup scheduled by allocation B")
	}
	must(t, w.pool.QueryRow(ctx, `SELECT retired_at IS NOT NULL FROM `+allocations+` WHERE tenant_id=$1 AND object_key=$2`, target.TenantID, keyA2).Scan(&retired))
	if !retired {
		t.Fatal("purge did not retire allocation A2")
	}
	must(t, w.pool.QueryRow(ctx, `SELECT retired_at IS NOT NULL FROM `+allocations+` WHERE tenant_id=$1 AND folder_prefix=$2 AND object_key=$3`, target.TenantID, item.Prefix(), keyB).Scan(&retired))
	if retired {
		t.Fatal("purge retired allocation B outside its snapshot")
	}
}

func TestMediaPurgeRetiresPublicAllocation(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	m := attachMedia(t, w, a)
	registry, jobs := newPurgeJobs(t, w, a, m.store)
	ref := galleryRef(t, a.engine.Client(), "Unadopted public allocation")
	target, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
	must(t, err)
	item, err := registry.Item(target)
	must(t, err)
	sum := sha256.Sum256([]byte("unadopted public blob"))
	blobs, err := jobs.Manifests().NewBlobs(t.Context(), target, layout.AreaPublic, [][]byte{sum[:]})
	must(t, err)
	key, err := item.Blob(blobs[0])
	must(t, err)
	w.clock.set(w.clock.now().Add(25 * time.Hour))
	processed, err := jobs.PurgeRetired(t.Context(), 1)
	must(t, err)
	if processed != 1 {
		t.Fatalf("purge attempted %d folders, expected 1", processed)
	}
	var retired bool
	must(t, w.pool.QueryRow(t.Context(), `SELECT retired_at IS NOT NULL FROM `+pgx.Identifier{w.schema, "content_media_allocations"}.Sanitize()+` WHERE tenant_id=$1 AND folder_prefix=$2 AND object_key=$3`, target.TenantID, item.Prefix(), key).Scan(&retired))
	if !retired {
		t.Error("purge did not retire the public allocation")
	}
	_, err = jobs.Sweep(t.Context(), target)
	must(t, err)
	var retirements int
	must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{w.schema, "content_media_commits"}.Sanitize()+` WHERE tenant_id=$1 AND folder_prefix=$2 AND (effects->'private' @> jsonb_build_array($3::text) OR effects->'public' @> jsonb_build_array($3::text))`, target.TenantID, item.Prefix(), key).Scan(&retirements))
	if retirements != 1 {
		t.Fatalf("public allocation appears in %d cleanup commits, expected 1", retirements)
	}
}

func TestMediaPurgeKeepsPublicUploadAllocation(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	m := attachMedia(t, w, a, func(c *ck.Config) {
		c.Media.Config.Kinds = append(c.Media.Config.Kinds, media.Kind{Name: "original", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "source", Types: []string{"text/plain"}, MaxBytes: 1024}},
		})
	})
	registry, jobs := newPurgeJobs(t, w, a, m.store)
	id, err := uuid.NewV7()
	must(t, err)
	target, err := registry.Ref("original", id.String())
	must(t, err)
	item, err := registry.Item(target)
	must(t, err)
	body := []byte("kept original")
	sum := sha256.Sum256(body)
	blobs, err := jobs.Manifests().NewBlobs(t.Context(), target, layout.AreaPublic, [][]byte{sum[:]})
	must(t, err)
	key, err := item.Blob(blobs[0])
	must(t, err)
	_, err = m.store.Put(t.Context(), key, bytes.NewReader(body), int64(len(body)), media.PutOptions{ContentType: "text/plain"})
	must(t, err)
	_, err = jobs.Manifests().Edit(t.Context(), target, func(manifest *mediastore.Manifest) error {
		manifest.Files = []media.File{{Path: "source.txt", Type: "text/plain", Size: int64(len(body)), Blob: blobs[0]}}
		return nil
	})
	must(t, err)
	_, err = jobs.Manifests().SyncPublic(t.Context(), target)
	must(t, err)
	var retired bool
	must(t, w.pool.QueryRow(t.Context(), `SELECT retired_at IS NOT NULL FROM `+pgx.Identifier{w.schema, "content_media_allocations"}.Sanitize()+` WHERE tenant_id=$1 AND object_key=$2`, target.TenantID, key).Scan(&retired))
	if retired {
		t.Error("public sync retired a kept upload allocation")
	}
	manifest, err := jobs.Manifests().EditExisting(t.Context(), target, func(manifest *mediastore.Manifest) error {
		manifest.Meta = map[string]any{"title": "Still kept"}
		return nil
	})
	must(t, err)
	if len(manifest.Files) != 1 || manifest.Files[0].Blob != blobs[0] {
		t.Fatal("later edit did not keep the public upload")
	}
}

func TestMediaCleanupDrainsDueFolders(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	m := attachMedia(t, w, a)
	a.server.Close()
	must(t, a.engine.Close(t.Context()))

	endpoint, err := url.Parse(os.Getenv("CONTENTKIT_CRITICAL_S3_ENDPOINT"))
	must(t, err)
	proxy := httputil.NewSingleHostReverseProxy(endpoint)
	var requests atomic.Int64
	var pause atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	wire := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if pause.CompareAndSwap(true, false) {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	defer func() {
		releaseOnce.Do(func() { close(release) })
		wire.Close()
	}()
	a.deps.Store, err = medias3.New(medias3.Config{Bucket: m.bucket, Endpoint: wire.URL, PublicEndpoint: endpoint.String(), AccessKeyID: os.Getenv("CONTENTKIT_CRITICAL_S3_USER"), SecretAccessKey: os.Getenv("CONTENTKIT_CRITICAL_S3_PASSWORD"), UsePathStyle: true})
	must(t, err)
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	queue := catalogstore.QueueFor(a.cfg.Scope.AppID.String())
	fleet, err := jobhook.Fleet(t.Context(), a.engine, &river.Config{Schema: w.schema + "_river", Queues: map[string]river.QueueConfig{queue: {MaxWorkers: 1}}})
	must(t, err)
	scope := layout.Scope{TenantID: a.cfg.Scope.TenantID.String(), AppID: a.cfg.Scope.AppID.String()}
	qs := pgx.Identifier{w.schema}.Sanitize()
	// Completed journal owners outlive their catalog rows. Empty folders still
	// require their durable cleanup obligation to be completed against storage.
	_, err = w.pool.Exec(t.Context(), `WITH owners AS (
 SELECT uuidv7()::text AS version FROM generate_series(1,401)
), recorded AS (
 INSERT INTO `+qs+`.content_media_commits
 (tenant_id,operation_id,content_kind,content_id,content_version_id,folder_prefix,actor_id,fingerprint,lease_id,state)
 SELECT $1,gen_random_uuid(),'gallery',$2,version,$3 || 'gallery/' || version || '/',
 '',decode(repeat('00',32),'hex'),gen_random_uuid(),'absent' FROM owners
 RETURNING tenant_id,folder_prefix,content_kind
)
INSERT INTO `+qs+`.content_media_cleanup (tenant_id,folder_prefix,content_kind,due_at)
SELECT tenant_id,folder_prefix,content_kind,$4 FROM recorded`, scope.Partition(), uuid.Must(uuid.NewV7()).String(), scope.Root(), w.clock.now().Add(-time.Hour))
	must(t, err)
	producer, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
	must(t, err)
	jobs := pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize()
	kind := "contentkit_content_purge_deleted"
	// A 16-minute-old checkpoint must catch up on startup; the former four-hour
	// cadence would suppress the run and leave all the due folders untouched.
	_, err = w.pool.Exec(t.Context(), `DELETE FROM `+jobs+` WHERE kind=$1 AND queue=$2`, kind, queue)
	must(t, err)
	metadata, err := json.Marshal(map[string]string{"river:periodic_job_id": jobqueue.For(kind, a.cfg.Scope.AppID.String())})
	must(t, err)
	previous, err := producer.Insert(t.Context(), scheduledContentJob{kind: kind}, &river.InsertOpts{Queue: queue, Metadata: metadata})
	must(t, err)
	_, err = w.pool.Exec(t.Context(), `UPDATE `+jobs+` SET state='completed',created_at=$2,scheduled_at=$2,attempted_at=$2,finalized_at=$2 WHERE id=$1`, previous.Job.ID, time.Now().Add(-16*time.Minute))
	must(t, err)
	pause.Store(true)
	must(t, fleet.Start(t.Context()))
	t.Cleanup(func() { must(t, fleet.Stop(context.Background())) })
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("15-minute purge did not catch up from its 16-minute-old checkpoint")
	}
	var running int64
	must(t, w.pool.QueryRow(t.Context(), `SELECT id FROM `+jobs+` WHERE kind=$1 AND queue=$2 AND state='running'`, kind, queue).Scan(&running))
	unique := mediastore.PendingOnce
	unique.ByQueue = true
	duplicate, err := producer.Insert(t.Context(), scheduledContentJob{kind: kind}, &river.InsertOpts{Queue: queue, UniqueOpts: unique})
	must(t, err)
	if !duplicate.UniqueSkippedAsDuplicate || duplicate.Job.ID != running {
		t.Fatal("periodic purge did not deduplicate against its running job")
	}
	releaseOnce.Do(func() { close(release) })
	finish := func(id int64) {
		t.Helper()
		waitForWithin(t, 90*time.Second, func() bool {
			current, err := producer.JobGet(t.Context(), id)
			must(t, err)
			if current.State == rivertype.JobStateDiscarded {
				t.Fatalf("cleanup failed: %+v", current.Errors)
			}
			return current.State == rivertype.JobStateCompleted
		})
	}
	finish(running)
	var remaining int
	must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+qs+`.content_media_cleanup WHERE tenant_id=$1 AND due_at<=$2`, scope.Partition(), w.clock.now()).Scan(&remaining))
	if remaining != 0 {
		t.Fatalf("one purge left %d of 401 due folders", remaining)
	}
	before, started := requests.Load(), time.Now()
	idle, err := producer.Insert(t.Context(), scheduledContentJob{kind: kind}, &river.InsertOpts{Queue: queue, UniqueOpts: unique})
	must(t, err)
	finish(idle.Job.ID)
	if n := requests.Load() - before; n != 0 {
		t.Fatalf("idle purge made %d object-storage requests", n)
	}
	t.Logf("idle purge: enqueue-to-observation=%s, object-storage requests=0", time.Since(started))
}
