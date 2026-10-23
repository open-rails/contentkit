package publishing

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/entities"
)

func TestPublishingEntityNamesReuseSpellingVariantsAndAliases(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	created, err := f.s.o.Entities.CreateEntities(ctx, []entities.EntityInput{
		{Kind: "tag", Slug: "big-breasts", Names: []entities.Name{
			{Language: "en", Kind: entities.NameCanonical, Name: "Big Breasts"},
			{Language: "en", Kind: entities.NameAlias, Name: "Large Bust"},
		}},
		{Kind: "artist", Slug: "same-spelling", Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Big Breasts"}}},
		{Kind: "tag", Slug: "other-language", Names: []entities.Name{{Language: "ja", Kind: entities.NameCanonical, Name: "Large Bust"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.o.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, name := range []string{"big-breasts", "big breasts", "Big Breasts", "bigbreasts", "BIG_BREASTS", " big   breasts ", "big–breasts", "ｂｉｇ ｂｒｅａｓｔｓ", "large-bust", "LARGEBUST"} {
		got, err := f.s.namedEntityIDs(ctx, tx, "tag", "", "en", name)
		if err != nil || !slices.Equal(got, []string{string(created[0].ID)}) {
			t.Errorf("%q resolved to %v: %v", name, got, err)
		}
	}
	got, err := f.s.namedEntityIDs(ctx, tx, "artist", "", "en", "bigbreasts")
	if err != nil || !slices.Equal(got, []string{string(created[1].ID)}) {
		t.Fatalf("kind isolation: %v %v", got, err)
	}
	got, err = f.s.namedEntityIDs(ctx, tx, "tag", "", "ja", "large bust")
	if err != nil || !slices.Equal(got, []string{string(created[2].ID)}) {
		t.Fatalf("language isolation: %v %v", got, err)
	}
}

func TestPublishingEntityAliasIsAssociatedWithoutCreatingDuplicate(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	created, err := f.s.o.Entities.CreateEntities(ctx, []entities.EntityInput{{Kind: "tag", Slug: "big-breasts", Names: []entities.Name{
		{Language: "en", Kind: entities.NameCanonical, Name: "Big Breasts"},
		{Language: "en", Kind: entities.NameAlias, Name: "Large Bust"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"big-breasts", "BIGBREASTS", "large_bust"} {
		in := request()
		in.CreateMissingEntities = true
		in.Entities = []EntityReference{{Kind: "tag", Name: name}}
		op, err := f.s.Put(ctx, f.actor, uuid.NewString(), in)
		if err != nil {
			t.Fatal(err)
		}
		if len(op.Entities) != 1 || op.Entities[0].ID != string(created[0].ID) || op.Entities[0].Name != "Big Breasts" || op.Entities[0].Created {
			t.Fatalf("%q did not reuse the canonical entity: %+v", name, op.Entities)
		}
		var attached bool
		err = f.s.o.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+pgx.Identifier{f.schema, "content_memberships"}.Sanitize()+` WHERE tenant_id=$1 AND content_id=$2 AND entity_id=$3 AND state='active')`, f.s.o.Tenant, op.ContentID, created[0].ID).Scan(&attached)
		if err != nil || !attached {
			t.Fatalf("reused entity was not associated: %v %v", attached, err)
		}
	}
	var count int
	if err = f.s.o.Pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{f.schema, "content_entities"}.Sanitize()+` WHERE tenant_id=$1 AND kind='tag'`, f.s.o.Tenant).Scan(&count); err != nil || count != 1 {
		t.Fatalf("alias created duplicates: %d %v", count, err)
	}
}

func TestPublishingConcurrentEntitySpellingVariantsCreateOnce(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// Keep the fixture's one-connection authority checks, but let the actual
	// operation transactions overlap on an independent pool to this same DB.
	config := f.s.o.Pool.Config()
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	options := f.s.o
	options.Pool = pool
	store, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		op  Operation
		err error
	}
	results := make(chan outcome, 4)
	start := make(chan struct{})
	var pending sync.WaitGroup
	for _, name := range []string{"big-breasts", "big breasts", "BIGBREASTS", "Big_Breasts"} {
		pending.Add(1)
		go func(name string) {
			defer pending.Done()
			<-start
			in := request()
			in.CreateMissingEntities = true
			in.Entities = []EntityReference{{Kind: "tag", Name: name}}
			op, err := store.Put(ctx, f.actor, uuid.NewString(), in)
			results <- outcome{op, err}
		}(name)
	}
	close(start)
	pending.Wait()
	close(results)
	var id string
	var creations int
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if len(result.op.Entities) != 1 {
			t.Fatalf("missing resolved entity: %+v", result.op)
		}
		e := result.op.Entities[0]
		if id != "" && id != e.ID {
			t.Fatalf("concurrent submissions created distinct entities: %s and %s", id, e.ID)
		}
		id = e.ID
		if e.Created {
			creations++
		}
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{f.schema, "content_entities"}.Sanitize()+` WHERE tenant_id=$1 AND kind='tag'`, f.s.o.Tenant).Scan(&count); err != nil || count != 1 || creations != 1 {
		t.Fatalf("expected one entity creation: rows=%d receipts=%d err=%v", count, creations, err)
	}
}

func TestPublishingEntityNameAmbiguityScopeAndPunctuation(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	series, err := f.s.o.Entities.CreateEntities(ctx, []entities.EntityInput{
		{Kind: "series", Slug: "one", Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "One"}}},
		{Kind: "series", Slug: "two", Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Two"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := f.s.o.Entities.CreateEntities(ctx, []entities.EntityInput{
		{Kind: "tag", Slug: "first", Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Big Breasts"}}},
		{Kind: "tag", Slug: "second", Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "big-breasts"}}},
		{Kind: "artist", Slug: "c", Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "C"}}},
		{Kind: "artist", Slug: "cpp", Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "C++"}}},
		{Kind: "artist", Slug: "acdc", Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "AC/DC"}}},
		{Kind: "character", Slug: "same", ScopeID: series[0].ID, Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Same Name"}}},
		{Kind: "character", Slug: "same", ScopeID: series[1].ID, Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Same Name"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.o.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	ambiguous, err := f.s.namedEntityIDs(ctx, tx, "tag", "", "en", "bigbreasts")
	if err != nil || len(ambiguous) != 2 {
		t.Fatalf("existing duplicates must be ambiguous: %v %v", ambiguous, err)
	}
	for _, tc := range []struct {
		kind, scope, name string
		want              []string
	}{
		{"artist", "", "C", []string{string(created[2].ID)}},
		{"artist", "", "C++", []string{string(created[3].ID)}},
		{"artist", "", "ACDC", nil},
		{"character", string(series[0].ID), "samename", []string{string(created[5].ID)}},
		{"character", string(series[1].ID), "same-name", []string{string(created[6].ID)}},
	} {
		got, err := f.s.namedEntityIDs(ctx, tx, tc.kind, tc.scope, "en", tc.name)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s/%s/%q: %v %v", tc.kind, tc.scope, tc.name, got, err)
		}
	}
}
