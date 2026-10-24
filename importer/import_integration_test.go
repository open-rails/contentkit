package importer_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	pngenc "image/png"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/importer"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/signaltest"
	"github.com/open-rails/contentkit/internal/testauth"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/mediatest"
	"github.com/open-rails/contentkit/media/versions"
	"github.com/open-rails/contentkit/media/workqueue"

	"github.com/open-rails/contentkit/signal"
)

const tenant = "site"

func TestBatchRecordCount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		batch importer.Batch
		want  int
	}{
		{"empty", importer.Batch{}, 0},
		{"metadata", importer.Batch{Source: "legacy", PreferenceFloor: 10, ProvisionScopes: true}, 0},
		{"groups", importer.Batch{Entities: make([]importer.EntityRecord, 2)}, 2},
		{"contents and versions", importer.Batch{Contents: []importer.ContentRecord{{}, {Versions: make([]importer.VersionRecord, 3)}}}, 5},
		{"memberships", importer.Batch{Memberships: make([]importer.MembershipSet, 2)}, 2},

		{"aliases", importer.Batch{Aliases: make([]importer.AliasRecord, 2)}, 2},
		{"comments", importer.Batch{Comments: make([]importer.CommentRecord, 2)}, 2},
		{"reactions", importer.Batch{Reactions: make([]importer.ReactionRecord, 2)}, 2},
		{"favorites", importer.Batch{Favorites: make([]importer.FavoriteRecord, 2)}, 2},
		{"polls and options", importer.Batch{Polls: []importer.PollRecord{{}, {Options: make([]importer.PollOptionRecord, 3)}}}, 5},
		{"votes", importer.Batch{Votes: make([]importer.VoteRecord, 2)}, 2},
		{"answers", importer.Batch{Answers: make([]importer.AnswerRecord, 2)}, 2},
		{"reading", importer.Batch{LegacyReading: make([]importer.LegacyReadingRecord, 2)}, 2},
		{"signals", importer.Batch{Signals: make([]importer.SignalRecord, 2)}, 2},
		{"deletes", importer.Batch{Deletes: make([]importer.DeleteRecord, 2)}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.batch.RecordCount(); got != tc.want {
				t.Fatalf("RecordCount() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestImportRejectsNestedRecordOverflow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		batch importer.Batch
	}{
		{"versions", importer.Batch{Contents: []importer.ContentRecord{{Versions: make([]importer.VersionRecord, importer.MaxRecords)}}}},
		{"options", importer.Batch{Polls: []importer.PollRecord{{Options: make([]importer.PollOptionRecord, importer.MaxRecords)}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.batch.Source = "legacy"
			// Capacity must be rejected before accessing any destination store.
			_, err := new(importer.Service).Import(t.Context(), tc.batch)
			if !errors.Is(err, importer.ErrInvalid) || !strings.Contains(err.Error(), "20001 records") {
				t.Fatalf("overflow error = %v", err)
			}
		})
	}
}

// importer holds root:import:write; staff holds every other permission.
type authz struct{}

func (authz) Can(_ context.Context, a access.Actor, perm string) (bool, error) {
	return a.ID == "importer" || a.ID == "staff" && perm != string(access.ImportWrite), nil
}

func id(kind string, n int64) string { return contentref.LegacyID("test", kind, n, time.Time{}) }

func ptr[T any](v T) *T { return &v }

var (
	t0     = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	future = time.Now().Add(365 * 24 * time.Hour).Truncate(time.Second).UTC()
)

type fixture struct {
	e      *contentkit.Engine
	pool   *pgxpool.Pool
	schema string
	routes http.Handler
}

func newEngine(t *testing.T) fixture { return newEngineWith(t, nil) }

// newEngineWith builds the engine, edited by mutate (media and its store).
func newEngineWith(t *testing.T, mutate func(*contentkit.Config, *contentkit.Deps)) fixture {
	t.Helper()
	ctx := t.Context()
	pool := pgtest.Pool(t, nil)
	pgtest.EnsureExtensions(t, ctx, pool)
	schema := "ck_i_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, name := range []string{schema, schema + "_river", schema + "_media_worker"} {
			_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{name}.Sanitize()+" CASCADE")
		}
		_, _ = pool.Exec(ctx, "DELETE FROM public.migrations WHERE schema = $1", schema)
	})
	cfg := contentkit.Config{Tenant: tenant, Database: contentkit.DatabaseConfig{Schema: schema},
		Content: kinds.Config{Languages: []string{"en", "ja"}, Tiers: []string{"premium"}, Restrictions: []string{"restricted"},
			Kinds: map[string]kinds.Kind{
				"gallery": {Route: "g", Presentation: "reader", Versions: kinds.VersionRules{Languages: "one", Traits: []string{"colored"}}, Segments: true,
					Entities: []string{"tag", "artist", "series", "character"}, Attributes: map[string]kinds.Attribute{"format": {Type: "enum", Values: []string{"doujin", "manga"}}}},
				"blog": {Route: "blog", Presentation: "post", Versions: kinds.VersionRules{Languages: "one", Hidden: true}, Entities: []string{"tag"}},
			},
			EntityKinds: map[string]kinds.EntityKind{
				"tag":       {Route: "tags"},
				"artist":    {Route: "artists", Roles: []string{"publisher"}},
				"series":    {Route: "series", Ordered: true, Parent: "series"},
				"character": {Route: "characters", ScopedBy: "series"},
				"channel":   {Route: "channels", Owner: "user", AuthScope: true},
			}},
		Interactions: contentkit.InteractionsConfig{Limits: contentkit.LimitsConfig{Disabled: true}}}
	deps := contentkit.Deps{Postgres: pool, Auth: testauth.Auth{Authz: authz{}}}
	if mutate != nil {
		mutate(&cfg, &deps)
	}
	e, err := contentkit.New(ctx, cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	routes, err := e.Routes(contentkit.Routes{Prefix: "/api", Groups: e.RouteGroups() &^ (contentkit.UploadRoutes | contentkit.MediaRoutes)})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	for _, r := range routes {
		mux.Handle(r.Path, r.Handler)
	}
	return fixture{e: e, pool: pool, schema: schema, routes: mux}
}

