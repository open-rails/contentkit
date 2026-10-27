package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/notify"
	notifypg "github.com/open-rails/authkit/notify/postgres"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	medias3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/signal"
)

func eraseAuthKitAccount(t *testing.T, w *world, a *application, id string) {
	t.Helper()
	schema := "contentkit_accounts"
	if a.cfg.Accounts != nil {
		schema = a.cfg.Accounts.Schema
	}
	store, err := notifypg.New(w.pool, notifypg.Config{Schema: schema, Scope: "contentkit:" + (layout.Scope{TenantID: a.cfg.Scope.TenantID.String()}).Partition()})
	must(t, err)
	must(t, store.EraseRecipient(t.Context(), notify.Recipient{Audience: notify.AudienceUser, ID: id}))
}

func checkAnonymizationProbe(t *testing.T, engine *ck.Engine, pending bool) {
	t.Helper()
	for _, probe := range engine.Probes() {
		if probe.Name == "contentkit_account_anonymization" {
			if err := probe.Check(t.Context()); (err != nil) != pending {
				t.Fatalf("account anonymization probe: pending=%v err=%v", pending, err)
			}
			return
		}
	}
	t.Fatal("account anonymization probe missing")
}

func TestAccountAnonymizationRecoveryAcrossStores(t *testing.T) {
	lifetime := t
	w := newWorld(t, func(c *ck.Config) {
		c.Accounts = &ck.AccountsConfig{Schema: "contentkit_accounts"}
	})
	a, sibling := w.apps[0], w.apps[1]
	configureDiscoverySignals(t, w, a)
	direct := a.deps.ClickHouse
	wire := newTCPCut(t, a.cfg.ClickHouse.Addr)
	borrowed, err := clickhouse.Open(&clickhouse.Options{Addr: []string{wire.listener.Addr().String()}, Auth: clickhouse.Auth{Database: a.cfg.ClickHouse.Database, Username: a.cfg.ClickHouse.Username, Password: a.cfg.ClickHouse.Password}, DialTimeout: time.Second, ReadTimeout: 15 * time.Second})
	must(t, err)
	t.Cleanup(func() { must(t, borrowed.Close()) })
	a.deps.ClickHouse = borrowed
	m := attachMedia(t, w, a, func(c *ck.Config) { c.Avatars = &ck.AvatarsConfig{} })
	account, ok, err := a.cfg.AccountMedia()
	must(t, err)
	if !ok || account.Schema != "contentkit_accounts" || account.WorkerSchema == a.cfg.Media.WorkerSchema {
		t.Fatalf("avatar primary lacks separate shared account storage: %+v", account)
	}
	user, credential := w.person(t, 0)
	other, otherCredential := w.person(t, 0)
	item, siblingItem := publishedArticle(t, a, "Anonymized contributions"), publishedArticle(t, sibling, "Independent app consumer")
	comment := postComment(t, a.client(credential), item.ID, "Discussion survives every retry")
	siblingComment := postComment(t, sibling.client(credential), siblingItem.ID, "Sibling app discussion")
	for _, token := range []string{credential, otherCredential} {
		_, err := a.client(token).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
		must(t, err)
	}
	_, err = sibling.client(credential).PostContentFavorite(t.Context(), siblingItem.ID, ck.PostContentFavoriteQuery{})
	must(t, err)
	for _, app := range []*application{a, sibling} {
		_, err := app.publishInbox(t.Context(), inboxPublication{Notifications: []notify.Input{{Recipient: notify.Recipient{Audience: "user", ID: user.ID}, Origin: notify.Origin{System: "critical", Scope: app.cfg.Scope.AppID.String(), ID: uuid.NewString()}, Kind: "account.private", CreatedAt: time.Now(), Data: json.RawMessage(`{"message":"Private account inbox"}`)}}})
		must(t, err)
	}
	eventID := uuid.NewString()
	must(t, a.client(credential).PostSignals(t.Context(), discovery.Signals{Signals: []discovery.SignalInput{{ContentID: item.ID, Type: "click", EventID: eventID, OccurredAt: time.Now()}}}))
	signalStore, err := signal.NewStore(direct, a.cfg.ClickHouse.Database)
	must(t, err)
	signalRef := contentref.New(a.cfg.Scope.AppID.String(), "article", item.ID)
	beforeHistory, err := signalStore.States(t.Context(), a.cfg.Scope.AppID.String(), signal.Subject{UserID: user.ID}, []signal.ContentRef{signalRef})
	must(t, err)
	if _, present := beforeHistory[signalRef.Key()]; !present {
		t.Fatalf("account history positive control missing: %+v", beforeHistory)
	}
	uploadAvatar := func(id, token string) string {
		ref := media.RefBody{Kind: "user", ID: id}
		_, err := a.client(token).IngestMedia(t.Context(), nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "avatar.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}})
		must(t, err)
		var address string
		waitForWithin(t, 45*time.Second, func() bool {
			read, err := a.client(token).GetMedia(t.Context(), ref.Kind, ref.ID, ck.GetMediaQuery{Editor: true})
			must(t, err)
			if read.State != media.StateReady || len(read.Public) == 0 || len(read.Public[0].Renditions) == 0 {
				return false
			}
			address = read.Public[0].Renditions[0].URL
			response, err := http.Get(address)
			must(t, err)
			response.Body.Close()
			return response.StatusCode == 200
		})
		return address
	}
	avatarURL := uploadAvatar(user.ID, credential)
	appReset, appResetCredential := w.person(t, 0)
	resetAvatar := uploadAvatar(appReset.ID, appResetCredential)
	// Both apps use one real signal database; app predicates, not separate
	// tables or providers, must keep the untouched sibling's data intact.
	sibling.server.Close()
	must(t, sibling.engine.Close(t.Context()))
	sibling.cfg.ClickHouse = a.cfg.ClickHouse
	sibling.cfg.Database.SkipMigrations = true
	sibling.deps.ClickHouse = direct
	sibling.engine, err = ck.New(t.Context(), sibling.cfg, sibling.deps)
	must(t, err)
	sibling.server = mountEngine(t, sibling.engine)
	for _, target := range []struct {
		app *application
		id  string
	}{{a, item.ID}, {sibling, siblingItem.ID}} {
		_, err := target.app.client(appResetCredential).PostContentFavorite(t.Context(), target.id, ck.PostContentFavoriteQuery{})
		must(t, err)
		must(t, target.app.client(appResetCredential).PostSignals(t.Context(), discovery.Signals{Signals: []discovery.SignalInput{{ContentID: target.id, Type: "click", EventID: uuid.NewString(), OccurredAt: time.Now()}}}))
		_, err = target.app.publishInbox(t.Context(), inboxPublication{Notifications: []notify.Input{{Recipient: notify.Recipient{Audience: "user", ID: appReset.ID}, Origin: notify.Origin{System: "critical", Scope: target.app.cfg.Scope.AppID.String(), ID: uuid.NewString()}, Kind: "app.private", CreatedAt: time.Now(), Data: json.RawMessage(`{"message":"App receipt"}`)}}})
		must(t, err)
	}
	shared, err := notifypg.New(w.pool, notifypg.Config{Schema: "contentkit_accounts", Scope: "contentkit:" + (layout.Scope{TenantID: a.cfg.Scope.TenantID.String()}).Partition()})
	must(t, err)
	sharedNotice, err := shared.Publish(t.Context(), notify.Input{Recipient: notify.Recipient{Audience: "user", ID: appReset.ID}, Origin: notify.Origin{System: "critical-shared", Scope: a.cfg.Scope.TenantID.String(), ID: uuid.NewString()}, Kind: "tenant.shared", Visibility: notify.VisibilityTenant, CreatedAt: time.Now(), Data: json.RawMessage(`{"message":"Shared account receipt"}`)})
	must(t, err)
	endpoint, err := url.Parse(os.Getenv("CONTENTKIT_CRITICAL_S3_ENDPOINT"))
	must(t, err)
	proxy := httputil.NewSingleHostReverseProxy(endpoint)
	var failDelete atomic.Bool
	faultyStorage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failDelete.Load() && (r.Method == http.MethodDelete || r.Method == http.MethodPost && r.URL.Query().Has("delete")) {
			http.Error(w, "owned account deletion outage", 503)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer faultyStorage.Close()
	store, err := medias3.New(medias3.Config{Bucket: m.bucket, Endpoint: faultyStorage.URL, PublicEndpoint: endpoint.String(), AccessKeyID: os.Getenv("CONTENTKIT_CRITICAL_S3_USER"), SecretAccessKey: os.Getenv("CONTENTKIT_CRITICAL_S3_PASSWORD"), UsePathStyle: true})
	must(t, err)
	must(t, store.Check(t.Context()))
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.deps.Store = store
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	accountObjects := func() int {
		prefix := (layout.Scope{TenantID: a.cfg.Scope.TenantID.String()}).Prefix("user", user.ID, "")
		objects, err := m.management.ListObjectsV2(t.Context(), &awss3.ListObjectsV2Input{Bucket: &m.bucket, Prefix: aws.String(prefix)})
		must(t, err)
		count := 0
		for _, object := range objects.Contents {
			name := aws.ToString(object.Key)
			if !strings.HasSuffix(name, "/"+layout.ManifestName) && !strings.HasSuffix(name, "/"+layout.ErasureFenceName) {
				count++
			}
		}
		return count
	}
	if accountObjects() == 0 {
		t.Fatal("avatar positive control has no actual account media")
	}
	t.Run("CK-J23-S06_app_deletion_replay_preserves_shared_inbox_and_sibling_data", func(t *testing.T) {
		for _, app := range []*application{a, sibling} {
			inbox, err := app.accountInbox(appResetCredential).List(t.Context(), inboxQuery{})
			must(t, err)
			if len(inbox.Items) != 2 {
				t.Fatalf("shared/app inbox positive controls absent: %+v", inbox)
			}
		}
		must(t, a.engine.AnonymizeAccount(t.Context(), appReset.ID))
		recovery, err := ck.NewForErasureReplay(t.Context(), a.cfg, a.deps)
		must(t, err)
		defer func() { must(t, recovery.Close(context.Background())) }()
		report, err := recovery.ReplayErasureFences(t.Context())
		must(t, err)
		if !report.Complete() {
			t.Fatal("app replay did not finish")
		}
		inbox, err := a.accountInbox(appResetCredential).List(t.Context(), inboxQuery{})
		must(t, err)
		if len(inbox.Items) != 1 || inbox.Items[0].ID != sharedNotice.ID {
			t.Fatal("app replay erased or duplicated shared account receipt")
		}
		inbox, err = sibling.accountInbox(appResetCredential).List(t.Context(), inboxQuery{})
		must(t, err)
		if len(inbox.Items) != 2 {
			t.Fatal("app replay changed sibling inbox")
		}
		favorite, err := sibling.client(appResetCredential).GetContentFavorite(t.Context(), siblingItem.ID, ck.GetContentFavoriteQuery{})
		must(t, err)
		if !favorite.Favorited {
			t.Fatal("app replay erased sibling activity")
		}
		retained, err := a.client("").GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
		must(t, err)
		if retained.Count == nil || *retained.Count != 3 {
			t.Fatalf("app replay lost an anonymous contribution: %+v", retained)
		}
		localState, err := signalStore.States(t.Context(), a.cfg.Scope.AppID.String(), signal.Subject{UserID: appReset.ID}, []signal.ContentRef{signalRef})
		must(t, err)
		if len(localState) != 0 {
			t.Fatal("app replay left target signal attribution")
		}
		siblingRef := contentref.New(sibling.cfg.Scope.AppID.String(), "article", siblingItem.ID)
		otherState, err := signalStore.States(t.Context(), sibling.cfg.Scope.AppID.String(), signal.Subject{UserID: appReset.ID}, []signal.ContentRef{siblingRef})
		must(t, err)
		if _, exists := otherState[siblingRef.Key()]; !exists {
			t.Fatal("app replay erased same subject in sibling signal partition")
		}
		response, err := http.Get(resetAvatar)
		must(t, err)
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Fatal("deleted account avatar remained visible")
		}
	})
	t.Run("CK-J24-S01_CK-J24-S02_soft_delete_and_restore_preserve_signal_and_avatar", func(t *testing.T) {
		must(t, a.engine.SetAccountDeleted(t.Context(), user.ID, true))
		masked := readComment(t, a, item.ID, comment.ID)
		if masked.UserID != "" || masked.AnonName != "<Deleted>" || masked.Body != comment.Body {
			t.Fatal("soft deletion did not mask retained discussion")
		}
		states, err := signalStore.States(t.Context(), a.cfg.Scope.AppID.String(), signal.Subject{UserID: user.ID}, []signal.ContentRef{signalRef})
		must(t, err)
		if _, present := states[signalRef.Key()]; !present {
			t.Fatal("soft deletion removed recoverable interaction data")
		}
		response, err := http.Get(avatarURL)
		must(t, err)
		response.Body.Close()
		if response.StatusCode != 200 || accountObjects() == 0 {
			t.Fatal("soft deletion erased recoverable avatar media")
		}
		must(t, a.engine.SetAccountDeleted(t.Context(), user.ID, false))
		restored := readComment(t, a, item.ID, comment.ID)
		if restored.UserID != user.ID || restored.Author == nil || restored.Body != comment.Body {
			t.Fatal("restoration did not recover original attribution")
		}
	})
	t.Run("authkit_purge_allows_restart_before_consumer", func(t *testing.T) {
		eraseAuthKitAccount(t, w, a, user.ID)
		wire.cut()
		for _, app := range []*application{a, sibling} {
			app.server.Close()
			must(t, app.engine.Close(t.Context()))
			app.cfg.Database.SkipMigrations = true
			app.deps.ClickHouse = borrowed
			app.engine, err = ck.New(t.Context(), app.cfg, app.deps)
			must(t, err)
			app.server = mountEngine(lifetime, app.engine)
			checkAnonymizationProbe(t, app.engine, false)
		}
		_, err := w.pool.Exec(t.Context(), `UPDATE contentkit_accounts.notify_recipients SET erased_at=now()-interval '16 minutes' WHERE scope=$1 AND audience='user' AND recipient=$2`, "contentkit:"+(layout.Scope{TenantID: a.cfg.Scope.TenantID.String()}).Partition(), user.ID)
		must(t, err)
		for _, app := range []*application{a, sibling} {
			checkAnonymizationProbe(t, app.engine, true)
		}
	})
	t.Run("CK-J24-S05_database_commit_failure_keeps_attribution_and_retry_pending", func(t *testing.T) {
		qs := pgx.Identifier{w.schema}.Sanitize()
		_, err := w.pool.Exec(t.Context(), fmt.Sprintf(`CREATE FUNCTION %s.critical_reject_anonymization() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.actor_id=TG_ARGV[0] THEN RAISE EXCEPTION 'owned account commit refusal'; END IF; RETURN NEW; END$$;
CREATE CONSTRAINT TRIGGER critical_reject_anonymization AFTER INSERT OR UPDATE ON %s.content_account_lifecycle DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %s.critical_reject_anonymization('%s')`, qs, qs, qs, user.ID))
		must(t, err)
		if err := a.engine.AnonymizeAccount(t.Context(), user.ID); err == nil {
			t.Fatal("failed database commit reported successful anonymization")
		}
		_, err = w.pool.Exec(t.Context(), "DROP TRIGGER critical_reject_anonymization ON "+qs+".content_account_lifecycle; DROP FUNCTION "+qs+".critical_reject_anonymization()")
		must(t, err)
		unchanged := readComment(t, a, item.ID, comment.ID)
		if unchanged.UserID != user.ID || unchanged.Body != comment.Body {
			t.Fatal("failed transaction partially changed attribution")
		}
	})
	t.Run("CK-J24-S05_signal_outage_keeps_terminal_cleanup_retryable", func(t *testing.T) {
		wire.cut()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := a.engine.AnonymizeAccount(ctx, user.ID); err == nil {
			t.Fatal("disconnected ClickHouse reported complete cleanup")
		}
		masked := readComment(t, a, item.ID, comment.ID)
		if masked.UserID != "" || masked.Author != nil || masked.AnonName != "<Deleted>" || masked.Body != comment.Body {
			t.Fatalf("PG retry lost discussion or attribution fence: %+v", masked)
		}
		counts, err := a.client("").GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
		must(t, err)
		if counts.Count == nil || *counts.Count != 3 {
			t.Fatal("anonymization lost a retained contribution")
		}
		inbox, err := a.accountInbox(credential).List(t.Context(), inboxQuery{})
		must(t, err)
		if len(inbox.Items) != 0 {
			t.Fatal("app inbox remained after terminal cleanup began")
		}
		inbox, err = sibling.accountInbox(credential).List(t.Context(), inboxQuery{})
		must(t, err)
		if len(inbox.Items) != 0 {
			t.Fatal("AuthKit purge retained the sibling's private inbox")
		}
		for _, replay := range []func(context.Context) (signal.ErasureReport, error){a.engine.ReplayErasureFences, a.engine.ReplayAccountErasures} {
			report, err := replay(t.Context())
			if err == nil || report.Complete() || report.Remaining["account_plane"] == 0 {
				t.Fatalf("failed anonymization replay lost its pending obligation: report=%+v err=%v", report, err)
			}
		}
	})
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.deps.ClickHouse = direct
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	t.Run("CK-J23-S01_CK-J24-S05_storage_fence_hides_avatar_before_physical_cleanup", func(t *testing.T) {
		failDelete.Store(true)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		if err := a.engine.AnonymizeAccount(ctx, user.ID); err == nil {
			t.Fatal("failed S3 deletion reported complete anonymization")
		}
		if accountObjects() == 0 {
			t.Fatal("physical cleanup was not actually pending")
		}
		response, err := http.Get(avatarURL)
		must(t, err)
		response.Body.Close()
		if response.StatusCode != 404 {
			t.Fatalf("avatar remained accessible during physical cleanup: %d", response.StatusCode)
		}
		states, err := signal.NewStore(direct, a.cfg.ClickHouse.Database)
		must(t, err)
		history, err := states.States(t.Context(), a.cfg.Scope.AppID.String(), signal.Subject{UserID: user.ID}, []signal.ContentRef{signalRef})
		must(t, err)
		if len(history) != 0 {
			t.Fatal("old account still reads history after anonymization fence")
		}
		sibling.server.Close()
		must(t, sibling.engine.Close(t.Context()))
		sibling.deps.ClickHouse = direct
		sibling.engine, err = ck.New(t.Context(), sibling.cfg, sibling.deps)
		must(t, err)
		must(t, sibling.engine.AnonymizeAccount(t.Context(), user.ID))
		must(t, sibling.engine.Close(t.Context()))
		sibling.engine, err = ck.New(t.Context(), sibling.cfg, sibling.deps)
		must(t, err)
		sibling.server = mountEngine(lifetime, sibling.engine)
		checkAnonymizationProbe(t, sibling.engine, false)
		checkAnonymizationProbe(t, a.engine, true)
	})
	must(t, a.engine.Close(t.Context()))
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	t.Run("CK-J24-S05_reopened_consumer_finishes_all_stores_once", func(t *testing.T) {
		failDelete.Store(false)
		must(t, a.engine.AnonymizeAccount(t.Context(), user.ID))
		must(t, a.engine.AnonymizeAccount(t.Context(), user.ID))
		checkAnonymizationProbe(t, a.engine, false)
		checkAnonymizationProbe(t, sibling.engine, false)
		// No public API exposes this transient mapping. The privacy invariant
		// requires proving the owned store discarded the account-to-anonymous
		// link after the cross-store operation completed.
		var state string
		var unlinked bool
		must(t, w.pool.QueryRow(t.Context(), "SELECT state,anonymous_key IS NULL FROM "+pgx.Identifier{w.schema, "content_account_lifecycle"}.Sanitize()+" WHERE tenant_id=$1 AND actor_id=$2", a.cfg.Scope.AppID.String(), user.ID).Scan(&state, &unlinked))
		if state != "purged" || !unlinked {
			t.Fatal("completed anonymization retained its account-to-anonymous mapping")
		}
		if accountObjects() != 0 {
			t.Fatal("successful terminal retry left account media")
		}
		rows, err := direct.Query(t.Context(), "SELECT count(),countIf(subject_kind='user') FROM "+a.cfg.ClickHouse.Database+".signals FINAL WHERE tenant=? AND event_id=?", a.cfg.Scope.AppID.String(), eventID)
		must(t, err)
		var total, attributed uint64
		if !rows.Next() {
			t.Fatal("retained signal aggregate missing")
		}
		must(t, rows.Scan(&total, &attributed))
		must(t, rows.Close())
		if total != 1 || attributed != 0 {
			t.Fatalf("terminal retries lost/duplicated/reattributed signal: total=%d attributed=%d", total, attributed)
		}
		must(t, a.engine.Close(t.Context()))
		a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
		must(t, err)
		a.server = mountEngine(lifetime, a.engine)
		masked := readComment(t, a, item.ID, comment.ID)
		if masked.UserID != "" || masked.Body != comment.Body || masked.AnonName != "<Deleted>" {
			t.Fatal("restart relinked or removed retained discussion")
		}
		counts, err := a.client("").GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
		must(t, err)
		if counts.Count == nil || *counts.Count != 3 {
			t.Fatal("retry double-counted or lost an anonymous contribution")
		}
		must(t, sibling.engine.AnonymizeAccount(t.Context(), user.ID))
		checkAnonymizationProbe(t, sibling.engine, false)
		kept := readComment(t, sibling, siblingItem.ID, siblingComment.ID)
		if kept.UserID != "" || kept.Body != siblingComment.Body {
			t.Fatal("sibling consumer did not independently anonymize its contribution")
		}
		_, err = a.client(credential).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
		requireError(t, err, 403, "forbidden")
		live, err := a.client(otherCredential).GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
		must(t, err)
		if !live.Favorited {
			t.Fatalf("unrelated account %s lost its favorite", other.ID)
		}
	})
	t.Run("CK-J24-S06_terminal_fence_refuses_late_avatar_and_notification", func(t *testing.T) {
		data := imageBytes(t)
		_, err := a.client(credential).PostMediaUploadPresign(t.Context(), media.PresignBody{Ref: media.RefBody{Kind: "user", ID: user.ID}, Path: "avatar.png", Type: "image/png", Size: int64(len(data)), SHA256: digestBytes(data)})
		requireError(t, err, 404, "not_found")
		published, err := a.publishInbox(t.Context(), inboxPublication{Notifications: []notify.Input{{Recipient: notify.Recipient{Audience: "user", ID: user.ID}, Origin: notify.Origin{System: "critical", Scope: a.cfg.Scope.AppID.String(), ID: uuid.NewString()}, Kind: "late.notice", CreatedAt: time.Now(), Data: json.RawMessage(`{"message":"must remain absent"}`)}}})
		must(t, err)
		if len(published.Items) != 0 {
			t.Fatal("late notification reattached permanently anonymized account")
		}
		inbox, err := a.accountInbox(credential).List(t.Context(), inboxQuery{})
		must(t, err)
		if len(inbox.Items) != 0 || accountObjects() != 0 {
			t.Fatal("terminal fence lost private-resource erasure")
		}
	})
	t.Run("pending_identity_purge_lets_sibling_start_and_replay_finishes_it", func(t *testing.T) {
		eraseAuthKitAccount(t, w, a, appReset.ID)
		sibling.server.Close()
		must(t, sibling.engine.Close(t.Context()))
		pending, err := ck.New(t.Context(), sibling.cfg, sibling.deps)
		must(t, err)
		must(t, pending.Close(t.Context()))
		sibling.engine, err = ck.NewForErasureReplay(t.Context(), sibling.cfg, sibling.deps)
		must(t, err)
		report, err := sibling.engine.ReplayAccountErasures(t.Context())
		must(t, err)
		if !report.Complete() {
			t.Fatalf("sibling account replay incomplete: %+v", report)
		}
		report, err = sibling.engine.ReplayAccountErasures(t.Context())
		must(t, err)
		if !report.Complete() {
			t.Fatal("idempotent sibling replay regressed")
		}
		must(t, sibling.engine.Close(t.Context()))
		sibling.engine, err = ck.New(t.Context(), sibling.cfg, sibling.deps)
		must(t, err)
		sibling.server = mountEngine(lifetime, sibling.engine)
		inbox, err := sibling.accountInbox(appResetCredential).List(t.Context(), inboxQuery{})
		must(t, err)
		if len(inbox.Items) != 0 {
			t.Fatal("full replay retained erased shared inbox")
		}
		favorite, err := sibling.client(appResetCredential).GetContentFavorite(t.Context(), siblingItem.ID, ck.GetContentFavoriteQuery{})
		must(t, err)
		if favorite.Favorited || favorite.Count == nil || *favorite.Count != 2 {
			t.Fatalf("full replay lost an anonymous contribution or kept attribution: %+v", favorite)
		}
	})
}
