package critical_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/kinds"
)

// accountColumns counts, per table.column of the catalog schema, the rows
// whose text, array, json or uuid value holds id.
func accountColumns(t *testing.T, w *world, id string) map[string]int {
	t.Helper()
	rows, err := w.pool.Query(t.Context(), `SELECT c.table_name, c.column_name FROM information_schema.columns c
 JOIN information_schema.tables tb ON tb.table_schema=c.table_schema AND tb.table_name=c.table_name
 WHERE c.table_schema=$1 AND tb.table_type='BASE TABLE' AND c.data_type IN ('text','character varying','jsonb','json','ARRAY','uuid')
 ORDER BY 1, 2`, w.schema)
	must(t, err)
	type column struct{ table, name string }
	columns, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (column, error) {
		var c column
		return c, row.Scan(&c.table, &c.name)
	})
	must(t, err)
	if len(columns) == 0 {
		t.Fatal("the sweep found no columns")
	}
	out := map[string]int{}
	for _, c := range columns {
		var n int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{w.schema, c.table}.Sanitize()+` WHERE strpos(`+pgx.Identifier{c.name}.Sanitize()+`::text, $1) > 0`, id).Scan(&n))
		if n > 0 {
			out[c.table+"."+c.name] = n
		}
	}
	return out
}

// A permanent erasure leaves the account id only in its fence and lifecycle
// records: every other column of the catalog schema is swept after a real
// purge of an account that authored, uploaded, owned, interacted and
// administered location rules.
func TestPurgeLeavesNoAccountID(t *testing.T) {
	// Upload limits give media its quota ledger, so uploads record their owner.
	w := newWorld(t, func(cfg *ck.Config) {
		limited := *cfg.Content.Channels
		limited.Uploads = kinds.UploadLimits{QuotaBytes: 1 << 30}
		cfg.Content.Channels = &limited
	})
	a := w.apps[0]
	attachMedia(t, w, a)
	startPublicationRuntime(t, a)
	ctx := t.Context()
	user := authtest.NewUser(t, a.tenant)
	user.TOTP = authtest.EnrollTOTP(t, a.tenant, user)
	token := authtest.SignIn(t, a.tenant, user).AccessToken
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(user.ID), w.contributor)
	client := a.client(token)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	channel, err := client.PostChannels(ctx, channels.Input{Slug: "swept-" + suffix, Names: []entities.Name{{Language: "en", Kind: "name", Name: "Swept channel"}}})
	must(t, err)
	credits := []catalog.Creator{{UserID: user.ID, Role: "author"}}
	item, err := client.PostContents(ctx, catalog.ContentInput{Kind: "article", Creators: &credits,
		Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Swept article", Body: "Swept body"}}}}})
	must(t, err)
	_, err = client.PostContentPublish(ctx, item.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, client, item.ID, item.Versions[0].ID)
	postComment(t, client, item.ID, "Swept discussion")
	_, err = client.PostContentFavorite(ctx, item.ID, ck.PostContentFavoriteQuery{})
	must(t, err)
	_, err = client.PostContentLike(ctx, item.ID, ck.PostContentLikeQuery{})
	must(t, err)
	ref := galleryRef(t, client, "Swept gallery")
	_, err = client.IngestMedia(ctx, nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(imageBytes(t))}})
	must(t, err)
	awaitMediaReady(t, client, ref)
	// A member holds one role in a group: the location role replaces the
	// contributor's once the content is in.
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(user.ID), w.locationManager)
	rule, err := client.PostModerationLocationRules(ctx, content.LocationRuleInput{Locations: []content.LocationEntry{{Country: "FR"}}, Target: "site", Domains: []string{"main"}, Note: "swept"})
	must(t, err)
	note := "still swept"
	_, err = client.PatchModerationLocationRule(ctx, rule.ID, content.LocationRuleUpdate{Note: &note})
	must(t, err)

	before := accountColumns(t, w, user.ID)
	for _, held := range []string{"content_items.created_by", "content_creators.user_id", "content_entities.owner_user_id", "content_comments.user_id",
		"content_media_commits.actor_id", "content_media_commits.effects", "content_media_usage.owner_id",
		"content_location_rules.created_by", "content_location_rule_history.actor", "content_location_rule_history.snapshot"} {
		if before[held] == 0 {
			t.Fatalf("fixture did not store the account in %s: %v", held, before)
		}
	}
	state, err := a.engine.AccountErasure(ctx, user.ID)
	must(t, err)
	if state.State != content.AccountStateActive || state.RequestedAt != nil || state.CompletedAt != nil {
		t.Fatalf("live account's erasure state: %+v", state)
	}

	eraseAuthKitAccount(t, w, a, user.ID)
	must(t, a.engine.AnonymizeAccount(ctx, user.ID))
	t.Run("CK-J24-S08_purge_leaves_the_id_only_in_its_fence", func(t *testing.T) {
		after := accountColumns(t, w, user.ID)
		for _, fence := range []string{"content_erased_subjects.actor_id", "content_account_lifecycle.actor_id"} {
			if after[fence] != 1 {
				t.Fatalf("the purge did not record its fence in %s: %v", fence, after)
			}
			delete(after, fence)
		}
		if len(after) != 0 {
			t.Fatalf("the purge left the account id in %v", after)
		}
		if channel.ID == "" || item.ID == "" {
			t.Fatal("swept records lost their identity")
		}
	})
	t.Run("CK-J24-S10_erasure_state_reads_the_completed_purge", func(t *testing.T) {
		local, err := a.engine.AccountErasure(ctx, user.ID)
		must(t, err)
		remote, err := a.engine.Client().GetAccountErasure(ctx, user.ID)
		must(t, err)
		if local.State != content.AccountStatePurged || local.RequestedAt == nil || local.CompletedAt == nil || local.CompletedAt.Before(*local.RequestedAt) {
			t.Fatalf("purged account's erasure state: %+v", local)
		}
		if remote.AccountID != user.ID || remote.State != local.State || remote.RequestedAt == nil || remote.CompletedAt == nil ||
			!remote.RequestedAt.Equal(*local.RequestedAt) || !remote.CompletedAt.Equal(*local.CompletedAt) {
			t.Fatalf("remote erasure state %+v differs from %+v", remote, local)
		}
		_, err = a.client(token).GetAccountErasure(ctx, user.ID)
		requireError(t, err, 403, "forbidden")
	})
}

