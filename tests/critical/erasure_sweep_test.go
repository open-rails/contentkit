package critical_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/media"
)

// accountNeedles is id and each base64 run that encodes it, at every byte
// alignment in both alphabets: an id inside encoded bytes is still the id.
func accountNeedles(id string) []string {
	out := []string{id}
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		for k := range 3 {
			s := enc.EncodeToString(append(make([]byte, k), id...))
			out = append(out, s[(4*k+2)/3:4*(k+len(id))/3])
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(out)))
}

// accountColumns counts, per table.column of the catalog schema, the rows
// whose text, array, json or uuid value holds id (accountNeedles).
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
	out, needles := map[string]int{}, accountNeedles(id)
	for _, c := range columns {
		var n int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{w.schema, c.table}.Sanitize()+` t
 WHERE EXISTS (SELECT 1 FROM unnest($1::text[]) n WHERE strpos(t.`+pgx.Identifier{c.name}.Sanitize()+`::text, n) > 0)`, needles).Scan(&n))
		if n > 0 {
			out[c.table+"."+c.name] = n
		}
	}
	return out
}

// bucketHolds reads every object in store, a manifest gunzipped, and returns
// the keys read and those whose bytes hold id (accountNeedles). The account's
// avatar folder is left out: erasure keeps it as the id's storage fence.
func bucketHolds(t *testing.T, a *application, store media.Store, id string) (read map[string]bool, held []string) {
	t.Helper()
	config, err := a.cfg.MediaKinds()
	must(t, err)
	reg, err := mediastore.NewRegistry(config, mediastore.Hooks{})
	must(t, err)
	var fence string
	if kind := reg.Config().Avatars.Kind; kind != "" {
		ref, err := reg.Ref(kind, id)
		must(t, err)
		item, err := reg.Item(ref)
		must(t, err)
		fence = item.Prefix()
	}
	read, needles := map[string]bool{}, accountNeedles(id)
	for obj, err := range store.List(t.Context(), "") {
		must(t, err)
		if fence != "" && strings.HasPrefix(obj.Key, fence) {
			continue
		}
		body, _, err := store.Get(t.Context(), obj.Key, media.GetOptions{})
		must(t, err)
		b, err := io.ReadAll(body)
		body.Close()
		must(t, err)
		if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
			zr, err := gzip.NewReader(bytes.NewReader(b))
			must(t, err)
			b, err = io.ReadAll(zr)
			must(t, err)
		}
		read[obj.Key] = true
		if slices.ContainsFunc(needles, func(n string) bool { return bytes.Contains(b, []byte(n)) }) {
			held = append(held, obj.Key)
		}
	}
	return read, held
}

// galleryManifestKey is the bucket key of ref's manifest.
func galleryManifestKey(t *testing.T, a *application, ref media.RefBody) string {
	t.Helper()
	config, err := a.cfg.MediaKinds()
	must(t, err)
	reg, err := mediastore.NewRegistry(config, mediastore.Hooks{})
	must(t, err)
	cref, err := reg.VersionRef(ref.Kind, ref.ID, ref.Version)
	must(t, err)
	item, err := reg.Item(cref)
	must(t, err)
	return item.ManifestKey()
}

// galleryManifest is ref's manifest key and the manifest stored there.
func galleryManifest(t *testing.T, a *application, store media.Store, ref media.RefBody) (string, *mediastore.Manifest) {
	t.Helper()
	key := galleryManifestKey(t, a, ref)
	body, _, err := store.Get(t.Context(), key, media.GetOptions{})
	must(t, err)
	defer body.Close()
	b, err := io.ReadAll(body)
	must(t, err)
	m, err := mediastore.DecodeManifest(b)
	must(t, err)
	return key, m
}

