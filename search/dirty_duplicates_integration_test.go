package search_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/search"
)

func TestMarkDirtyCoalescesDuplicateValueKeys(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, t.Context(), pool)
	ctx := t.Context()
	base := contentref.New("duplicate-test", "gallery", "01920000-0000-7000-8000-000000000001")
	key := func(ref contentref.ContentRef, language string) search.DocumentKey {
		return search.DocumentKey{ContentRef: ref, Language: language}
	}
	parent := key(base, "en")
	first := search.DirtyMark{DocumentKey: parent, Reason: "before"}
	if err := search.MarkDirty(ctx, pool, schema, []search.DirtyMark{first}); err != nil {
		t.Fatal(err)
	}
	table := pgx.Identifier{schema, "content_search_dirty"}.Sanitize()
	var before int64
	if err := pool.QueryRow(ctx, `SELECT revision FROM `+table+` WHERE content_id=$1`, base.ContentID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// NewVersion allocates a distinct pointer each time; identity is its value.
	marks := []search.DirtyMark{
		first,
		{DocumentKey: key(base.WithVersion("edition-one"), "en"), Reason: "old-version"},
		{DocumentKey: parent, Deleted: true, Reason: "delete"},
		{DocumentKey: key(base, "ja"), Reason: "other-language"},
		{DocumentKey: key(base.WithVersion("edition-two"), "en"), Reason: "other-version"},
		{DocumentKey: parent, Deleted: false, Reason: "  final-parent  "},
		{DocumentKey: key(base.WithVersion("edition-one"), "en"), Deleted: true, Reason: "final-version"},
	}
	if err := search.MarkDirty(ctx, pool, schema, marks); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT coalesce(content_version_id,''),language,is_deleted,reason,revision FROM `+table+` WHERE content_id=$1`, base.ContentID)
	if err != nil {
		t.Fatal(err)
	}
	type state struct {
		deleted  bool
		reason   string
		revision int64
	}
	got := map[string]state{}
	for rows.Next() {
		var version, language string
		var s state
		if err := rows.Scan(&version, &language, &s.deleted, &s.reason, &s.revision); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		got[version+"/"+language] = s
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got["/en"].deleted || got["/en"].reason != "final-parent" || got["/en"].revision <= before || !got["edition-one/en"].deleted || got["edition-one/en"].reason != "final-version" || got["/ja"].reason != "other-language" || got["edition-two/en"].reason != "other-version" {
		t.Fatalf("coalesced dirty documents: %+v", got)
	}
	// An empty last reason has the same host default as a separate later call.
	if err := search.MarkDirty(ctx, pool, schema, []search.DirtyMark{{DocumentKey: parent, Reason: "earlier"}, {DocumentKey: parent}}); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT reason FROM `+table+` WHERE content_id=$1 AND content_version_id IS NULL AND language='en'`, base.ContentID).Scan(&reason); err != nil || reason != "host" {
		t.Fatalf("last empty reason: %q %v", reason, err)
	}
}

func TestMarkDirtyValidatesEveryDuplicateBeforeWriting(t *testing.T) {
	pool := pgtest.Pool(t, nil)
	schema := pgtest.Schema(t, t.Context(), pool)
	good := search.DocumentKey{ContentRef: contentref.New("validation-test", "gallery", "01920000-0000-7000-8000-000000000001"), Language: "en"}
	invalid := good
	empty := ""
	invalid.ContentVersionID = &empty
	if err := search.MarkDirty(context.Background(), pool, schema, []search.DirtyMark{{DocumentKey: invalid}, {DocumentKey: good}}); err == nil {
		t.Fatal("invalid overwritten key was ignored")
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{schema, "content_search_dirty"}.Sanitize()).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid batch partially wrote %d rows: %v", count, err)
	}
}