// Every write that stores an account id refuses an erased one, as interactions
// always have: authored content, credits, channel ownership and imports.
func TestErasedAccountWritesFenced(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	admin, ctx := a.engine.Client(), t.Context()
	user := authtest.NewUser(t, a.tenant)
	user.TOTP = authtest.EnrollTOTP(t, a.tenant, user)
	token := authtest.SignIn(t, a.tenant, user).AccessToken
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(user.ID), w.contributor)
	must(t, a.engine.AnonymizeAccount(ctx, user.ID))
	client := a.client(token)
	text := []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Fenced write"}}}}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	t.Run("CK-J24-S09_an_erased_account_creates_nothing", func(t *testing.T) {
		_, err := client.PostContents(ctx, catalog.ContentInput{Kind: "article", Versions: text})
		requireError(t, err, 403, "forbidden")
		_, err = client.PostChannels(ctx, channels.Input{Slug: "erased-" + suffix, Names: []entities.Name{{Language: "en", Kind: "name", Name: "Erased channel"}}})
		requireError(t, err, 403, "forbidden")
	})
	t.Run("CK-J24-S09_no_write_names_an_erased_account", func(t *testing.T) {
		credits := []catalog.Creator{{UserID: user.ID, Role: "author"}}
		_, err := admin.PostContents(ctx, catalog.ContentInput{Kind: "article", Creators: &credits, Versions: text})
		requireError(t, err, 403, "forbidden")
		res, err := admin.PostImport(ctx, importer.Batch{Source: "erased-subjects",
			Channels: []importer.ChannelRecord{{ExternalID: "owned", Slug: "erased-owned-" + suffix, OwnerUserID: user.ID}},
			Contents: []importer.ContentRecord{{ExternalID: "credited", Kind: "article", State: catalog.StateDraft, Access: &access.Rule{Level: access.Public}, Creators: credits,
				Versions: []importer.VersionRecord{{ExternalID: "credited/version", Languages: []string{"en"}, State: catalog.StatePublished, Text: map[string]catalog.VersionText{"en": {Title: "Credited"}}}}}}})
		must(t, err)
		if got := importFailures(res); !reflect.DeepEqual(got, []importFailure{{importer.SectionChannels, 0, importer.CodeErased}, {importer.SectionContents, 0, importer.CodeErased}}) {
			t.Fatalf("imports naming an erased account: %+v", res)
		}
		after := accountColumns(t, w, user.ID)
		delete(after, "content_erased_subjects.actor_id")
		delete(after, "content_account_lifecycle.actor_id")
		if len(after) != 0 {
			t.Fatalf("a refused write stored the erased account in %v", after)
		}
	})
}

