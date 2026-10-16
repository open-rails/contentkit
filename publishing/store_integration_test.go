package publishing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/mediatest"
	"github.com/open-rails/contentkit/media/token"
	"github.com/open-rails/contentkit/media/versions"
)

// databaseRoles deliberately uses the catalog's one-connection pool. A
// permission check inside an already-open catalog transaction would deadlock.
type databaseRoles struct{ pool *pgxpool.Pool }

func (r databaseRoles) Can(ctx context.Context, a access.Actor, p string) (bool, error) {
	var n int
	err := r.pool.QueryRow(ctx, "SELECT 1").Scan(&n)
	return a.ID == "publisher" || a.ID == "other", err
}

type databaseBody struct{ pool *pgxpool.Pool }

func (b databaseBody) Sanitize(ctx context.Context, raw string) (string, error) {
	var n int
	err := b.pool.QueryRow(ctx, "SELECT 1").Scan(&n)
	return raw, err
}

type fixture struct {
	s      *Store
	actor  access.Actor
	schema string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	env := mediatest.Open(t)
	schema := env.ContentSchema()
	pc := env.Pool().Config()
	pc.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	roles := databaseRoles{pool}
	a := testauth.Auth{Authz: roles}
	model := kinds.Config{Languages: []string{"en", "ja"}, Kinds: map[string]kinds.Kind{"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one"}, Groups: []string{"artist", "series", "character", "tag"}}}, GroupKinds: map[string]kinds.GroupKind{"artist": {Route: "artists"}, "series": {Route: "series"}, "character": {Route: "characters", ScopedBy: "series"}, "tag": {Route: "tags"}}}
	var cat *catalog.Store
	g, err := groups.New(groups.Options{Pool: pool, Schema: schema, Tenant: env.Tenant, Kinds: model, ContentChanged: func(ctx context.Context, q groups.Querier, ids []string) error {
		return cat.ContentChanged(ctx, q, ids)
	}})
	if err != nil {
		t.Fatal(err)
	}
	cat, err = catalog.New(t.Context(), catalog.Options{Pool: pool, Schema: schema, Tenant: env.Tenant, Kinds: model, Auth: a, Groups: g, Body: databaseBody{pool}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cat.Close)
	v, err := versions.New(versions.Options{Pool: pool, Schema: schema, Tenant: env.Tenant, Kinds: model, Auth: a, Resolver: cat.Resolver(), Facets: cat})
	if err != nil {
		t.Fatal(err)
	}
	mc := media.Config{Namespace: env.Tenant, BaseURL: "https://media.test", Hooks: v.Hooks(), Kinds: []media.Kind{{Name: "gallery", Versions: true, KeepOriginals: true, Publish: &media.Publish{Pages: "pages/{name}", Renditions: []string{"read"}, Import: "import/{name}"}, Uploads: []media.Upload{{Path: "import/{name}", Types: []string{"application/zip"}, MaxBytes: 1 << 30}, {Path: "pages/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20}}, Private: []media.Private{{Name: "read", From: "pages/{name}", To: "read/{name}.webp", Image: &media.Image{Width: 64}}}}}}
	reg, err := media.NewRegistry(mc)
	if err != nil {
		t.Fatal(err)
	}
	journal, q := env.Processing(reg)
	jobs, err := media.NewJobs(media.JobsConfig{Store: env.Store, Registry: reg, Locker: media.PGLocker(pool), Journal: journal, Processes: q, Pool: pool})
	if err != nil {
		t.Fatal(err)
	}
	up, err := media.NewUploads(media.UploadOptions{Store: env.Store, Manifests: jobs.Manifests()})
	if err != nil {
		t.Fatal(err)
	}
	if err = v.Bind(versions.Media{Manifests: jobs.Manifests(), Uploads: up, Jobs: jobs}); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Pool: pool, Schema: schema, Tenant: env.Tenant, Catalog: cat, Groups: g, Registry: reg, Uploads: up, Manifests: jobs.Manifests(), Versions: v, Auth: a})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{s: s, actor: testauth.Actor(access.Actor{ID: "publisher"}, roles), schema: schema}
}

func request() Request {
	sum := sha256.Sum256([]byte("zip"))
	return Request{Content: catalog.ContentInput{Kind: "gallery", Text: map[string]catalog.Text{"en": {Title: "A gallery"}}}, Archive: Archive{Name: "gallery.zip", Size: 3, SHA256: hex.EncodeToString(sum[:])}}
}

func TestPublishingAtomicMetadataAndReplay(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	in := request()
	in.CreateMissingEntities = true
	in.Entities = []EntityReference{{Kind: "artist", Name: "André Example"}, {Kind: "series", Name: "Zenless Zone Zero"}, {Kind: "character", Name: "Miyabi", Scope: &EntityReference{Kind: "series", Name: "Zenless Zone Zero"}}}
	id := uuid.NewString()
	first, err := f.s.Put(ctx, f.actor, id, in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.s.Put(ctx, f.actor, id, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentID != second.ContentID || first.VersionID != second.VersionID || first.Upload.Blob != second.Upload.Blob {
		t.Fatal("replay changed draft or upload")
	}
	if len(first.Entities) != 3 || !first.Entities[0].Created || !first.Entities[1].Created || !first.Entities[2].Created || first.Entities[2].Scope == nil || !first.Entities[2].Scope.Created || first.Entities[2].Scope.ID != first.Entities[1].ID {
		t.Fatalf("entity receipt: %+v", first.Entities)
	}
	if !reflect.DeepEqual(first.Entities, second.Entities) {
		t.Fatal("replay changed entity receipt")
	}
	var count int
	if err = f.s.o.Pool.QueryRow(ctx, `SELECT count(*) FROM `+f.s.items).Scan(&count); err != nil || count != 1 {
		t.Fatalf("contents=%d: %v", count, err)
	}
	var slug string
	if err = f.s.o.Pool.QueryRow(ctx, `SELECT slug FROM `+pgx.Identifier{f.schema, "content_groups"}.Sanitize()+` WHERE kind='series'`).Scan(&slug); err != nil || slug != "zenless-zone-zero" {
		t.Fatalf("slug=%q: %v", slug, err)
	}
	in.Content.Text["en"] = catalog.Text{Title: "Different"}
	_, err = f.s.Put(ctx, f.actor, id, in)
	var failed *OperationError
	if !errors.As(err, &failed) || failed.Code != "operation_conflict" {
		t.Fatalf("changed replay: %v", err)
	}
	// An invalid initial version fails after taxonomy resolution: neither newly
	// resolved labels nor a partial draft may escape the rolled-back transaction.
	bad := request()
	bad.CreateMissingEntities = true
	bad.Entities = []EntityReference{{Kind: "tag", Name: "Must roll back"}}
	bad.Content.Versions = []catalog.VersionInput{{Languages: []string{"not-configured"}}}
	if _, err = f.s.Put(ctx, f.actor, uuid.NewString(), bad); err == nil {
		t.Fatal("invalid metadata accepted")
	}
	if err = f.s.o.Pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{f.schema, "content_groups"}.Sanitize()+` WHERE kind='tag'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial taxonomy escaped: %d %v", count, err)
	}
	if err = f.s.o.Pool.QueryRow(ctx, `SELECT count(*) FROM `+f.s.items).Scan(&count); err != nil || count != 1 {
		t.Fatalf("partial draft escaped: %d %v", count, err)
	}
}

func TestPublishingExactNamesSourceAndOwnership(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	g := f.s.o.Groups
	created, err := g.CreateGroups(ctx, []groups.GroupInput{{Kind: "tag", Slug: "one", Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "One"}, {Language: "en", Kind: groups.NameAlias, Name: "Shared"}}}, {Kind: "tag", Slug: "two", Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Two"}, {Language: "en", Kind: groups.NameAlias, Name: "Shared"}}}})
	if err != nil {
		t.Fatal(err)
	}
	in := request()
	in.Entities = []EntityReference{{Kind: "tag", Name: "Shared"}}
	_, err = f.s.Put(ctx, f.actor, uuid.NewString(), in)
	var failure *OperationError
	if !errors.As(err, &failure) || failure.Code != "ambiguous_entity" {
		t.Fatalf("ambiguity: %v", err)
	}
	in.Entities = []EntityReference{{ID: string(created[0].ID)}}
	in.Source = &Source{Provider: "example", ID: "42"}
	id := uuid.NewString()
	op, err := f.s.Put(ctx, f.actor, id, in)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Put(ctx, f.actor, uuid.NewString(), in)
	if !errors.As(err, &failure) || failure.Code != "source_conflict" {
		t.Fatalf("source collision: %v", err)
	}
	other := testauth.Actor(access.Actor{ID: "other"}, databaseRoles{f.s.o.Pool})
	_, err = f.s.Get(ctx, other, id)
	if !errors.As(err, &failure) || failure.Code != "not_found" {
		t.Fatalf("cross-owner read: %v", err)
	}
	// GET has no storage side effect and never exposes its write capability.
	got, err := f.s.Get(ctx, f.actor, id)
	if err != nil || got.Upload != nil || got.ContentID != op.ContentID {
		t.Fatalf("get: %+v %v", got, err)
	}
}

func TestPublishingRejectsUnsupportedAuthorityBeforeWriting(t *testing.T) {
	f := newFixture(t)
	in := request()
	in.Publish = WhenReady
	in.CreateMissingEntities = true
	in.Entities = []EntityReference{{Kind: "tag", Name: "Not written"}}
	_, err := f.s.Put(t.Context(), f.actor, uuid.NewString(), in)
	var rejected *OperationError
	if !errors.As(err, &rejected) || rejected.Code != "not_configured" {
		t.Fatalf("unsupported authority: %v", err)
	}
	for _, table := range []string{"content_items", "content_groups", "content_publishing_operations"} {
		var count int
		err = f.s.o.Pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{f.schema, table}.Sanitize()).Scan(&count)
		if err != nil || count != 0 {
			t.Fatalf("%s changed: %d %v", table, count, err)
		}
	}
}

func TestPublishingSlugUnicodeAndScopeValidation(t *testing.T) {
	f := newFixture(t)
	in := request()
	in.CreateMissingEntities = true
	in.Entities = []EntityReference{{Kind: "artist", Name: "藤原"}, {Kind: "artist", Name: "André Artist"}}
	if _, err := f.s.Put(t.Context(), f.actor, uuid.NewString(), in); err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.o.Pool.Query(t.Context(), `SELECT slug FROM `+pgx.Identifier{f.schema, "content_groups"}.Sanitize()+` ORDER BY slug`)
	if err != nil {
		t.Fatal(err)
	}
	slugs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(slugs) != 2 || slugs[0] != "andre-artist" || !strings.HasPrefix(slugs[1], "group-") {
		t.Fatalf("slugs: %v", slugs)
	}
	in.Entities = []EntityReference{{Kind: "character", Name: "Miyabi", Scope: &EntityReference{Kind: "artist", Name: "André Artist"}}}
	_, err = f.s.Put(t.Context(), f.actor, uuid.NewString(), in)
	if err == nil {
		t.Fatal(fmt.Sprint("accepted wrong character scope"))
	}
}

// Removal used to lock facets before the catalog row, opposite publication.
// The callback holds publication at its content lock while another pool starts
// the membership deletion; both must finish once publication is released.
func TestMembershipRemovalAndPublicationUseOneLockOrder(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	in := request()
	in.CreateMissingEntities = true
	in.Entities = []EntityReference{{Kind: "tag", Name: "Concurrent"}}
	op, err := f.s.Put(ctx, f.actor, uuid.NewString(), in)
	if err != nil {
		t.Fatal(err)
	}
	var group string
	if err = f.s.o.Pool.QueryRow(ctx, `SELECT group_id FROM `+pgx.Identifier{f.schema, "content_memberships"}.Sanitize()+` WHERE content_id=$1`, op.ContentID).Scan(&group); err != nil {
		t.Fatal(err)
	}
	// The publishing fixture deliberately has one connection. A second pool
	// represents a different API process in this concurrency test.
	otherPool, err := pgxpool.NewWithConfig(ctx, f.s.o.Pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer otherPool.Close()
	otherGroups, err := groups.New(groups.Options{Pool: otherPool, Schema: f.schema, Tenant: f.s.o.Tenant, Kinds: f.s.o.Catalog.Kinds(), ContentChanged: func(ctx context.Context, q groups.Querier, ids []string) error {
		return f.s.o.Catalog.ContentChanged(ctx, q, ids)
	}})
	if err != nil {
		t.Fatal(err)
	}
	locked, release := make(chan struct{}), make(chan struct{})
	published := make(chan error, 1)
	go func() {
		published <- f.s.o.Catalog.PublishChecked(ctx, f.actor, []catalog.Publish{{ContentID: op.ContentID}}, func(context.Context, pgx.Tx) error {
			close(locked)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-locked:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	removed := make(chan error, 1)
	go func() {
		removed <- otherGroups.RemoveMemberships(ctx, []groups.Membership{{GroupID: groups.GroupID(group), ContentID: op.ContentID}})
	}()
	select {
	case err := <-removed:
		t.Fatalf("membership deletion escaped content lock: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(release)
	if err = <-published; err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err = <-removed; err != nil {
		t.Fatalf("remove: %v", err)
	}
}

func TestExpiredMultipartCanBeResumedWithoutAnotherDraft(t *testing.T) {
	f := newFixture(t)
	ring, err := token.NewRing(token.Key{ID: "test", Secret: bytes.Repeat([]byte("t"), 32)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := media.NewUploads(media.UploadOptions{Store: f.s.o.Manifests.Store(), Manifests: f.s.o.Manifests, Tickets: &ring, TicketTTL: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	f.s.o.Uploads = u
	in := request()
	in.Archive.Size = media.MaxSinglePut + 1
	id := uuid.NewString()
	first, err := f.s.Put(t.Context(), f.actor, id, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Upload == nil || first.Upload.Multipart == nil {
		t.Fatal("expected a multipart plan")
	}
	_, err = f.s.Complete(t.Context(), f.actor, id)
	var failed *OperationError
	if !errors.As(err, &failed) || failed.Code != "not_uploaded" || !failed.Retryable {
		t.Fatalf("expired completion: %v", err)
	}
	next, err := f.s.Put(t.Context(), f.actor, id, in)
	if err != nil {
		t.Fatal(err)
	}
	if next.ContentID != first.ContentID || next.VersionID != first.VersionID || next.Upload == nil || next.Upload.Blob == first.Upload.Blob {
		t.Fatalf("expired upload did not renew safely: %+v", next)
	}
}

type testDelegation struct{ actor access.Actor }

func (d testDelegation) Capture(context.Context, access.Actor) (json.RawMessage, error) {
	return json.RawMessage(`{"test_delegation":true}`), nil
}
func (d testDelegation) Resume(context.Context, json.RawMessage) (access.Actor, error) {
	return d.actor, nil
}

func TestPublishingScheduleNormalizesAndReplays(t *testing.T) {
	f := newFixture(t)
	f.s.o.Authority = testDelegation{f.actor}
	at := time.Now().Add(24 * time.Hour).In(time.FixedZone("caller", -6*60*60))
	in := request()
	in.Publish = WhenReady
	in.PublishAt = &at
	id := uuid.NewString()
	first, err := f.s.Put(t.Context(), f.actor, id, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.PublishAt == nil || !first.PublishAt.Equal(at) || first.PublishAt.Location() != time.UTC {
		t.Fatalf("schedule not normalized: %v", first.PublishAt)
	}
	replay, err := f.s.Put(t.Context(), f.actor, id, in)
	if err != nil || replay.ContentID != first.ContentID || !replay.PublishAt.Equal(*first.PublishAt) {
		t.Fatalf("schedule replay: %+v %v", replay, err)
	}
	later := at.Add(time.Hour)
	in.PublishAt = &later
	_, err = f.s.Put(t.Context(), f.actor, id, in)
	var failed *OperationError
	if !errors.As(err, &failed) || failed.Code != "operation_conflict" {
		t.Fatalf("schedule changed under same id: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	in.PublishAt = &past
	if _, err = f.s.Put(t.Context(), f.actor, uuid.NewString(), in); err != nil {
		t.Fatalf("past schedule should publish when ready: %v", err)
	}
	in.Publish = Draft
	_, err = f.s.Put(t.Context(), f.actor, uuid.NewString(), in)
	if !errors.As(err, &failed) || failed.Code != "invalid_request" || failed.Field != "publish_at" {
		t.Fatalf("draft schedule accepted: %v", err)
	}
}