// A permanent erasure leaves the account id only in its fence and lifecycle
// records: every other column of the catalog schema is swept after a real
// purge of an account that authored, uploaded, owned, interacted and
// administered location rules.
func TestPurgeLeavesNoAccountID(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	storage := attachMedia(t, w, a, func(c *ck.Config) {
		c.Media.Kinds[0].Publish = &media.Publish{Pages: "pages/{name}", Renditions: []string{"reader"}}
	})
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
	page := imageBytes(t)
	_, err = client.IngestMedia(ctx, nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "pages/one.png", Type: "image/png", Body: bytes.NewReader(page)},
		{Path: "pages/two.png", Type: "image/png", Body: bytes.NewReader(page)}})
	must(t, err)
	awaitMediaReady(t, client, ref)
	// The duplicate is removed automatically; the account restores it.
	var removed string
	waitForWithin(t, 45*time.Second, func() bool {
		notices, err := client.GetContentVersionPageNotices(ctx, ref.ID, ref.Version)
		must(t, err)
		for _, n := range notices.Notices {
			if n.Kind == "removed" {
				removed = n.ID
			}
		}
		return removed != ""
	})
	_, err = client.PostContentVersionPageNoticeRestore(ctx, ref.ID, ref.Version, removed)
	must(t, err)
	manifestKey, restored := galleryManifest(t, a, storage.store, ref)
	if !slices.ContainsFunc(restored.Files, func(f media.File) bool {
		page, _ := f.Meta["page"].(map[string]any)
		return page["mode"] == "include"
	}) {
		t.Fatalf("the restore recorded no override: %+v", restored.Files)
	}
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
	t.Run("CK-J24-S11_purge_leaves_no_id_in_the_bucket", func(t *testing.T) {
		read, held := bucketHolds(t, a, storage.store, user.ID)
		if !read[manifestKey] {
			t.Fatalf("the sweep did not read the restored item's manifest %s", manifestKey)
		}
		if len(held) != 0 {
			t.Fatalf("the purge left the account id in %v", held)
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
		_, reader := w.person(t, 0)
		_, err = a.client(reader).GetAccountErasure(ctx, user.ID)
		requireError(t, err, 403, "forbidden")
	})
}

// heldManifest holds the engine's first write of one manifest until released.
type heldManifest struct {
	media.Store
	key     atomic.Pointer[string]
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func (h *heldManifest) Put(ctx context.Context, key string, body io.Reader, size int64, o media.PutOptions) (media.Object, error) {
	if k := h.key.Load(); k != nil && *k == key {
		first := false
		h.once.Do(func() { first = true })
		if first {
			close(h.reached)
			select {
			case <-h.release:
			case <-ctx.Done():
				return media.Object{}, ctx.Err()
			}
		}
	}
	return h.Store.Put(ctx, key, body, size, o)
}

// PresignEditorGet keeps editor reads of the wrapped bucket working.
func (h *heldManifest) PresignEditorGet(ctx context.Context, key string, ttl time.Duration) (media.PresignedRequest, error) {
	return h.Store.(mediastore.EditorReadStore).PresignEditorGet(ctx, key, ttl)
}

// An upload authorized before its account's erasure, whose manifest write
// lands after it, stores the id nowhere: a commit records no uploader.
func TestUploadRacingErasureStoresNoAccountID(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	held := &heldManifest{reached: make(chan struct{}), release: make(chan struct{})}
	storage := attachMediaWith(t, w, a, func(s media.Store) media.Store { held.Store = s; return held })
	ctx := t.Context()
	user := authtest.NewUser(t, a.tenant)
	user.TOTP = authtest.EnrollTOTP(t, a.tenant, user)
	token := authtest.SignIn(t, a.tenant, user).AccessToken
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(user.ID), w.contributor)
	client := a.client(token)
	ref := galleryRef(t, client, "Racing gallery")
	op := stageFile(t, client, ref, "pages/one.png", imageBytes(t))
	key := galleryManifestKey(t, a, ref)
	held.key.Store(&key)
	committed := make(chan error, 1)
	go func() {
		_, err := client.PostMediaUploadCommit(ctx, media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{op}})
		committed <- err
	}()
	select {
	case <-held.reached:
	case err := <-committed:
		t.Fatalf("the commit answered before writing its manifest: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the commit never wrote its manifest")
	}
	must(t, a.engine.AnonymizeAccount(ctx, user.ID))
	close(held.release)
	var err error
	select {
	case err = <-committed:
	case <-time.After(30 * time.Second):
		t.Fatal("the released commit did not answer")
	}
	t.Run("CK-J24-S12_an_upload_racing_the_purge_stores_no_id", func(t *testing.T) {
		// Landing after the scrub or refused on a retry, it stores no id.
		if err == nil {
			awaitMediaReady(t, a.engine.Client(), ref)
		} else {
			requireError(t, err, 403, "forbidden")
		}
		after := accountColumns(t, w, user.ID)
		delete(after, "content_erased_subjects.actor_id")
		delete(after, "content_account_lifecycle.actor_id")
		if len(after) != 0 {
			t.Fatalf("the racing upload stored the erased account in %v", after)
		}
		read, inBucket := bucketHolds(t, a, storage.store, user.ID)
		if err == nil && !read[key] {
			t.Fatalf("the sweep did not read the committed manifest %s", key)
		}
		if len(inBucket) != 0 {
			t.Fatalf("the racing upload stored the erased account in %v", inBucket)
		}
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