// catalogBatch is a small catalog in every section.
func catalogBatch() importer.Batch {
	tag := func(n int) importer.Ref { return importer.Ref{ExternalID: "tag:" + string(rune('0'+n))} }
	gallery1 := importer.Ref{ExternalID: "gallery:1"}
	comment := func(n int) *importer.Ref { return &importer.Ref{ExternalID: "comment:" + string(rune('0'+n))} }
	return importer.Batch{
		Source: "test-legacy",
		Entities: []importer.EntityRecord{
			{ExternalID: "tag:1", ID: id("tag", 1), Kind: "tag", Slug: "big", Code: "TAG000001",
				Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Big"}, {Language: "en", Kind: entities.NameAlias, Name: "Huge"}},
				Text:  []entities.Text{{Language: "en", Description: "Big things."}}, CreatedAt: &t0},
			{ExternalID: "tag:2", Kind: "tag", Slug: "color", Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Color"}}, CreatedAt: &t0},
			{ExternalID: "artist:1", ID: id("artist", 1), Kind: "artist", Slug: "artist-one",
				Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Artist One"}, {Language: "ja", Kind: entities.NameCanonical, Name: "アーティスト"}}},
			{ExternalID: "series:1", ID: id("series", 1), Kind: "series", Slug: "saga", Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Saga"}}},
			{ExternalID: "series:2", ID: id("series", 2), Kind: "series", Slug: "saga-two", Parent: &importer.Ref{ExternalID: "series:1"},
				Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Saga Two"}}},
			{ExternalID: "char:1", ID: id("character", 1), Kind: "character", Slug: "hero", Scope: &importer.Ref{ExternalID: "series:2"},
				Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Hero"}},
				Edges: []importer.EdgeRecord{{Relation: "member_of", To: importer.Ref{ExternalID: "series:1"}}}},
		},
		Contents: []importer.ContentRecord{
			{ExternalID: "gallery:1", ID: id("gallery", 1), Kind: "gallery", State: "published", PublishedAt: ptr(t0.Add(24 * time.Hour)), Code: "GAZ000001",
				Comments: "closed",
				Access:   &access.Rule{Level: access.TierLevel, Tiers: []string{"premium"}}, Attributes: map[string]any{"format": "doujin"},
				Creators: []catalog.Creator{{UserID: "u-1"}},
				Text:     map[string]catalog.Text{"en": {Title: "First", Description: "The first."}, "ja": {Title: "最初"}},
				Versions: []importer.VersionRecord{
					{ExternalID: "version:1", ID: id("version", 1), Code: "k3f9", Languages: []string{"en"}, Ordinal: ptr(1), PublishedAt: ptr(t0.Add(24 * time.Hour)),
						Text:     map[string]catalog.VersionText{"en": {Title: "First, in English"}},
						Segments: []catalog.Segment{{Start: 0, End: 10}, {Start: 10, End: 20, ReleaseAt: &future}}, CreatedAt: &t0},
					{ExternalID: "version:2", ID: id("version", 2), Languages: []string{"ja"}, Traits: []string{"colored"}, State: "draft", CreatedAt: &t0},
				}, CreatedAt: &t0, UpdatedAt: ptr(t0.Add(48 * time.Hour))},
			{ExternalID: "gallery:2", ID: id("gallery", 2), Kind: "gallery", State: "deleted", PublishedAt: &t0, DeletedAt: ptr(t0.Add(72 * time.Hour)),
				Text:     map[string]catalog.Text{"en": {Title: "Gone"}},
				Versions: []importer.VersionRecord{{ExternalID: "version:3", Languages: []string{"en"}, PublishedAt: &t0, CreatedAt: &t0}}, CreatedAt: &t0},
			{ExternalID: "gallery:3", ID: id("gallery", 3), Kind: "gallery", State: "published", PublishAt: &future, Restrictions: []string{"restricted"},
				Text:     map[string]catalog.Text{"en": {Title: "Soon"}},
				Versions: []importer.VersionRecord{{ExternalID: "version:4", ID: id("version", 4), Languages: []string{"en"}, CreatedAt: &t0}}, CreatedAt: &t0},
			{ExternalID: "post:1", ID: id("post", 1), Kind: "blog", State: "published", PublishedAt: &t0, Text: map[string]catalog.Text{"en": {Title: "News", Body: "<p>Hello</p>"}},
				CreatedAt: &t0},
		},
		Memberships: []importer.MembershipSet{
			{Content: gallery1, Kinds: []string{"tag"}, Memberships: []importer.MembershipRecord{{Entity: tag(1)}, {Entity: tag(2)}}},
			{Content: gallery1, Kinds: []string{"artist"}, Memberships: []importer.MembershipRecord{{Entity: importer.Ref{ExternalID: "artist:1"}},
				{Entity: importer.Ref{ExternalID: "artist:1"}, Role: "publisher"}}},
			{Content: gallery1, Kinds: []string{"series"}, Memberships: []importer.MembershipRecord{{Entity: importer.Ref{ExternalID: "series:2"}, Position: ptr(3)}}},
			{Content: gallery1, Kinds: []string{"character"}, Memberships: []importer.MembershipRecord{{Entity: importer.Ref{ExternalID: "char:1"}}}},
			{Content: importer.Ref{ExternalID: "post:1"}, Kinds: []string{"tag"}, Memberships: []importer.MembershipRecord{{Entity: tag(1)}}},
		},

		Aliases: []importer.AliasRecord{{Kind: "folder", Key: "12345", Target: importer.TargetRef{Content: &gallery1}}, {Kind: "tag-name", Key: "Big", Target: importer.TargetRef{Entity: ptr(tag(1))}}},
		Comments: []importer.CommentRecord{
			{ExternalID: "comment:1", ID: id("comment", 1), Target: importer.TargetRef{Content: &gallery1}, Language: "en", UserID: "u-1", Body: "Great", CreatedAt: &t0},
			{ExternalID: "comment:2", ID: id("comment", 2), Target: importer.TargetRef{Content: &gallery1}, Language: "en", AnonName: "guest", Body: "Agreed",
				ReplyTo: comment(1), CreatedAt: ptr(t0.Add(time.Hour))},
			{ExternalID: "comment:3", ID: id("comment", 3), Target: importer.TargetRef{Content: &gallery1}, UserID: "u-2", Body: "spam", DeletedAt: ptr(t0.Add(time.Hour)), CreatedAt: &t0},
			{ExternalID: "comment:4", ID: id("comment", 4), Target: importer.TargetRef{Content: &gallery1}, Language: "ja", UserID: "u-3", Body: "同意",
				ReplyTo: comment(2), CreatedAt: ptr(t0.Add(2 * time.Hour))},
		},
		Reactions: []importer.ReactionRecord{
			{Target: importer.TargetRef{Content: &gallery1}, UserID: "u-1", Value: 1, CreatedAt: &t0},
			{Target: importer.TargetRef{Content: &gallery1}, UserID: "u-2", Value: -1, CreatedAt: &t0},
			{Target: importer.TargetRef{Content: &gallery1}, IP: "203.0.113.9", Value: 1, CreatedAt: &t0},
			{Target: importer.TargetRef{Comment: comment(1)}, UserID: "u-1", Value: 1, CreatedAt: &t0},
			{Target: importer.TargetRef{Entity: ptr(tag(1))}, UserID: "u-1", Value: -1, CreatedAt: &t0},
			{Target: importer.TargetRef{Version: &importer.Ref{ExternalID: "version:1"}}, UserID: "u-3", Value: 0, CreatedAt: &t0},
		},
		Favorites: []importer.FavoriteRecord{{Target: importer.TargetRef{Content: &gallery1}, UserID: "u-1", CreatedAt: &t0},
			{Target: importer.TargetRef{Version: &importer.Ref{ExternalID: "version:1"}}, UserID: "u-2", CreatedAt: &t0}},
		Polls: []importer.PollRecord{
			{ExternalID: "poll:1", ID: id("poll", 1), Question: "Best?", Language: "en", CreatedAt: &t0,
				Options: []importer.PollOptionRecord{{ExternalID: "answer:1", ID: id("option", 1), Label: "This", Position: 0}, {ExternalID: "answer:2", Label: "That", Position: 1}}},
			{ExternalID: "poll:2", Kind: "free_text", Question: "Why?", Language: "en", CreatedAt: &t0},
		},
		Votes: []importer.VoteRecord{
			{Poll: importer.Ref{ExternalID: "poll:1"}, Option: importer.Ref{ExternalID: "answer:1"}, UserID: "u-1", CreatedAt: &t0},
			{Poll: importer.Ref{ExternalID: "poll:1"}, Option: importer.Ref{ExternalID: "answer:2"}, IP: "203.0.113.9", CreatedAt: &t0},
			{Poll: importer.Ref{ExternalID: "poll:1"}, Option: importer.Ref{ExternalID: "answer:1"}, UserID: "u-2", CreatedAt: &t0},
		},
		Answers: []importer.AnswerRecord{{Poll: importer.Ref{ExternalID: "poll:2"}, UserID: "u-1", Text: "Because", CreatedAt: &t0}},
		LegacyReading: []importer.LegacyReadingRecord{
			{Target: importer.TargetRef{Version: &importer.Ref{ExternalID: "version:1"}}, UserID: "u-1", FirstReadAt: &t0, LastReadAt: t0.Add(time.Hour), Progress: 5, ProgressMax: 20, Resume: "005", Revision: 1},
			{Target: importer.TargetRef{Content: &gallery1}, AnonKey: "a-1", LastReadAt: t0, Progress: 20, ProgressMax: 20, Completed: true, Resume: "020"},
		},
		Signals: []importer.SignalRecord{
			{Target: importer.TargetRef{Version: &importer.Ref{ExternalID: "version:1"}}, UserID: "u-1", Type: "click", EventID: "click:1", OccurredAt: t0},
			{Target: importer.TargetRef{Content: &gallery1}, AnonKey: "a-1", Type: "click", EventID: "click:2", OccurredAt: t0},
		},
	}
}

func (f fixture) importBatch(t *testing.T, b importer.Batch) importer.Result {
	t.Helper()
	res, err := f.e.Client().PostImport(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (f fixture) scalar(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var out string
	if err := f.pool.QueryRow(t.Context(), strings.ReplaceAll(sql, "$s.", pgx.Identifier{f.schema}.Sanitize()+"."), args...).Scan(&out); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return out
}

// fingerprint is every imported table's content.
func (f fixture) fingerprint(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range []string{"content_items", "content_text", "content_creators", "content_versions", "content_version_text", "content_segments",
		"content_facets", "content_events", "content_entities", "content_entity_names", "content_entity_text", "content_entity_edges", "content_memberships",
		"content_codes", "content_code_aliases", "content_external_ids", "content_comments", "content_reactions", "content_favorites",
		"content_interaction_counts", "content_poll_questions", "content_poll_options", "content_poll_votes", "content_poll_answers", "content_search_dirty"} {
		out[table] = f.scalar(t, `SELECT count(*) || ':' || md5(coalesce(string_agg(x::text, '|' ORDER BY x::text), '')) FROM $s.`+table+` x`)
	}
	return out
}

// A small catalog in every section imports with its ids, times and
// threads kept and its counts exact; importing it again changes nothing.
func TestImportConverges(t *testing.T) {
	f := newEngine(t)
	b := catalogBatch()
	res := f.importBatch(t, b)
	if len(res.Failed) > 0 {
		t.Fatalf("failures: %+v", res.Failed)
	}
	want := map[string]int{"entities": 6, "contents": 4, "memberships": 5, "aliases": 2, "comments": 4, "reactions": 6, "favorites": 2, "polls": 2,
		"votes": 3, "answers": 1}
	for s, n := range want {
		if res.Written[s] != n {
			t.Errorf("written %s = %d, want %d (%v)", s, res.Written[s], n, res.Written)
		}
	}
	if res.Skipped["signals"] != 2 || res.Skipped["legacy_reading"] != 2 {
		t.Errorf("skipped %v: clicks and legacy bookmarks need a signal plane", res.Skipped)
	}
	if res.IDs["content"]["gallery:1"] != id("gallery", 1) || res.IDs["version"]["version:2"] != id("version", 2) || res.IDs["group"]["tag:1"] != id("tag", 1) ||
		res.IDs["version"]["version:3"] == "" || res.IDs["group"]["tag:2"] == "" || res.IDs["poll_option"]["answer:2"] == "" {
		t.Errorf("ids %v", res.IDs)
	}
	g1, c1 := id("gallery", 1), id("comment", 1)
	for _, c := range [][2]string{
		{`SELECT state || ' ' || extract(epoch FROM published_at)::bigint || ' ' || extract(epoch FROM created_at)::bigint || ' ' || extract(epoch FROM updated_at)::bigint
 FROM $s.content_items WHERE id = $1`, "published 1577923200 1577836800 1578009600"},
		{`SELECT access::text || ' ' || attributes::text FROM $s.content_items WHERE id = $1`, `{"level": "tier", "tiers": ["premium"]} {"format": "doujin"}`},
		{`SELECT comments FROM $s.content_items WHERE id = $1`, "closed"},
		{`SELECT string_agg(language || '=' || title, ',' ORDER BY language) FROM $s.content_text WHERE content_id = $1`, "en=First,ja=最初"},
		{`SELECT string_agg(id || ':' || position || ':' || state || ':' || coalesce(ordinal, 0), ',' ORDER BY position) FROM $s.content_versions WHERE content_id = $1`, id("version", 1) + ":0:published:1," + id("version", 2) + ":1:draft:0"},
		{`SELECT count(*)::text FROM $s.content_segments WHERE version_id = (SELECT id FROM $s.content_versions WHERE content_id = $1 AND position = 0)`, "2"},
		{`SELECT code FROM $s.content_codes WHERE content_id = $1`, "GAZ000001"},
		{`SELECT code FROM $s.content_versions WHERE content_id = $1 AND position = 0`, "K3F9"},
		{`SELECT likes || '/' || dislikes || '/' || favorites || '/' || comment_count FROM $s.content_interaction_counts WHERE content_id = $1 AND content_version_id IS NULL`, "2/1/1/1"},
		{`SELECT array_length(entity_ids, 1)::text FROM $s.content_facets WHERE content_id = $1 AND version_id = (SELECT id FROM $s.content_versions WHERE content_id = $1 AND position = 0)`, "6"},
	} {
		sql, want := c[0], c[1]
		if got := f.scalar(t, sql, g1); got != want {
			t.Errorf("%s\n got %q\nwant %q", sql, got, want)
		}
	}
	for _, c := range [][2]string{
		{`SELECT reply_count || '/' || likes || '/' || language FROM $s.content_comments WHERE id = $1::uuid`, "2/1/en"},
		{`SELECT string_agg(id::text, ',' ORDER BY created_at) FROM $s.content_comments WHERE reply_to_id = $1::uuid`, id("comment", 2) + "," + id("comment", 4)},
		{`SELECT string_agg(vote_count::text, ',' ORDER BY position) FROM $s.content_poll_options WHERE question_id = (SELECT id FROM $s.content_poll_questions WHERE question = 'Best?') AND $1 <> ''`, "2,1"},
		{`SELECT code FROM $s.content_codes WHERE content_id = '` + id("tag", 1) + `' AND $1 <> ''`, "TAG000001"},
		{`SELECT state || ' ' || (published_at IS NULL) FROM $s.content_items WHERE id = '` + id("gallery", 3) + `' AND $1 <> ''`, "published true"},
		{`SELECT comments FROM $s.content_items WHERE id = '` + id("gallery", 3) + `' AND $1 <> ''`, "open"},
		{`SELECT array_to_string(languages, ',') FROM $s.content_versions WHERE content_id = '` + id("post", 1) + `' AND $1 <> ''`, "en"},
		{`SELECT scope_id || ' ' || slug FROM $s.content_entities WHERE id = '` + id("character", 1) + `' AND $1 <> ''`, id("series", 2) + " hero"},
		{`SELECT count(*)::text FROM $s.content_code_aliases WHERE source = 'test-legacy' AND $1 <> ''`, "2"},
		{`SELECT count(*)::text FROM $s.content_reactions WHERE content_kind = 'tag' AND content_id = '` + id("tag", 1) + `' AND $1 <> ''`, "1"},
	} {
		sql, want := c[0], c[1]
		if got := f.scalar(t, sql, c1); got != want {
			t.Errorf("%s\n got %q\nwant %q", sql, got, want)
		}
	}
	before := f.fingerprint(t)
	if again := f.importBatch(t, catalogBatch()); len(again.Failed) > 0 || !mapsEqual(again.IDs, res.IDs) {
		t.Fatalf("re-import: failures %+v, ids %v", again.Failed, again.IDs)
	}
	after := f.fingerprint(t)
	for table, fp := range before {
		if after[table] != fp {
			t.Errorf("re-import changed %s: %s -> %s", table, fp, after[table])
		}
	}
	// A changed record is updated alone.
	edit := catalogBatch()
	edit.Contents = edit.Contents[:1]
	edit.Contents[0].Text["en"] = catalog.Text{Title: "First, revised"}
	revisions := func() map[string]int64 {
		t.Helper()
		rows, err := f.pool.Query(t.Context(), `SELECT id, revision FROM `+pgx.Identifier{f.schema, "content_items"}.Sanitize()+` ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int64{}
		for rows.Next() {
			var id string
			var revision int64
			if err := rows.Scan(&id, &revision); err != nil {
				t.Fatal(err)
			}
			out[id] = revision
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	oldRevisions := revisions()
	if res := f.importBatch(t, importer.Batch{Source: edit.Source, Contents: edit.Contents}); len(res.Failed) > 0 || res.Written["contents"] != 1 {
		t.Fatalf("edit: %+v", res)
	}
	newRevisions := revisions()
	if len(newRevisions) != len(oldRevisions) {
		t.Errorf("content set changed: %v -> %v", oldRevisions, newRevisions)
	}
	for id, old := range oldRevisions {
		want := old
		if id == g1 {
			want++
		}
		if got := newRevisions[id]; got != want {
			t.Errorf("content %s revision = %d, want %d", id, got, want)
		}
	}
	if f.scalar(t, `SELECT title FROM $s.content_text WHERE content_id = $1 AND language = 'en'`, g1) != "First, revised" {
		t.Error("changed record title was not updated")
	}
	if f.scalar(t, `SELECT count(*)::text FROM $s.content_external_ids WHERE $1 <> ''`, "x") != "22" {
		t.Errorf("external ids duplicated: %s", f.scalar(t, `SELECT count(*)::text FROM $s.content_external_ids WHERE $1 <> ''`, "x"))
	}
}

// Comments remain an independently imported policy: re-sending converges,
// an edit is visible and an omitted setting is open. Bare counters are retired.
func TestImportCommentsSetting(t *testing.T) {
	f := newEngine(t)
	gallery := func(n int64, comments string) importer.ContentRecord {
		ext := fmt.Sprintf("gallery:%d", n)
		return importer.ContentRecord{ExternalID: ext, ID: id("gallery", n), Kind: "gallery", State: "published", PublishedAt: &t0, Comments: comments,
			Text:     map[string]catalog.Text{"en": {Title: ext}},
			Versions: []importer.VersionRecord{{ExternalID: "version:" + ext, ID: id("version", n), Languages: []string{"en"}, PublishedAt: &t0}}}
	}
	read := func() map[string]string {
		t.Helper()
		page, err := f.e.Client().GetContents(t.Context(), contentkit.GetContentsQuery{Kind: "gallery"})
		if err != nil {
			t.Fatal(err)
		}
		state := map[string]string{}
		for _, c := range page.Contents {
			item, err := f.e.Client().GetContent(t.Context(), c.ID, contentkit.GetContentQuery{})
			if err != nil {
				t.Fatal(err)
			}
			state[c.ID] = item.Comments
		}
		return state
	}
	g1, g2 := id("gallery", 1), id("gallery", 2)
	b := importer.Batch{Source: "test-legacy", Contents: []importer.ContentRecord{gallery(1, "hidden"), gallery(2, "")}}
	if res := f.importBatch(t, b); len(res.Failed) > 0 {
		t.Fatalf("import %+v", res.Failed)
	}
	state := read()
	if state[g1] != "hidden" || state[g2] != "open" {
		t.Fatalf("after import %v", state)
	}
	revisions := f.scalar(t, `SELECT string_agg(revision::text, ',' ORDER BY id) FROM $s.content_items WHERE $1 <> ''`, "x")
	if res := f.importBatch(t, b); len(res.Failed) > 0 || f.scalar(t, `SELECT string_agg(revision::text, ',' ORDER BY id) FROM $s.content_items WHERE $1 <> ''`, "x") != revisions {
		t.Fatalf("re-import changed the items: %+v", res)
	}
	b.Contents[0] = gallery(1, "")
	if res := f.importBatch(t, b); len(res.Failed) > 0 {
		t.Fatalf("edit %+v", res.Failed)
	}
	if state := read(); state[g1] != "open" || state[g2] != "open" {
		t.Fatalf("after edit %v", state)
	}
	res := f.importBatch(t, importer.Batch{Source: "test-legacy", Contents: []importer.ContentRecord{gallery(1, "maybe")}})
	if len(res.Failed) != 1 || res.Failed[0].Code != importer.CodeInvalid {
		t.Fatalf("invalid records %+v", res)
	}
}

func mapsEqual(a, b map[string]map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, m := range a {
		n := b[k]
		if len(m) != len(n) {
			return false
		}
		for x, v := range m {
			if n[x] != v {
				return false
			}
		}
	}
	return true
}

// Records that fail are reported and the rest written, in the same
// transaction; sending the batch again once fixed completes it.
func TestImportPartialFailureResumes(t *testing.T) {
	f := newEngine(t)
	gallery := func(n int, state string, owner *importer.Ref) importer.ContentRecord {
		ext := "gallery:" + string(rune('0'+n))
		return importer.ContentRecord{ExternalID: ext, ID: id("gallery", int64(n)), Kind: "gallery", State: state, OwnerEntity: owner,
			Text: map[string]catalog.Text{"en": {Title: ext}}, Versions: []importer.VersionRecord{{ExternalID: "version:" + ext, Languages: []string{"en"}}}}
	}
	b := importer.Batch{Source: "test-legacy",
		Contents: []importer.ContentRecord{gallery(1, "published", nil), gallery(2, "published", &importer.Ref{ExternalID: "tag:9"}), gallery(3, "live", nil), gallery(4, "draft", nil)},
		Comments: []importer.CommentRecord{
			{ExternalID: "c:1", Target: importer.TargetRef{Content: &importer.Ref{ExternalID: "gallery:1"}}, UserID: "u", Body: "hi"},
			{ExternalID: "c:2", Target: importer.TargetRef{Content: &importer.Ref{ExternalID: "gallery:3"}}, UserID: "u", Body: "lost"},
			{ExternalID: "c:3", Target: importer.TargetRef{Content: &importer.Ref{ExternalID: "gallery:1"}}, UserID: "u", Body: "orphan", ReplyTo: &importer.Ref{ID: id("comment", 99)}},
		},
	}
	res := f.importBatch(t, b)
	failed := map[string]string{}
	for _, x := range res.Failed {
		failed[x.Section+" "+x.ExternalID] = x.Code
	}
	wantFailed := map[string]string{"contents gallery:2": "not_found", "contents gallery:3": "invalid", "comments c:2": "not_found", "comments c:3": "not_found"}
	if !mapsEqual(map[string]map[string]string{"": failed}, map[string]map[string]string{"": wantFailed}) || res.Written["contents"] != 2 || res.Written["comments"] != 1 {
		t.Fatalf("result %+v", res)
	}
	if f.scalar(t, `SELECT string_agg(id, ',' ORDER BY id) FROM $s.content_items WHERE $1 <> ''`, "x") != id("gallery", 1)+","+id("gallery", 4) {
		t.Fatal("the failed records were written")
	}
	// Fix the causes and send it all again.
	b.Entities = []importer.EntityRecord{{ExternalID: "tag:9", ID: id("tag", 9), Kind: "tag", Slug: "nine"}}
	b.Contents[2].State = "published"
	b.Comments[2].ReplyTo = &importer.Ref{ExternalID: "c:1"}
	res = f.importBatch(t, b)
	if len(res.Failed) > 0 || res.Written["contents"] != 4 || res.Written["comments"] != 3 {
		t.Fatalf("resume %+v", res)
	}
	for _, c := range [][2]string{
		{`SELECT count(*)::text FROM $s.content_items WHERE $1 <> ''`, "4"},
		{`SELECT count(*)::text FROM $s.content_comments WHERE $1 <> ''`, "3"},
		{`SELECT count(*)::text FROM $s.content_external_ids WHERE entity = 'content' AND $1 <> ''`, "4"},
		{`SELECT owner_entity_id FROM $s.content_items WHERE id = '` + id("gallery", 2) + `' AND $1 <> ''`, id("tag", 9)},
	} {
		sql, want := c[0], c[1]
		if got := f.scalar(t, sql, "x"); got != want {
			t.Errorf("%s = %q, want %q", sql, got, want)
		}
	}
	// A given id never moves an imported record.
	b.Contents[0].ID = id("gallery", 7)
	res = f.importBatch(t, importer.Batch{Source: b.Source, Contents: b.Contents[:1]})
	if len(res.Failed) != 1 || res.Failed[0].Code != "conflict" {
		t.Fatalf("moved id %+v", res)
	}
}

// A record that fails after naming an identity does not take that identity's
// mapping with it: a later record of the chunk with the same identity is
// written and mapped.
func TestImportMapsIdentityAfterFailedFirstOccurrence(t *testing.T) {
	f := newEngine(t)
	gallery := func(state string) importer.ContentRecord {
		return importer.ContentRecord{ExternalID: "gallery:5", ID: id("gallery", 5), Kind: "gallery", State: state,
			Text: map[string]catalog.Text{"en": {Title: "G"}}, Versions: []importer.VersionRecord{{ExternalID: "version:gallery:5", Languages: []string{"en"}}}}
	}
	res := f.importBatch(t, importer.Batch{Source: "test-legacy", Contents: []importer.ContentRecord{gallery("live"), gallery("published")}})
	if len(res.Failed) != 1 || res.Written["contents"] != 1 {
		t.Fatalf("result %+v", res)
	}
	if got := f.scalar(t, `SELECT string_agg(id, ',') FROM $s.content_external_ids WHERE entity = 'content' AND external_id = $1`, "gallery:5"); got != id("gallery", 5) {
		t.Fatalf("mapping for the written record = %q, want %q", got, id("gallery", 5))
	}
}

// Every imported version has a code, unique within its content: a given one
// is kept, the others assigned; re-importing keeps them, and a code another
// version holds, or a different one for a stored version, is a conflict.
func TestImportVersionCodes(t *testing.T) {
	f := newEngine(t)
	gallery := func(n int64, codes ...string) importer.ContentRecord {
		c := importer.ContentRecord{ExternalID: fmt.Sprint("gallery:", n), ID: id("gallery", n), Kind: "gallery", State: "published",
			Text: map[string]catalog.Text{"en": {Title: "G"}}}
		for i, code := range codes {
			c.Versions = append(c.Versions, importer.VersionRecord{ExternalID: fmt.Sprint("version:", n, ":", i), Code: code, Languages: []string{"en"}})
		}
		return c
	}
	codes := func(n int64) []string {
		return strings.Split(f.scalar(t, `SELECT string_agg(code, ',' ORDER BY position) FROM $s.content_versions WHERE content_id = $1`, id("gallery", n)), ",")
	}
	b := importer.Batch{Source: "test-legacy", Contents: []importer.ContentRecord{gallery(1, "", "", "AB12"), gallery(2, "ab-12")}}
	if res := f.importBatch(t, b); len(res.Failed) > 0 {
		t.Fatalf("failures %+v", res.Failed)
	}
	one := codes(1)
	if len(one) != 3 || one[2] != "AB12" || one[0] == one[1] || slices.Contains(one[:2], "AB12") || codes(2)[0] != "AB12" {
		t.Fatalf("codes %v %v", one, codes(2))
	}
	for _, c := range one {
		if code, err := contenturl.ParseVersionCode(c); err != nil || string(code) != c {
			t.Fatalf("code %q: %v", c, err)
		}
	}
	if res := f.importBatch(t, importer.Batch{Source: b.Source, Contents: []importer.ContentRecord{gallery(1, "", one[1], "")}}); len(res.Failed) > 0 ||
		!slices.Equal(codes(1), one) {
		t.Fatalf("re-import %+v: %v, want %v", res.Failed, codes(1), one)
	}
	for _, c := range []importer.ContentRecord{gallery(1, "ZZ99"), gallery(1, one[1], "", "")} {
		if res := f.importBatch(t, importer.Batch{Source: b.Source, Contents: []importer.ContentRecord{c}}); len(res.Failed) != 1 || res.Failed[0].Code != "conflict" {
			t.Fatalf("conflict %v: %+v", c.Versions, res)
		}
	}
	if res := f.importBatch(t, importer.Batch{Source: b.Source, Contents: []importer.ContentRecord{gallery(3, "AB12", "ab12")}}); len(res.Failed) != 1 || res.Failed[0].Code != "invalid" {
		t.Fatalf("a shared code within one item %+v", res)
	}
	if !slices.Equal(codes(1), one) {
		t.Fatalf("refused imports changed codes: %v", codes(1))
	}
}

// The route admits root:import:write holders only, refuses a malformed
// batch as a whole, and answers the result.
func TestImportRoute(t *testing.T) {
	f := newEngine(t)
	post := func(actor string, body any) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(body)
		req := testauth.As(httptest.NewRequest(http.MethodPost, "/api/v1/import", &buf), access.Actor{ID: actor})
		rec := httptest.NewRecorder()
		f.routes.ServeHTTP(rec, req)
		return rec
	}
	b := importer.Batch{Source: "test-legacy", Entities: []importer.EntityRecord{{ExternalID: "t", Kind: "tag", Slug: "t"}}}
	if rec := post("staff", b); rec.Code != http.StatusForbidden {
		t.Fatalf("staff without the permission: %d %s", rec.Code, rec.Body)
	}
	if rec := post("importer", importer.Batch{Source: "Bad Source"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad source: %d", rec.Code)
	}
	if rec := post("importer", map[string]any{"source": "x", "galleries": []int{1}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown section: %d", rec.Code)
	}
	if rec := post("importer", map[string]any{"source": "x", "contents": []any{map[string]any{"external_id": "g", "kind": "gallery", "view_baseline": 100}}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("retired counter accepted: %d", rec.Code)
	}
	rec := post("importer", b)
	var res importer.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || rec.Code != http.StatusOK || res.Written["entities"] != 1 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	ids := func(actor string, q any) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(q)
		req := testauth.As(httptest.NewRequest(http.MethodPost, "/api/v1/import/external-ids", &buf), access.Actor{ID: actor})
		rec := httptest.NewRecorder()
		f.routes.ServeHTTP(rec, req)
		return rec
	}
	q := importer.ExternalIDsQuery{Source: "test-legacy", Lists: []importer.ExternalIDList{{Entity: "group"}}}
	if rec := ids("staff", q); rec.Code != http.StatusForbidden {
		t.Fatalf("external ids, staff without the permission: %d", rec.Code)
	}
	var got importer.ExternalIDs
	if rec := ids("importer", q); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil ||
		len(got.Lists) != 1 || len(got.Lists[0].IDs) != 1 || got.Lists[0].IDs[0].ExternalID != "t" || got.Lists[0].IDs[0].ID != res.IDs["group"]["t"] {
		t.Fatalf("external ids: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []importer.ExternalIDsQuery{
		{Source: "test-legacy"},
		{Source: "test-legacy", Lists: []importer.ExternalIDList{{Entity: "gallery"}}},
		{Source: "test-legacy", Lists: []importer.ExternalIDList{{Entity: "group"}, {Entity: "group"}}},
		{Source: "test-legacy", Lists: []importer.ExternalIDList{{Entity: "group", Cursor: "%%"}}},
		{Source: "test-legacy", Lists: []importer.ExternalIDList{{Entity: "group", Limit: 10_001}}},
	} {
		if rec := ids("importer", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("external ids %+v: %d", bad, rec.Code)
		}
	}
}

// Imported interaction totals and entity counts/covers are available before
// workers start. Only keyword documents require the search worker.
func TestImportRebuildsProjections(t *testing.T) {
	f := newEngine(t)
	if res := f.importBatch(t, catalogBatch()); len(res.Failed) > 0 {
		t.Fatalf("failures %+v", res.Failed)
	}
	g1 := id("gallery", 1)
	if got := f.scalar(t, `SELECT likes || '/' || favorites FROM $s.content_interaction_counts WHERE content_kind='gallery' AND content_id=$1 AND content_version_id IS NULL`, g1); got != "2/1" {
		t.Fatalf("immediate imported counts: %q, want 2/1", got)
	}
	for _, kind := range []string{"tag", "artist"} {
		rec := httptest.NewRecorder()
		f.routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/entities/"+id(kind, 1), nil))
		var entity entities.EntityDetail
		if err := json.Unmarshal(rec.Body.Bytes(), &entity); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("immediate %s: %d %s: %v", kind, rec.Code, rec.Body, err)
		}
		if entity.Cover == nil || !entity.Cover.Auto || entity.Cover.ContentID != g1 {
			t.Fatalf("immediate %s cover: %+v", kind, entity.Cover)
		}
		found := false
		for _, count := range entity.Counts {
			if count.Language == "" && count.Count == 1 {
				found = true
			}
		}
		if !found {
			t.Fatalf("immediate %s counts: %+v", kind, entity.Counts)
		}
	}
	if err := f.e.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for _, c := range []struct{ name, sql, want string }{
		{"search documents", `SELECT (count(DISTINCT content_id) = 2 AND count(*) >= 3)::text FROM $s.content_search_documents WHERE content_id IN ($1, '` + id("tag", 1) + `')`, "true"},
	} {
		for {
			if got := f.scalar(t, c.sql, g1); got == c.want {
				break
			} else if time.Now().After(deadline) {
				t.Fatalf("%s: %q, want %q", c.name, got, c.want)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !slices.Contains([]string{"0", "1", "2"}, f.scalar(t, `SELECT count(*)::text FROM $s.content_search_dirty WHERE content_id = $1`, g1)) {
		t.Error("search marks pile up")
	}
}

// A entity of an auth_scope kind keeps the AuthKit scope it names; one naming
// none is provisioned only when the batch asks. Avatars and owners import.
func TestImportEntityScopes(t *testing.T) {
	f := newEngine(t)
	avatar := "i-" + uuid.NewString()
	channel := func(n int, scope string) importer.EntityRecord {
		return importer.EntityRecord{ExternalID: "channel:" + string(rune('0'+n)), ID: id("channel", int64(n)), Kind: "channel", Slug: "c" + string(rune('0'+n)),
			OwnerUserID: "u-1", Avatar: avatar, AuthScope: scope}
	}
	res := f.importBatch(t, importer.Batch{Source: "test-legacy", Entities: []importer.EntityRecord{channel(1, "scope-1"), channel(2, "")}})
	if res.Written["entities"] != 1 || len(res.Failed) != 1 || res.Failed[0].ExternalID != "channel:2" || res.Failed[0].Code != "invalid" {
		t.Fatalf("result %+v", res)
	}
	if got := f.scalar(t, `SELECT owner_user_id || ' ' || avatar || ' ' || auth_scope FROM $s.content_entities WHERE id = $1`, id("channel", 1)); got != "u-1 "+avatar+" scope-1" {
		t.Fatalf("channel 1: %q", got)
	}
	// Re-imported without its scope, an entity keeps it.
	again := channel(1, "")
	if res := f.importBatch(t, importer.Batch{Source: "test-legacy", Entities: []importer.EntityRecord{again}}); len(res.Failed) > 0 {
		t.Fatalf("re-import %+v", res.Failed)
	}
	if got := f.scalar(t, `SELECT auth_scope FROM $s.content_entities WHERE id = $1`, id("channel", 1)); got != "scope-1" {
		t.Fatalf("scope after re-import: %q", got)
	}
	res = f.importBatch(t, importer.Batch{Source: "test-legacy", ProvisionScopes: true, Entities: []importer.EntityRecord{channel(2, "")}})
	if len(res.Failed) > 0 || res.Written["entities"] != 1 {
		t.Fatalf("provisioned import %+v", res)
	}
}

// Media is not imported: an importer fills an imported version's folder
// through the upload routes and sets its reader metadata, here as the host's
// in-process client.
func TestImportThenIngestMedia(t *testing.T) {
	env := mediatest.Open(t)
	var registry *media.Registry
	f := newEngineWith(t, func(cfg *contentkit.Config, deps *contentkit.Deps) {
		cfg.Tenant = env.Tenant
		pngs := []string{"image/png"}
		cfg.Media = &contentkit.MediaConfig{
			Config: media.Config{BaseURL: "https://media.test", Kinds: []media.Kind{{Name: "gallery", KeepOriginals: true,
				Publish: &media.Publish{Pages: "originals/{name}", Renditions: []string{"low"}, Playback: "listen"},
				Uploads: []media.Upload{{Path: "originals/{name}", Types: pngs, MaxBytes: 10 << 20},
					{Path: "audio/{name}", Types: []string{"audio/mpeg"}, MaxBytes: 10 << 20}},
				Private: []media.Private{{Name: "low", From: "originals/{name}", To: "low/{name}.webp", Image: &media.Image{Width: 64, Quality: 80}},
					{Name: "listen", From: "audio/{name}", To: "listen/{name}/", Audio: &media.Audio{}}}}}},
			Delivery:    contentkit.DeliveryConfig{SigningKey: "k1:" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("s"), 32))},
			CommitLimit: contentkit.RateLimitConfig{Disabled: true}, ReadLimit: contentkit.RateLimitConfig{Disabled: true}}
		deps.Store = env.Store
		model, err := cfg.MediaKinds()
		if err != nil {
			t.Fatal(err)
		}
		registry, err = media.NewRegistry(model)
		if err != nil {
			t.Fatal(err)
		}
	})
	if err := f.e.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	b := catalogBatch()
	res := f.importBatch(t, importer.Batch{Source: b.Source, Entities: b.Entities, Contents: b.Contents[:1]})
	if len(res.Failed) > 0 {
		t.Fatalf("import %+v", res.Failed)
	}
	firstPage, secondPage := png(t, 8), png(t, 9)
	pages := []contentkit.MediaFile{{Path: "originals/0001.png", Type: "image/png", Body: bytes.NewReader(firstPage)},
		{Path: "originals/0002.png", Type: "image/png", Body: bytes.NewReader(secondPage)}}
	ref := media.RefBody{Kind: "gallery", ID: id("gallery", 1), Version: id("version", 1)}
	op := uuid.NewString()
	out, err := f.e.Client().IngestMedia(t.Context(), nil, ref, op, pages)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Files) != 2 || out.Files[0].Path != "originals/0001.png" || out.Files[1].Path != "originals/0002.png" {
		t.Fatalf("committed %+v", out.Files)
	}
	// Sending them again finds the folder's bytes and keeps two pages.
	for _, p := range pages {
		_, _ = p.Body.Seek(0, 0)
	}
	if again, err := f.e.Client().IngestMedia(t.Context(), nil, ref, uuid.NewString(), pages); err != nil || len(again.Files) != 2 {
		t.Fatalf("again %+v %v", again.Files, err)
	}
	// A file already in the bucket, outside ContentKit's namespaces, is
	// copied server-side; one inside them is refused.
	src, body := "legacy-"+env.Tenant+"/0003.png", png(t, 10)
	if _, err := env.Store.Put(t.Context(), src, bytes.NewReader(body), int64(len(body)), media.PutOptions{ContentType: "application/octet-stream"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Store.Delete(context.Background(), src) })
	copied, err := f.e.Client().IngestMedia(t.Context(), nil, ref, uuid.NewString(),
		[]contentkit.MediaFile{{Path: "originals/0003.png", Type: "image/png", From: src, Size: int64(len(body))}})
	if err != nil || len(copied.Files) != 3 || copied.Files[2].Path != "originals/0003.png" {
		t.Fatalf("copied %+v %v", copied.Files, err)
	}
	if _, err := env.Store.Head(t.Context(), src); err != nil {
		t.Fatalf("the source went: %v", err)
	}
	if _, err := f.e.Client().IngestMedia(t.Context(), nil, ref, uuid.NewString(),
		[]contentkit.MediaFile{{Path: "originals/0004.png", Type: "image/png", From: env.Tenant + "/gallery/" + id("gallery", 1) + "/x.png"}}); err == nil {
		t.Fatal("a source inside ContentKit's namespace was copied")
	}
	// A page's audio plays when it opens: the importer sets the cue, and
	// setting it again is a no-op; a cue naming audio the version lacks is
	// refused.
	if _, err := f.e.Client().IngestMedia(t.Context(), nil, ref, uuid.NewString(),
		[]contentkit.MediaFile{{Path: "audio/0001.mp3", Type: "audio/mpeg", Body: bytes.NewReader([]byte("ID3 page one"))}}); err != nil {
		t.Fatal(err)
	}
	// The upload commit is intentionally asynchronous. This fixture runs no
	// image/audio worker; perform only its placement step so the source hashes
	// are verified before authoring metadata that refers to them.
	queue, err := workqueue.New(f.pool, registry, f.schema+"_media_worker")
	if err != nil {
		t.Fatal(err)
	}
	journal, err := media.NewPGJournal(f.pool, f.schema, queue)
	if err != nil {
		t.Fatal(err)
	}
	manifests, err := media.NewManifests(env.Store, registry, media.ManifestOptions{Locker: media.PGLocker(f.pool), Journal: journal})
	if err != nil {
		t.Fatal(err)
	}
	versionRef, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
	if err != nil {
		t.Fatal(err)
	}
	if placed, err := manifests.Place(t.Context(), versionRef); err != nil || placed != 4 {
		t.Fatalf("place three pages and their audio: %d %v", placed, err)
	}
	cue := versions.ReaderMeta{SchemaVersion: 1, Foreground: []versions.ForegroundCue{{File: "0001", Page: ptr(fmt.Sprintf("%x", sha256.Sum256(firstPage)))}}}
	for range 2 {
		if err := f.e.Client().PutContentVersionReader(t.Context(), ref.ID, ref.Version, cue); err != nil {
			t.Fatalf("set reader metadata: %v", err)
		}
	}
	cue.Foreground = append(cue.Foreground, versions.ForegroundCue{File: "0002", Page: ptr(fmt.Sprintf("%x", sha256.Sum256(secondPage)))})
	var ce *contentkit.Error
	if err := f.e.Client().PutContentVersionReader(t.Context(), ref.ID, ref.Version, cue); !errors.As(err, &ce) || ce.Status != http.StatusBadRequest {
		t.Fatalf("a cue on missing audio: %v", err)
	}
}

// A legacy record's one cover is ingested into each of its versions' folders
// and each version record selects its copy: covers live in versions.
func TestImportCoversPerVersion(t *testing.T) {
	env := mediatest.Open(t)
	f := newEngineWith(t, func(cfg *contentkit.Config, deps *contentkit.Deps) {
		cfg.Tenant = env.Tenant
		pngs := []string{"image/png"}
		cfg.Media = &contentkit.MediaConfig{
			Config: media.Config{BaseURL: "https://media.test", Kinds: []media.Kind{{Name: "gallery", KeepOriginals: true,
				Uploads: []media.Upload{{Path: "covers/{name}", Types: pngs, MaxBytes: 10 << 20, Named: true, Max: 5},
					{Path: "originals/{name}", Types: pngs, MaxBytes: 10 << 20}},
				Public: []media.Public{{Name: "cover", From: "covers/{name}", To: "cover-{name}-{w}.webp", Widths: []int{64}}}}}},
			Delivery:    contentkit.DeliveryConfig{SigningKey: "k1:" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("s"), 32))},
			CommitLimit: contentkit.RateLimitConfig{Disabled: true}, ReadLimit: contentkit.RateLimitConfig{Disabled: true}}
		deps.Store = env.Store
	})
	if err := f.e.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	b := catalogBatch()
	record := b.Contents[0]
	if res := f.importBatch(t, importer.Batch{Source: b.Source, Entities: b.Entities, Contents: []importer.ContentRecord{record}}); len(res.Failed) > 0 {
		t.Fatalf("import %+v", res.Failed)
	}
	legacy := png(t, 12)
	covers := map[string]string{}
	for i := range record.Versions {
		v := &record.Versions[i]
		out, err := f.e.Client().IngestMedia(t.Context(), nil, media.RefBody{Kind: "gallery", ID: record.ID, Version: v.ID}, uuid.NewString(),
			[]contentkit.MediaFile{{Path: "covers/legacy.png", Type: "image/png", Body: bytes.NewReader(legacy)}})
		if err != nil || len(out.Files) != 1 || !strings.HasPrefix(out.Files[0].Path, "covers/i-") {
			t.Fatalf("version %s cover %+v %v", v.ID, out.Files, err)
		}
		v.Cover = strings.TrimSuffix(strings.TrimPrefix(out.Files[0].Path, "covers/"), ".png")
		covers[v.ID] = v.Cover
	}
	if res := f.importBatch(t, importer.Batch{Source: b.Source, Contents: []importer.ContentRecord{record}}); len(res.Failed) > 0 {
		t.Fatalf("re-import %+v", res.Failed)
	}
	c, err := f.e.Client().GetContent(t.Context(), record.ID, contentkit.GetContentQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Versions) != 2 || covers[c.Versions[0].ID] == covers[c.Versions[1].ID] {
		t.Fatalf("versions %+v, covers %v", c.Versions, covers)
	}
	for _, v := range c.Versions {
		if v.Cover == nil || v.Cover.Image != covers[v.ID] {
			t.Fatalf("version %s cover %+v, want %s", v.ID, v.Cover, covers[v.ID])
		}
	}

	// A re-sync that sends neither covers nor segments keeps both.
	for i := range record.Versions {
		record.Versions[i].Cover, record.Versions[i].Segments = "", nil
	}
	if res := f.importBatch(t, importer.Batch{Source: b.Source, Contents: []importer.ContentRecord{record}}); len(res.Failed) > 0 {
		t.Fatalf("re-sync %+v", res.Failed)
	}
	if c, err = f.e.Client().GetContent(t.Context(), record.ID, contentkit.GetContentQuery{}); err != nil {
		t.Fatal(err)
	}
	for _, v := range c.Versions {
		if v.Cover == nil || v.Cover.Image != covers[v.ID] {
			t.Fatalf("re-synced version %s cover %+v, want %s", v.ID, v.Cover, covers[v.ID])
		}
	}
}

func png(t *testing.T, w int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, w))
	img.Set(0, 0, color.RGBA{R: 200, A: 255})
	var b bytes.Buffer
	if err := pngenc.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Importing old bookmarks preserves resume/history without inventing measured
// readers or completions. Explicit clicks remain independently replayable.
func TestImportReadingBookmarksAndExplicitClicks(t *testing.T) {
	env := signaltest.FromEnv(t)
	chDB := "ck_import_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	env.Empty(t, chDB)
	admin := env.Open(t, "")
	f := newEngineWith(t, func(cfg *contentkit.Config, _ *contentkit.Deps) {
		cfg.ClickHouse = contentkit.ClickHouseConfig{Database: chDB, Addr: env.Addr, Username: env.User, Password: env.Password, Cluster: env.Cluster}
	})
	t.Cleanup(func() {
		env.Drop(t, admin, chDB)
		_, _ = f.pool.Exec(context.Background(), "DELETE FROM public.migrations WHERE app = 'contentkit_signal' AND database LIKE '%' || $1", chDB)
	})
	b := catalogBatch()

	for range 2 {
		res := f.importBatch(t, b)
		if len(res.Failed) > 0 || res.Written["signals"] != 2 || res.Written["legacy_reading"] != 2 || res.Skipped["signals"] != 0 || res.Skipped["legacy_reading"] != 0 {
			t.Fatalf("result %+v", res)
		}
	}
	conn := env.Open(t, chDB)
	var n uint64
	if err := conn.QueryRow(t.Context(), `SELECT count() FROM signals FINAL WHERE tenant = ? AND event_id IN ('click:1', 'click:2')`, tenant).Scan(&n); err != nil || n != 2 {
		t.Fatalf("explicit clicks duplicated on replay: %d %v", n, err)
	}
	if err := conn.QueryRow(t.Context(), `SELECT count() FROM engagement_segments WHERE tenant = ?`, tenant).Scan(&n); err != nil || n != 0 {
		t.Fatalf("bookmarks fabricated measured observations: %d %v", n, err)
	}
	store, err := signal.NewStore(conn, chDB)
	if err != nil {
		t.Fatal(err)
	}
	work := contentref.New(tenant, "gallery", id("gallery", 1))
	refs := []contentref.ContentRef{work, work.WithVersion(id("version", 1))}
	user := signal.Subject{UserID: "u-1"}
	standing := func(want uint32, resume string) {
		t.Helper()
		states, err := store.States(t.Context(), tenant, user, refs)
		if err != nil || len(states) != 2 {
			t.Fatalf("work/version legacy standing: %+v %v", states, err)
		}
		for _, ref := range refs {
			s := states[ref.Key()]
			if !s.Seen || s.MaxProgress != want || s.ProgressMax != 20 || s.Resume != resume || s.ActiveMS != 0 || s.Quality != 0 || s.Completed {
				t.Fatalf("legacy bookmark was changed into measured reading: %+v", s)
			}
		}
		history, err := store.History(t.Context(), tenant, user, signal.HistoryOptions{Status: signal.HistorySeen})
		if err != nil || len(history) != 1 || history[0].ContentID != work.ContentID || history[0].Resume != resume {
			t.Fatalf("version bookmark absent from work history: %+v %v", history, err)
		}
	}
	standing(5, "005")
	anon := signal.Subject{AnonKey: "a-1"}
	if states, err := store.States(t.Context(), tenant, anon, []contentref.ContentRef{work}); err != nil || len(states) != 1 || !states[work.Key()].Completed || states[work.Key()].Resume != "020" || states[work.Key()].ActiveMS != 0 || states[work.Key()].Quality != 0 {
		t.Fatalf("legacy completion/resume was lost or promoted to measured quality: %+v %v", states, err)
	}
	for _, subject := range []signal.Subject{user, anon} {
		if n, err := store.CompletedCount(t.Context(), tenant, subject, "gallery"); err != nil || n != 0 {
			t.Fatalf("legacy completion counted as a measured account stat: %d %v", n, err)
		}
	}
	if days, err := store.Activity(t.Context(), tenant, "gallery", signal.Window{}); err != nil || len(days) != 0 {
		t.Fatalf("bookmarks/clicks fabricated daily readers: %+v %v", days, err)
	}
	// A later bookmark replaces the resume copy; retrying its older revision
	// must not move either the version or work history backward.
	newer := b.LegacyReading[0]
	newer.LastReadAt, newer.Progress, newer.Resume, newer.Revision = t0.Add(2*time.Hour), 8, "008", 2
	for _, row := range []importer.LegacyReadingRecord{newer, b.LegacyReading[0]} {
		res := f.importBatch(t, importer.Batch{Source: b.Source, LegacyReading: []importer.LegacyReadingRecord{row}})
		if len(res.Failed) > 0 || res.Written["legacy_reading"] != 1 {
			t.Fatalf("bookmark checkpoint: %+v", res)
		}
	}
	standing(8, "008")
	if report, err := store.EraseSubjects(t.Context(), []string{tenant}, []signal.Subject{user}); err != nil || !report.Complete() {
		t.Fatalf("erase bookmark: %+v %v", report, err)
	}
	if res := f.importBatch(t, importer.Batch{Source: b.Source, LegacyReading: []importer.LegacyReadingRecord{newer}}); len(res.Failed) > 0 {
		t.Fatalf("erased replay: %+v", res)
	}
	if states, err := store.States(t.Context(), tenant, user, refs); err != nil || len(states) != 0 {
		t.Fatalf("import replay resurrected an erased bookmark: %+v %v", states, err)
	}
	retired := b.Signals[0]
	retired.Type = "view"
	if res := f.importBatch(t, importer.Batch{Source: b.Source, Signals: []importer.SignalRecord{retired}}); len(res.Failed) != 1 || res.Failed[0].Code != importer.CodeInvalid {
		t.Fatalf("retired bare opens still import: %+v", res)
	}
}

// Records gone from the source are deleted by external id, as their delete
// routes would, never by absence; re-sending the deletes changes nothing.
func TestImportDeletes(t *testing.T) {
	f := newEngine(t)
	if res := f.importBatch(t, catalogBatch()); len(res.Failed) > 0 {
		t.Fatalf("failures: %+v", res.Failed)
	}
	if res := f.importBatch(t, importer.Batch{Source: "test-legacy", Contents: catalogBatch().Contents[:1]}); f.scalar(t,
		`SELECT state FROM $s.content_items WHERE id = $1`, id("gallery", 3)) != "published" || len(res.Failed) > 0 {
		t.Fatalf("a batch without a record deletes nothing: %+v", res)
	}
	del := func(entity, external string) importer.DeleteRecord {
		return importer.DeleteRecord{Entity: entity, ExternalID: external}
	}
	b := importer.Batch{Source: "test-legacy", Deletes: []importer.DeleteRecord{
		del("content", "gallery:3"), del("version", "version:2"), del("group", "tag:2"), del("comment", "comment:2"), del("poll", "poll:2"),
		del("content", "gallery:2"), del("content", "gallery:99"), del("version", "version:99"), del("group", "tag:99"),
		del("comment", "comment:99"), del("poll", "poll:99"), del("poll_option", "answer:1"), del("content", ""),
	}}
	res := f.importBatch(t, b)
	if res.Written["deletes"] != 11 || len(res.Failed) != 2 || res.Failed[0].ExternalID != "answer:1" || res.Failed[0].Code != importer.CodeInvalid ||
		res.Failed[1].Code != importer.CodeInvalid {
		t.Fatalf("deletes: %+v", res)
	}
	g1 := id("gallery", 1)
	for _, c := range [][2]string{
		{`SELECT state || ' ' || (deleted_at IS NOT NULL) FROM $s.content_items WHERE id = '` + id("gallery", 3) + `' AND $1 <> ''`, "deleted true"},
		{`SELECT bool_and(deleted)::text FROM $s.content_facets WHERE content_id = '` + id("gallery", 3) + `' AND $1 <> ''`, "true"},
		{`SELECT state FROM $s.content_versions WHERE id = '` + id("version", 2) + `' AND $1 <> ''`, "deleted"},
		{`SELECT state FROM $s.content_items WHERE id = $1`, "published"},
		{`SELECT state FROM $s.content_entities WHERE id = (SELECT id FROM $s.content_external_ids WHERE external_id = 'tag:2') AND $1 <> ''`, "deleted"},
		{`SELECT array_length(entity_ids, 1)::text FROM $s.content_facets WHERE content_id = $1 AND version_id = '` + id("version", 1) + `'`, "5"},
		{`SELECT (deleted_at IS NOT NULL)::text FROM $s.content_comments WHERE id = '` + id("comment", 2) + `'::uuid AND $1 <> ''`, "true"},
		{`SELECT reply_count::text FROM $s.content_comments WHERE id = '` + id("comment", 1) + `'::uuid AND $1 <> ''`, "1"},
		{`SELECT (deleted_at IS NOT NULL)::text FROM $s.content_poll_questions WHERE question = 'Why?' AND $1 <> ''`, "true"},
		{`SELECT (deleted_at IS NULL)::text FROM $s.content_poll_questions WHERE question = 'Best?' AND $1 <> ''`, "true"},
		{`SELECT count(*)::text FROM $s.content_events WHERE type = 'content.deleted' AND entity_id = '` + id("gallery", 3) + `' AND $1 <> ''`, "1"},
	} {
		if got := f.scalar(t, c[0], g1); got != c[1] {
			t.Errorf("%s\n got %q\nwant %q", c[0], got, c[1])
		}
	}
	before := f.fingerprint(t)
	if again := f.importBatch(t, b); again.Written["deletes"] != 11 || len(again.Failed) != 2 {
		t.Fatalf("re-sent deletes: %+v", again)
	}
	after := f.fingerprint(t)
	for table, fp := range before {
		if after[table] != fp {
			t.Errorf("re-sent deletes changed %s", table)
		}
	}
	// A canonical ID is still interpreted only as an external ID, never as
	// permission to delete that record directly.
	if res := f.importBatch(t, importer.Batch{Source: "test-legacy", Deletes: []importer.DeleteRecord{del("version", id("version", 1))}}); len(res.Failed) != 0 || res.Written["deletes"] != 1 {
		t.Errorf("unmapped external id: %+v", res)
	}
	if got := f.scalar(t, `SELECT state FROM $s.content_versions WHERE id = $1`, id("version", 1)); got != "published" {
		t.Fatalf("unmapped external id deleted a canonical id: %s", got)
	}
	if res := f.importBatch(t, importer.Batch{Source: "another-source", Deletes: []importer.DeleteRecord{del("content", "gallery:1")}}); len(res.Failed) != 0 || res.Written["deletes"] != 1 {
		t.Fatalf("another source's absent mapping: %+v", res)
	}
	if got := f.scalar(t, `SELECT state FROM $s.content_items WHERE id = $1`, g1); got != "published" {
		t.Fatalf("another source deleted the content: %s", got)
	}
	other := newEngineWith(t, func(cfg *contentkit.Config, deps *contentkit.Deps) {
		cfg.Tenant = "other-site"
		cfg.Database.Schema = f.schema
		deps.Postgres = f.pool
	})
	if res := other.importBatch(t, importer.Batch{Source: "test-legacy", Deletes: []importer.DeleteRecord{del("content", "gallery:1")}}); len(res.Failed) != 0 || res.Written["deletes"] != 1 {
		t.Fatalf("another tenant's absent mapping: %+v", res)
	}
	if got := f.scalar(t, `SELECT state FROM $s.content_items WHERE id = $1`, g1); got != "published" {
		t.Fatalf("another tenant deleted the content: %s", got)
	}
	// Absence is special only for deletes, not references in other records.
	if res := f.importBatch(t, importer.Batch{Source: "test-legacy", Comments: []importer.CommentRecord{{
		ExternalID: "missing-target", Target: importer.TargetRef{Content: &importer.Ref{ExternalID: "gallery:99"}}, UserID: "u-1", Body: "hello",
	}}}); len(res.Failed) != 1 || res.Failed[0].Code != importer.CodeNotFound {
		t.Fatalf("ordinary missing reference: %+v", res)
	}
}

// A persistent sync pages the external ids its imports hold, per entity, and
// sends deletes for those its source lost; deleted ones say so.
func TestImportExternalIDs(t *testing.T) {
	f := newEngine(t)
	if res := f.importBatch(t, catalogBatch()); len(res.Failed) > 0 {
		t.Fatalf("failures: %+v", res.Failed)
	}
	list := func(lists ...importer.ExternalIDList) importer.ExternalIDs {
		t.Helper()
		out, err := f.e.Client().PostImportExternalIds(t.Context(), importer.ExternalIDsQuery{Source: "test-legacy", Lists: lists})
		if err != nil || len(out.Lists) != len(lists) {
			t.Fatalf("external ids %+v: %+v %v", lists, out, err)
		}
		return out
	}
	held := func(p importer.ExternalIDPage) []string {
		out := []string{}
		for _, x := range p.IDs {
			out = append(out, fmt.Sprintf("%s=%t", x.ExternalID, x.Deleted))
		}
		return out
	}
	// The source lost gallery:3; gallery:2 was imported deleted.
	source := map[string]bool{"gallery:1": true, "gallery:2": true, "post:1": true}
	var contents []string
	var deletes []importer.DeleteRecord
	got := list(importer.ExternalIDList{Entity: "content", Limit: 3}, importer.ExternalIDList{Entity: "comment"})
	for page := got.Lists[0]; ; {
		for _, x := range page.IDs {
			contents = append(contents, x.ExternalID)
			if x.ID != f.scalar(t, `SELECT id FROM $s.content_external_ids WHERE entity = 'content' AND external_id = $1`, x.ExternalID) {
				t.Fatalf("%s maps to %s", x.ExternalID, x.ID)
			}
			if !source[x.ExternalID] && !x.Deleted {
				deletes = append(deletes, importer.DeleteRecord{Entity: importer.RecordContent, ExternalID: x.ExternalID})
			}
		}
		if page.NextCursor == "" {
			break
		}
		page = list(importer.ExternalIDList{Entity: "content", Limit: 3, Cursor: page.NextCursor}).Lists[0]
	}
	if want := []string{"gallery:1", "gallery:2", "gallery:3", "post:1"}; !reflect.DeepEqual(contents, want) {
		t.Fatalf("contents paged %v, want %v", contents, want)
	}
	if want := []string{"comment:1=false", "comment:2=false", "comment:3=true", "comment:4=false"}; !reflect.DeepEqual(held(got.Lists[1]), want) {
		t.Fatalf("comments %v, want %v", held(got.Lists[1]), want)
	}
	if res := f.importBatch(t, importer.Batch{Source: "test-legacy", Deletes: deletes}); res.Written["deletes"] != 1 || len(res.Failed) > 0 {
		t.Fatalf("deletes %+v: %+v", deletes, res)
	}
	if want := []string{"gallery:1=false", "gallery:2=true", "gallery:3=true", "post:1=false"}; !reflect.DeepEqual(held(list(importer.ExternalIDList{Entity: "content"}).Lists[0]), want) {
		t.Fatalf("after the sync's deletes: %v, want %v", held(list(importer.ExternalIDList{Entity: "content"}).Lists[0]), want)
	}
	other, err := f.e.Client().PostImportExternalIds(t.Context(), importer.ExternalIDsQuery{Source: "other-legacy", Lists: []importer.ExternalIDList{{Entity: "poll_option"}}})
	if err != nil || len(other.Lists) != 1 || len(other.Lists[0].IDs) != 0 {
		t.Fatalf("another source's ids: %+v %v", other, err)
	}
}

// An import deleting content and an entity, racing a reparent of an entity the
// content is in, takes the entity hierarchy lock before the content row, as
// the reparent does, so neither deadlocks.
func TestImportDeletesRacingReparent(t *testing.T) {
	f := newEngine(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	b := catalogBatch()
	b.Entities = append(b.Entities, importer.EntityRecord{ExternalID: "tag:9", Kind: "tag", Slug: "unrelated"},
		importer.EntityRecord{ExternalID: "series:6", ID: id("series", 6), Kind: "series", Slug: "orphaned"},
		importer.EntityRecord{ExternalID: "series:7", Kind: "series", Slug: "orphan", Parent: &importer.Ref{ExternalID: "series:6"}})
	if res := f.importBatch(t, b); len(res.Failed) > 0 {
		t.Fatalf("failures: %+v", res.Failed)
	}
	q := func(sql string) string {
		return strings.ReplaceAll(sql, "$s.", pgx.Identifier{f.schema}.Sanitize()+".")
	}
	// series:7's parent is deleted under it, so deleting series:7 fails at
	// write time and the chunk retries record by record.
	if _, err := f.pool.Exec(ctx, q(`UPDATE $s.content_entities SET state = 'deleted' WHERE id = $1`), id("series", 6)); err != nil {
		t.Fatal(err)
	}
	blockedBy := func(pid uint32) uint32 {
		t.Helper()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			var waiter uint32
			err := f.pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, pid).Scan(&waiter)
			if err == nil {
				return waiter
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
				t.Fatalf("nothing blocked on backend %d", pid)
			case <-tick.C:
			}
		}
	}
	// hold pauses the import inside its content delete, after it has the
	// content row, so the reparent reaches that row while the import waits.
	hold, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, q(`SELECT 1 FROM $s.content_facets WHERE content_id = $1 FOR UPDATE`), id("gallery", 1)); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		res importer.Result
		err error
	}
	imported := make(chan outcome, 1)
	go func() {
		res, err := f.e.Client().PostImport(ctx, importer.Batch{Source: "test-legacy", Deletes: []importer.DeleteRecord{
			{Entity: "content", ExternalID: "gallery:1"}, {Entity: "group", ExternalID: "tag:9"}, {Entity: "group", ExternalID: "series:7"},
			{Entity: "group", ExternalID: "never-imported"}}})
		imported <- outcome{res, err}
	}()
	importPID := blockedBy(hold.Conn().PgConn().PID())
	reparented := make(chan error, 1)
	go func() {
		_, err := f.e.Client().PatchEntity(ctx, id("series", 2), entities.EntityUpdate{ParentID: ptr(entities.EntityID(""))})
		reparented <- err
	}()
	blockedBy(importPID)
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-reparented; err != nil {
		t.Errorf("reparent: %v", err)
	}
	if o := <-imported; o.err != nil || o.res.Written["deletes"] != 3 || len(o.res.Failed) != 1 || o.res.Failed[0].Index != 2 {
		t.Errorf("import: %+v %v", o.res, o.err)
	}
}
