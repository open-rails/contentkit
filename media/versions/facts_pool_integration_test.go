package versions

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/workqueue"
)

type transactionFacts struct{}

func (transactionFacts) RefreshTx(ctx context.Context, tx pgx.Tx, _ []string) error {
	_, err := tx.Exec(ctx, "SELECT 1")
	return err
}

// A locked facts transaction must not borrow another connection to validate
// the manifest's version owner. One connection makes the pool cycle deterministic.
func TestFactRefreshUsesItsTransactionForManifestOwnership(t *testing.T) {
	env := s3test.Open(t)
	admin := env.Pool()
	schema := env.ContentSchema()
	ctx := t.Context()
	reg, err := media.NewRegistry(media.Config{Namespace: env.Tenant, Kinds: []media.Kind{{Name: "movie", Versions: true, Uploads: []media.Upload{{Path: "source", Types: []string{"video/mp4"}, MaxBytes: 1024}}}}})
	if err != nil {
		t.Fatal(err)
	}
	target := Target{Content: contentref.NewID(), Version: contentref.NewID()}
	ref, err := reg.VersionRef("movie", target.Content, target.Version)
	if err != nil {
		t.Fatal(err)
	}
	items, versions := pgxIdent(schema, "content_items"), pgxIdent(schema, "content_versions")
	if _, err := admin.Exec(ctx, `INSERT INTO `+items+`(tenant_id,id,kind) VALUES($1,$2,'movie')`, env.Tenant, target.Content); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO `+versions+`(tenant_id,id,content_id,languages,width) VALUES($1,$2,$3,'{en}',99)`, env.Tenant, target.Version, target.Content); err != nil {
		t.Fatal(err)
	}
	workerSchema := pgtest.EmptySchema(t, ctx, admin)
	if err := workqueue.Migrate(ctx, admin, workerSchema); err != nil {
		t.Fatal(err)
	}
	queue, err := workqueue.New(admin, reg, workerSchema)
	if err != nil {
		t.Fatal(err)
	}
	initialJournal, err := media.NewPGJournal(admin, schema, queue)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := media.NewManifests(env.Store, reg, media.ManifestOptions{Locker: media.PGLocker(admin), Journal: initialJournal})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initial.Edit(ctx, ref, func(m *media.Manifest) error { m.Meta = map[string]any{"fixture": true}; return nil }); err != nil {
		t.Fatal(err)
	}
	pool := pgtest.Pool(t, func(c *pgxpool.Config) { c.MaxConns = 1 })
	journal, err := media.NewPGJournal(pool, schema, nil)
	if err != nil {
		t.Fatal(err)
	}
	manifests, err := media.NewManifests(env.Store, reg, media.ManifestOptions{Locker: media.PGLocker(pool), Journal: journal})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{o: Options{Pool: pool, Tenant: env.Tenant, Kinds: kinds.Config{Kinds: map[string]kinds.Kind{"movie": {Presentation: "player"}}}, Facets: transactionFacts{}}, t: tables{items: items, versions: versions}, reg: reg, m: Media{Manifests: manifests}}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.refreshMediaFacts(bounded, "movie", target); err != nil {
		t.Fatalf("facts refresh borrowed outside its held transaction: %v", err)
	}
	var width *int
	if err := admin.QueryRow(ctx, `SELECT width FROM `+versions+` WHERE tenant_id=$1 AND id=$2`, env.Tenant, target.Version).Scan(&width); err != nil || width != nil {
		t.Fatalf("facts not refreshed: width=%v err=%v", width, err)
	}
}
