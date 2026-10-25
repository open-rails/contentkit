package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// Exercise the actual deployed0.126.1 lineage, not a fresh current schema with
// old-looking rows. Required retirement and dated-association decisions stay explicit.
func TestReleasedEntityJobUpgrade(t *testing.T) {
	w := newWorld(t)
	schema, tenant := w.schema+"_entity_previous", "previous-entity-jobs"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for _, suffix := range []string{"_river", "_auth", ""} {
			_, err := w.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema + suffix}.Sanitize()+" CASCADE")
			must(t, err)
		}
	})
	oldCall := func(action string, input []byte) []byte {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "go", "run", "-p=1", ".")
		cmd.Dir = "previous-release/entity-jobs"
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOMAXPROCS=1", "CONTENTKIT_PREVIOUS_ENTITY_SCHEMA="+schema, "CONTENTKIT_PREVIOUS_ENTITY_ACTION="+action)
		cmd.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("released entity initializer %s: %v\n%s", action, err, stderr.String())
		}
		return out
	}
	raw := oldCall("seed", nil)
	var old struct {
		Version                         string
		Contents                        []catalog.Content
		Entities                        []entities.Entity
		Projection, Foreign, Historical int64
	}
	must(t, json.Unmarshal(raw, &old))
	if old.Version != "v0.126.1" || len(old.Contents) != 3 || len(old.Entities) != 3 {
		t.Fatalf("fixture did not execute the pinned deployed release: %+v", old)
	}
	roles := authkit.NewRoles()
	roles.Root.Declare(access.Names(access.Permissions()...)...)
	auth, _ := authtest.New(t, authtest.WithConfig(func(c *authkit.Config) { c.Roles = roles }), authtest.WithDeps(func(d *authkit.Deps) { d.Postgres = w.pool }))
	cfg := ck.Config{Tenant: tenant, Database: ck.DatabaseConfig{Schema: schema, SkipMigrations: true}, Content: kinds.Config{
		Languages: []string{"en"}, EntityKinds: map[string]kinds.EntityKind{"tag": {Route: "tags"}, "rail": {Route: "rails"}},
		Kinds: map[string]kinds.Kind{"article": {Route: "articles", Presentation: "post", Entities: []string{"tag", "rail"}, Versions: kinds.VersionRules{Languages: "one", Hidden: true}}},
	}}
	table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	jobs := pgx.Identifier{schema + "_river", "river_job"}.Sanitize()
	snapshot := func(query string, args ...any) string {
		t.Helper()
		var got string
		must(t, w.pool.QueryRow(t.Context(), query, args...).Scan(&got))
		return got
	}
	ledger := func() string {
		return snapshot(`SELECT coalesce(jsonb_agg(to_jsonb(m) ORDER BY sequence),'[]'::jsonb)::text FROM public.migrations m WHERE app='contentkit' AND schema=$1`, schema)
	}
	oldLedger := ledger()
	stored := snapshot("SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY entity_id,language),'[]'::jsonb)::text FROM " + table("content_entity_counts") + " c")
	if stored == "[]" {
		t.Fatal("previous release never populated its stored counts")
	}
	associations := snapshot("SELECT jsonb_agg(to_jsonb(m) ORDER BY entity_id,content_id)::text FROM " + table("content_memberships") + " m")
	job := func(id int64) string { return snapshot("SELECT to_jsonb(j)::text FROM "+jobs+" j WHERE id=$1", id) }
	foreign, historical := job(old.Foreign), job(old.Historical)
	assertOldShape := func() {
		t.Helper()
		var columns int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name='content_memberships' AND column_name IN ('active_from','active_until')`, schema).Scan(&columns))
		if columns != 2 || snapshot("SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY entity_id,language),'[]'::jsonb)::text FROM "+table("content_entity_counts")+" c") != stored {
			t.Fatal("refused upgrade changed old schema or counts")
		}
		if snapshot("SELECT jsonb_agg(to_jsonb(m) ORDER BY entity_id,content_id)::text FROM "+table("content_memberships")+" m") != associations {
			t.Fatal("refused upgrade changed existing association decisions")
		}
	}
	t.Run("CK-J01-S02_preflight_precedes_schema_changes", func(t *testing.T) {
		err := ck.MigratePostgres(t.Context(), w.pool, cfg)
		if err == nil || !strings.Contains(err.Error(), "ownership is ambiguous") {
			t.Fatalf("tenantless released projection was not refused: %v", err)
		}
		if ledger() != oldLedger {
			t.Fatal("preflight refusal altered the published migration ledger")
		}
		assertOldShape()
	})
	producer, err := river.NewClient(riverpgxv5.New(w.pool), &river.Config{Schema: schema + "_river"})
	must(t, err)
	// The fixture owns this exact stopped producer envelope; do not guess by kind.
	owned, err := producer.JobGet(t.Context(), old.Projection)
	must(t, err)
	if owned.Kind != "contentkit_entity_projection" || owned.Queue != "contentkit" || string(owned.EncodedArgs) != "{}" {
		t.Fatalf("unexpected retirement target: %+v", owned)
	}
	_, err = producer.JobCancel(t.Context(), old.Projection)
	must(t, err)
	t.Run("CK-J01-S02_future_and_expired_associations_require_explicit_resolution", func(t *testing.T) {
		err := ck.MigratePostgres(t.Context(), w.pool, cfg)
		if err == nil || !strings.Contains(err.Error(), "dated content memberships") {
			t.Fatalf("dated released associations were not refused: %v", err)
		}
		assertOldShape()
		var applied bool
		must(t, w.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM public.migrations WHERE app='contentkit' AND schema=$1 AND sequence='49')`, schema).Scan(&applied))
		if applied {
			t.Fatal("refused timing removal was recorded as applied")
		}
	})
	oldCall("remove-dated", raw)
	must(t, ck.MigratePostgres(t.Context(), w.pool, cfg))
	must(t, ck.MigratePostgres(t.Context(), w.pool, cfg))
	if job(old.Foreign) != foreign || job(old.Historical) != historical {
		t.Fatal("upgrade changed foreign work or terminal history")
	}
	var exists bool
	must(t, w.pool.QueryRow(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, schema+".content_entity_counts").Scan(&exists))
	if exists {
		t.Fatal("retired stored count table survived upgrade")
	}
	engine, err := ck.New(t.Context(), cfg, ck.Deps{Postgres: w.pool, Auth: auth.Authenticator()})
	must(t, err)
	t.Cleanup(func() { must(t, engine.Close(context.Background())) })
	public := ck.NewClient(mountEngine(t, engine).URL+"/content", nil)
	for _, before := range old.Contents {
		after, err := public.GetContent(t.Context(), before.ID, ck.GetContentQuery{})
		must(t, err)
		if after.ID != before.ID || after.Code != before.Code || after.Title != before.Title || after.Body != before.Body || after.Versions[0].ID != before.Versions[0].ID {
			t.Fatalf("upgrade changed content identity/body: before%+v after%+v", before, after)
		}
	}
	page, err := public.GetEntities(t.Context(), ck.GetEntitiesQuery{Kind: "tag", Language: "en"})
	must(t, err)
	if len(page.Entities) != 1 || page.Entities[0].Count != 1 || page.Entities[0].Cover == nil || page.Entities[0].Cover.ContentID != old.Contents[0].ID {
		t.Fatalf("upgraded public count/cover requires a removed worker: %+v", page)
	}
	for _, entity := range old.Entities[1:] {
		members, err := public.GetContents(t.Context(), ck.GetContentsQuery{Kind: "article", Entities: []string{string(entity.ID)}})
		must(t, err)
		if len(members.Contents) != 0 {
			t.Fatal("removed dated membership became permanent")
		}
	}
	var beforeRows, afterRows []map[string]any
	must(t, json.Unmarshal([]byte(oldLedger), &beforeRows))
	must(t, json.Unmarshal([]byte(ledger()), &afterRows))
	bySequence := map[any]map[string]any{}
	for _, row := range afterRows {
		bySequence[row["sequence"]] = row
	}
	for _, row := range beforeRows {
		if !reflect.DeepEqual(row, bySequence[row["sequence"]]) {
			t.Fatal("upgrade rewrote a published migration receipt")
		}
	}
}
