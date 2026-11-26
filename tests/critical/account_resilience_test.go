package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/notify"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	medias3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/signal"
)

// eraseAuthKitAccount purges id's account as a host does. AuthKit keeps every
// live group owned (409 last_owner), so the host first hands each group the
// account owns, such as its channels' scopes, to a steward. PurgeUsers then
// ends the recovery window and AuthKit's own River deletes the account, its
// inbox erased in the same transaction. ContentKit's consumer
// (AnonymizeAccount) has not run.
func eraseAuthKitAccount(t *testing.T, w *world, a *application, id string) {
	t.Helper()
	w.startAuth(t)
	var steward *authtest.User
	for m, err := range iam.All(func(p iam.PageRequest) (iam.ListPage[iam.Membership], error) {
		return a.tenant.ListMemberships(t.Context(), iam.UserSubject(id), p)
	}) {
		must(t, err)
		if !m.Role.IsOwner() {
			continue
		}
		if steward == nil {
			// An owner counts only once enrolled in a second factor, as in an
			// ownership transfer.
			u := authtest.NewUser(t, a.tenant)
			u.TOTP = authtest.EnrollTOTP(t, a.tenant, u)
			steward = &u
		}
		authtest.GrantRole(t, a.tenant, iam.GroupByID(m.Group.ID), iam.UserSubject(steward.ID), m.Role)
	}
	results, err := a.tenant.PurgeUsers(t.Context(), []string{id})
	must(t, err)
	for _, result := range results {
		must(t, result.Err)
	}
	waitForWithin(t, 30*time.Second, func() bool {
		_, err := a.tenant.User(t.Context(), iam.UserByID(id), authkit.IncludeDeleted())
		if errors.Is(err, iam.ErrUserNotFound) {
			return true
		}
		must(t, err)
		return false
	})
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
	m := attachMedia(t, w, a, func(c *ck.Config) {
		c.Accounts = &ck.AccountsConfig{Schema: "contentkit_accounts", Avatars: &ck.AvatarsConfig{}}
	})
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
	opened(t, a.client(credential), item.ID)
	activity := func(app *application, id string) uint64 {
		return logged(t, direct, a.cfg.ClickHouse.Database, app.cfg.Scope.AppID.String(), uuid.MustParse(id))
	}
	waitForWithin(t, 30*time.Second, func() bool { return activity(a, user.ID) > 0 })
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
	// Each app's inbox owner is its own deployment: an app-visible notice shows
	// in its app alone, a tenant-wide one in both.
	appNotice := map[*application]string{}
	for _, target := range []struct {
		app *application
		id  string
	}{{a, item.ID}, {sibling, siblingItem.ID}} {
		_, err := target.app.client(appResetCredential).PostContentFavorite(t.Context(), target.id, ck.PostContentFavoriteQuery{})
		must(t, err)
		opened(t, target.app.client(appResetCredential), target.id)
		appNotice[target.app] = uuid.NewString()
		_, err = target.app.publishInbox(t.Context(), inboxPublication{Notifications: []notify.Input{{Recipient: notify.Recipient{Audience: "user", ID: appReset.ID}, Origin: notify.Origin{System: "critical", Scope: target.app.cfg.Scope.AppID.String(), ID: appNotice[target.app]}, Kind: "app.private", CreatedAt: time.Now(), Data: json.RawMessage(`{"message":"App receipt"}`)}}})
		must(t, err)
	}
	// Both apps hold appReset's activity before any erasure.
	waitForWithin(t, 30*time.Second, func() bool { return activity(a, appReset.ID) > 0 && activity(sibling, appReset.ID) > 0 })
	shared, err := w.tenant[0].Notifications(authkit.NotificationProducer{Name: "critical-shared", Origin: notify.OriginScope{System: "critical-shared", Scope: a.cfg.Scope.TenantID.String()}, Visibility: notify.VisibilityTenant})
	must(t, err)
	sharedNotice := uuid.NewString()
	must(t, shared.Publish(t.Context(), []notify.Publication{{RecipientID: appReset.ID, ID: sharedNotice, Revision: 1, Kind: "tenant.shared", CreatedAt: time.Now(), Data: json.RawMessage(`{"message":"Shared account receipt"}`)}}))
	// inboxOf is appReset's inbox in app's view, as the origin IDs it shows.
	inboxOf := func(t *testing.T, app *application) []string {
		t.Helper()
		inbox, err := app.accountInbox(appResetCredential).List(t.Context(), inboxQuery{})
		must(t, err)
		ids := []string{}
		for _, item := range inbox.Items {
			ids = append(ids, item.Origin.ID)
		}
		slices.Sort(ids)
		return ids
	}
	isolated := func(app *application) []string {
		ids := []string{appNotice[app], sharedNotice}
		slices.Sort(ids)
		return ids
	}
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
		prefix := (layout.Scope{TenantID: a.cfg.Scope.TenantID.String()}).Prefix("user", user.ID)
		objects, err := m.management.ListObjectsV2(t.Context(), &awss3.ListObjectsV2Input{Bucket: &m.bucket, Prefix: aws.String(prefix)})
		must(t, err)
		count := 0
		for _, object := range objects.Contents {
			name := aws.ToString(object.Key)
			if !strings.HasSuffix(name, "/"+layout.ManifestName) && !strings.HasSuffix(name, "/"+layout.FenceName) {
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
			if got := inboxOf(t, app); !slices.Equal(got, isolated(app)) {
				t.Fatalf("an app's inbox is not its own notices and the tenant's: %v, want %v", got, isolated(app))
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
		// The inbox is AuthKit's, erased by its purge: one app's erasure leaves
		// the account's inbox, the tenant's notice and the sibling's as they were.
		if got := inboxOf(t, a); !slices.Equal(got, isolated(a)) {
			t.Fatalf("app replay erased or duplicated an account receipt: %v, want %v", got, isolated(a))
		}
		if got := inboxOf(t, sibling); !slices.Equal(got, isolated(sibling)) {
			t.Fatalf("app replay changed sibling inbox: %v, want %v", got, isolated(sibling))
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
		if activity(a, appReset.ID) != 0 {
			t.Fatal("app replay left the target's activity readable")
		}
		if activity(sibling, appReset.ID) == 0 {
			t.Fatal("app replay erased same subject in sibling activity")
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
		if activity(a, user.ID) == 0 {
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
		if len(a.inboxItems(t, user.ID)) != 0 {
			t.Fatal("AuthKit purge retained the app's private inbox")
		}
		if len(sibling.inboxItems(t, user.ID)) != 0 {
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
		if activity(a, user.ID) != 0 {
			t.Fatal("old account still reads activity after its erasure fence")
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
		// Fixture clock: this app's unfinished erasure began 16 minutes ago.
		_, err = w.pool.Exec(t.Context(), `UPDATE contentkit_accounts.content_account_erasure_intents SET requested_at=now()-interval '16 minutes' WHERE tenant_id=$1 AND app_id=$2 AND actor_id=$3`,
			a.cfg.Scope.TenantID.String(), a.cfg.Scope.AppID.String(), user.ID)
		must(t, err)
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
		// The account's activity is fenced once, however often the purge ran.
		rows, err := direct.Query(t.Context(), "SELECT count() FROM "+a.cfg.ClickHouse.Database+".erasures FINAL WHERE tenant=? AND subject=?", a.cfg.Scope.AppID.String(), user.ID)
		must(t, err)
		var fences uint64
		if !rows.Next() {
			t.Fatal("erasure fence missing")
		}
		must(t, rows.Scan(&fences))
		must(t, rows.Close())
		if fences != 1 || activity(a, user.ID) != 0 {
			t.Fatalf("terminal retries left %d fences, %d readable rows", fences, activity(a, user.ID))
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
		// AuthKit refuses the purged account's session; ContentKit's own fence
		// refuses an account this app erased while it still signs in.
		_, err = a.client(credential).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
		requireError(t, err, 401, "unauthorized")
		_, err = a.client(appResetCredential).PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
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
		requireError(t, err, 401, "unauthorized")
		_, err = a.client(appResetCredential).PostMediaUploadPresign(t.Context(), media.PresignBody{Ref: media.RefBody{Kind: "user", ID: appReset.ID}, Path: "avatar.png", Type: "image/png", Size: int64(len(data)), SHA256: digestBytes(data)})
		requireError(t, err, 404, "not_found")
		published, err := a.publishInbox(t.Context(), inboxPublication{Notifications: []notify.Input{{Recipient: notify.Recipient{Audience: "user", ID: user.ID}, Origin: notify.Origin{System: "critical", Scope: a.cfg.Scope.AppID.String(), ID: uuid.NewString()}, Kind: "late.notice", CreatedAt: time.Now(), Data: json.RawMessage(`{"message":"must remain absent"}`)}}})
		must(t, err)
		if len(published.Items) != 0 {
			t.Fatal("late notification reattached permanently anonymized account")
		}
		if len(a.inboxItems(t, user.ID)) != 0 || accountObjects() != 0 {
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
		if len(sibling.inboxItems(t, appReset.ID)) != 0 {
			t.Fatal("AuthKit purge retained the erased shared inbox")
		}
		// The sibling holds no evidence of a purge its consumer never began:
		// its event reader finishes it, as user.purged arrives.
		must(t, sibling.engine.AnonymizeAccount(t.Context(), appReset.ID))
		favorite, err := sibling.client("").GetContentFavorite(t.Context(), siblingItem.ID, ck.GetContentFavoriteQuery{})
		must(t, err)
		var attributed int
		must(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "content_favorites"}.Sanitize()+" WHERE app_id=$1::uuid AND user_id=$2::uuid", sibling.cfg.Scope.AppID.String(), appReset.ID).Scan(&attributed))
		if attributed != 0 || favorite.Count == nil || *favorite.Count != 2 {
			t.Fatalf("sibling erasure lost an anonymous contribution or kept attribution: %+v, %d attributed", favorite, attributed)
		}
	})
}