// An app whose signal plane was provisioned keeps an erasure pending on an
// engine without ClickHouse; an app that never had one completes it.
func TestProvisionedSignalPlaneKeepsErasurePending(t *testing.T) {
	w := newWorld(t)
	a, never := w.apps[0], w.apps[1]
	ctx := t.Context()
	addr, chUser, password := os.Getenv("CONTENTKIT_CRITICAL_CH_ADDR"), os.Getenv("CONTENTKIT_CRITICAL_CH_USER"), os.Getenv("CONTENTKIT_CRITICAL_CH_PASSWORD")
	if addr == "" || chUser == "" || password == "" {
		t.Fatal("real owned ClickHouse settings are required for selected erasure tests")
	}
	database := w.schema + "_plane"
	provisioned := a.cfg
	provisioned.ClickHouse = ck.ClickHouseConfig{Addr: addr, Username: chUser, Password: password, Database: database, Cluster: os.Getenv("CONTENTKIT_CRITICAL_CH_CLUSTER")}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		direct, err := clickhouse.Open(&clickhouse.Options{Addr: []string{addr}, Auth: clickhouse.Auth{Username: chUser, Password: password}, DialTimeout: time.Second})
		if err != nil {
			t.Errorf("open ClickHouse for cleanup: %v", err)
			return
		}
		defer direct.Close()
		if err := direct.Exec(ctx, "DROP DATABASE IF EXISTS "+database+" SYNC"); err != nil {
			t.Errorf("drop owned ClickHouse fixture: %v", err)
		}
	})
	// The app's plane is migrated; its running engine has no ClickHouse.
	must(t, ck.MigrateClickHouse(ctx, w.pool, provisioned))
	t.Run("CK-J23-S08_provisioned_plane_without_clickhouse_keeps_erasure_pending", func(t *testing.T) {
		user, _ := w.person(t, 0)
		if err := a.engine.AnonymizeAccount(ctx, user.ID); !errors.Is(err, ck.ErrSignalPlaneRequired) {
			t.Fatalf("an engine without its provisioned signal plane completed an erasure: %v", err)
		}
		state, err := a.engine.AccountErasure(ctx, user.ID)
		must(t, err)
		if state.State != content.AccountStatePurging || state.RequestedAt == nil || state.CompletedAt != nil {
			t.Fatalf("pending erasure state: %+v", state)
		}
	})
	t.Run("CK-J23-S08_app_without_a_plane_completes_erasure", func(t *testing.T) {
		user, _ := w.person(t, 0)
		must(t, never.engine.AnonymizeAccount(ctx, user.ID))
		state, err := never.engine.AccountErasure(ctx, user.ID)
		must(t, err)
		if state.State != content.AccountStatePurged || state.CompletedAt == nil {
			t.Fatalf("erasure without a plane: %+v", state)
		}
	})
}
