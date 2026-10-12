package e2e

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/media/versions"
)

// books stands in for the catalog's visibility and the gate's access over
// the contract tables until those modules land: a version is live when it
// and its content are published, its content's creators edit it, and only
// a public rule opens it.
type books struct{ w *world }

func (b books) See(ctx context.Context, a access.Actor, targets []versions.Target) (map[versions.Target]versions.Sight, error) {
	out := map[versions.Target]versions.Sight{}
	for _, t := range targets {
		var live, creator bool
		err := b.w.pool.QueryRow(ctx, `SELECT c.state = 'published' AND v.state = 'published',
	EXISTS (SELECT 1 FROM `+b.t("content_creators")+` cr WHERE cr.tenant_id = c.tenant_id AND cr.content_id = c.id AND cr.user_id = $4)
FROM `+b.t("content_items")+` c JOIN `+b.t("content_versions")+` v ON v.tenant_id = c.tenant_id AND v.content_id = c.id
WHERE c.tenant_id = $1 AND c.id = $2 AND ($3 = '' OR v.id = $3) LIMIT 1`, tenant, t.Content, t.Version, a.ID).Scan(&live, &creator)
		if err == pgx.ErrNoRows {
			continue
		} else if err != nil {
			return nil, err
		}
		out[t] = versions.Sight{Visible: live || creator && a.Person(), Live: live, Editor: creator && a.Person()}
	}
	return out, nil
}

func (b books) Open(ctx context.Context, _ access.Actor, vs []versions.Target) (map[versions.Target]versions.Opening, error) {
	out := map[versions.Target]versions.Opening{}
	for _, t := range vs {
		var public bool
		if err := b.w.pool.QueryRow(ctx, `SELECT access->>'level' = 'public' FROM `+b.t("content_items")+` WHERE tenant_id = $1 AND id = $2`,
			tenant, t.Content).Scan(&public); err != nil && err != pgx.ErrNoRows {
			return nil, err
		}
		out[t] = versions.Opening{Opens: public}
	}
	return out, nil
}

func (books) Tiers(context.Context, access.Actor) ([]string, error) { return nil, nil }
func (books) RefreshTx(context.Context, pgx.Tx, []string) error     { return nil }
func (b books) t(name string) string                                { return pgx.Identifier{b.w.schema, name}.Sanitize() }

// versionsScenario reaches the versions module's routes, and media's
// version reads, over a premium book version by owner with no media yet.
func versionsScenario(t *testing.T, w *world, f *server, owner, user, anon string) {
	svc, err := versions.New(versions.Options{Tenant: tenant, Schema: w.schema, Pool: w.pool, Visibility: books{w}, Access: books{w}, Facets: books{w}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Bind(versions.Media{Manifests: w.jobs.Manifests(), Uploads: w.up, Jobs: w.jobs}); err != nil {
		t.Fatal(err)
	}
	h, err := svc.Handler(w.ak.Authenticator(), nil)
	if err != nil {
		t.Fatal(err)
	}
	f.mount("/contents", h)
	book, v1, v2 := cid(40), cid(41), cid(42)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO ` + books{w}.t("content_items") + ` (tenant_id, id, kind, state, access) VALUES ($1, $2, 'book', 'published', '{"level":"tier","tiers":["premium"]}')`,
			[]any{tenant, book}},
		{`INSERT INTO ` + books{w}.t("content_creators") + ` (tenant_id, content_id, user_id) VALUES ($1, $2, $3)`, []any{tenant, book, w.owner}},
		{`INSERT INTO ` + books{w}.t("content_versions") + ` (tenant_id, id, content_id, languages, state) VALUES ($1, $2, $4, '{en}', 'published'), ($1, $3, $4, '{en}', 'published')`,
			[]any{tenant, v1, v2, book}},
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
	f.want(404, anon, "GET", "/media/book/"+book+"/versions/"+v1, nil, nil)
	f.want(404, anon, "GET", "/media/book/"+book+"/versions/"+v1+"/hls/hls/master.m3u8", nil, nil)
}
