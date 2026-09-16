package search

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPGroongaDiscoveryIntegrationIsolationAndRecovery(t *testing.T) {
	dsn := os.Getenv("SEARCHKIT_PGROONGA_URL")
	if dsn == "" {
		t.Skip("SEARCHKIT_PGROONGA_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	pools := make([]*pgxpool.Pool, 2)
	for i := range pools {
		db := fmt.Sprintf("sk_discovery_%d_%d", time.Now().UnixNano(), i)
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{db}.Sanitize()); err != nil {
			t.Fatal(err)
		}
		cfg := admin.Config()
		cfg.ConnConfig.Database = db
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		pools[i] = pool
		defer func() {
			pool.Close()
			_, _ = admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{db}.Sanitize())
		}()
		// A genuine missing-extension error must not poison later installation.
		if _, err := getPGroongaExtensionSchema(ctx, pool); err == nil {
			t.Fatal("expected missing extension")
		}
		schema := fmt.Sprintf("extensions_%d", i)
		if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA %s; CREATE EXTENSION pgroonga WITH SCHEMA %s; CREATE TABLE public.search_documents(entity_type text,entity_id text,language text,raw_document text); INSERT INTO public.search_documents VALUES('series','%d','ja','鬼滅の刃'); CREATE INDEX native_titles ON public.search_documents USING pgroonga(raw_document);`, schema, schema, i)); err != nil {
			t.Fatal(err)
		}
		canceled, stop := context.WithCancel(ctx)
		stop()
		if _, err := getPGroongaExtensionSchema(canceled, pool); err == nil {
			t.Fatal("expected canceled lookup")
		}
	}
	for _, i := range []int{0, 1, 0, 1} {
		want := fmt.Sprintf("extensions_%d", i)
		got, err := getPGroongaExtensionSchema(ctx, pools[i])
		if err != nil || got != want {
			t.Fatalf("pool %d: schema=%q err=%v", i, got, err)
		}
		hits, err := PGroongaSearch(ctx, pools[i], "鬼滅", PGroongaOptions{Schema: "public", Language: "ja", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].EntityID != fmt.Sprint(i) {
			t.Fatalf("pool %d returned %v", i, hits)
		}
	}
}
