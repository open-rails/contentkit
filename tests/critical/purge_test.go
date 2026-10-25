package critical_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
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
	contentKey, err := access.ContentKey(a.cfg.Scope.AppID.String(), ref.ID)
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
	must(t, a.engine.Start(t.Context()))
	admin = a.engine.Client()
	producer, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: w.schema + "_river"})
	must(t, err)
	purge := func(t *testing.T, want rivertype.JobState) {
		t.Helper()
		job, err := producer.Insert(t.Context(), scheduledContentJob{kind: "contentkit_content_purge_deleted"}, &river.InsertOpts{Queue: catalog.Queue, MaxAttempts: 1})
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
			if !strings.HasSuffix(aws.ToString(object.Key), "/"+layout.ErasureFenceName) && !strings.HasSuffix(aws.ToString(object.Key), "/"+layout.ManifestName) {
				count++
			}
		}
		return count
	}
	if objectCount() == 0 {
		t.Fatal("purge positive control has no durable media")
	}
	must(t, admin.DeleteContentVersion(t.Context(), ref.ID, ref.Version))
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
			var tombstone media.Manifest
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
			must(t, admin.DeleteContentVersion(t.Context(), ref.ID, ref.Version))
			rows, err := admin.GetManage(t.Context(), ck.GetManageQuery{State: "deleted", Ids: []string{ref.ID}})
			must(t, err)
			if len(rows) != 1 || rows[0].DeletedAt == nil {
				t.Fatal("race fixture lacks deletion timestamp")
			}
			w.clock.set(rows[0].DeletedAt.Add(30*24*time.Hour + time.Microsecond))
			return ref
		}
		restored := makeDeleted("Restore wins")
		// Finish this upload's metadata before constructing the controlled race.
		// Processing readiness does not mean every follow-up snapshot has run.
		waitFor(t, func() bool {
			var pending int
			must(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize()+" WHERE kind='contentkit_version_publish' AND args->>'version'=$1 AND state NOT IN ('completed','cancelled')", restored.Version).Scan(&pending))
			return pending == 0
		})
		qs := pgx.Identifier{w.schema}.Sanitize()
		const barrier int64 = 612900086
		// The test controls the pause outside the application pool. Otherwise
		// the paused restore and two waiting edits exhaust a four-connection
		// pool together with this holder, preventing the test from releasing it.
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
		var restorePID int
		waitFor(t, func() bool {
			var pids []int
			must(t, holder.QueryRow(ctx, "SELECT coalesce(array_agg(pid), '{}') FROM pg_locks WHERE locktype='advisory' AND objid=$1 AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND NOT granted", barrier).Scan(&pids))
			if len(pids) != 1 {
				return false
			}
			restorePID = pids[0]
			return true
		})
		// Two ordinary edits reproduce the same occupied connections as late
		// metadata workers queued behind the paused restore. The purger must
		// still skip that locked item rather than erase its media.
		label := "Concurrent metadata edit"
		writes := make(chan error, 2)
		for range 2 {
			go func() {
				_, err := admin.PatchContentVersion(ctx, restored.ID, restored.Version, catalog.VersionInput{Label: &label})
				writes <- err
			}()
		}
		waitFor(t, func() bool {
			var blocked int
			must(t, holder.QueryRow(ctx, `WITH RECURSIVE blocked(pid) AS (
 SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid))
 UNION SELECT a.pid FROM pg_stat_activity a JOIN blocked b ON b.pid=ANY(pg_blocking_pids(a.pid)) WHERE a.datname=current_database()
) SELECT count(*) FROM blocked`, restorePID).Scan(&blocked))
			return blocked >= 2
		})
		waitFor(t, func() bool {
			var n int
			must(t, w.pool.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND objid=$1 AND NOT granted", barrier).Scan(&n))
			return n == 1
		})
		purge(t, rivertype.JobStateCompleted)
		_, err = holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", barrier)
		must(t, err)
		must(t, <-result)
		for range 2 {
			must(t, <-writes)
		}
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
		fencePath := "/" + m.bucket + "/" + root + claimed.Kind + "/" + claimed.Version + "/public/" + layout.ErasureFenceName
		pauseErasure.Store(&fencePath)
		job, err := producer.Insert(t.Context(), scheduledContentJob{kind: "contentkit_content_purge_deleted"}, &river.InsertOpts{Queue: catalog.Queue, MaxAttempts: 1})
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
		_, err = admin.GetContent(t.Context(), restored.ID, ck.GetContentQuery{})
		must(t, err)
	})
}
