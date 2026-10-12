package e2e

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

// versionsScenario reaches the versions module's routes, and media's
// version reads, over a premium book version by owner with no media yet.
// The world's resolver stands in for the catalog's over the contract tables.
func versionsScenario(t *testing.T, w *world, f *server, owner, user, anon string) {
	book, v1, v2 := cid(40), cid(41), cid(42)
	table := func(name string) string { return pgx.Identifier{w.schema, name}.Sanitize() }
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO ` + table("content_items") + ` (tenant_id, id, kind, state, access) VALUES ($1, $2, 'book', 'published', '{"level":"tier","tiers":["premium"]}')`,
			[]any{w.tenant, book}},
		{`INSERT INTO ` + table("content_creators") + ` (tenant_id, content_id, user_id) VALUES ($1, $2, $3)`, []any{w.tenant, book, w.owner}},
		{`INSERT INTO ` + table("content_versions") + ` (tenant_id, id, content_id, languages, state) VALUES ($1, $2, $4, '{en}', 'published'), ($1, $3, $4, '{en}', 'published')`,
			[]any{w.tenant, v1, v2, book}},
	} {
		if _, err := w.pool.Exec(t.Context(), q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	path := "/contents/" + book + "/versions/" + v1
	var pending versions.Pending
	f.want(202, anon, "GET", path+"/reader", nil, &pending)
	f.want(404, anon, "GET", "/contents/"+cid(43)+"/versions/"+cid(44)+"/reader", nil, nil)
	f.want(401, anon, "PUT", path+"/reader", versions.ReaderMeta{SchemaVersion: 1}, nil)
	f.want(403, user, "PUT", path+"/reader", versions.ReaderMeta{SchemaVersion: 1}, nil)
	f.want(204, owner, "PUT", path+"/reader", versions.ReaderMeta{SchemaVersion: 1}, nil)
	f.want(403, user, "POST", path+"/fork", versions.ForkBody{From: v2}, nil)
	f.want(400, owner, "POST", path+"/fork", versions.ForkBody{}, nil)
	f.want(403, anon, "GET", path+"/download", nil, nil)
	f.want(404, owner, "GET", path+"/download", nil, nil)
	// The published premium version shows, locked.
	var read media.ReadResult
	f.want(200, anon, "GET", "/media/book/"+book+"/versions/"+v1, nil, &read)
	if read.Access != media.AccessNone {
		t.Fatalf("a locked version read %+v", read)
	}
	f.want(404, anon, "GET", "/media/book/"+book+"/versions/"+v1+"/hls/hls/master.m3u8", nil, nil)
}
