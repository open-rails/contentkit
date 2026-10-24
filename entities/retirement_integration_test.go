package entities

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var errRetirementFixture = errors.New("storage retirement unavailable")

type controlledRetirement struct {
	pool   *pgxpool.Pool
	table  string
	passes int
}

func (f *controlledRetirement) RetireTx(ctx context.Context, tx pgx.Tx, refs []Ref) error {
	for _, ref := range refs {
		if _, err := tx.Exec(ctx, `INSERT INTO `+f.table+`(id) VALUES($1) ON CONFLICT DO NOTHING`, string(ref.ID)); err != nil {
			return err
		}
	}
	return nil
}
func (f *controlledRetirement) RetirementStartedTx(ctx context.Context, tx pgx.Tx, ref Ref) (bool, error) {
	var yes bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+f.table+` WHERE id=$1)`, string(ref.ID)).Scan(&yes)
	return yes, err
}
func (f *controlledRetirement) RetirementComplete(ctx context.Context, ref Ref) (bool, error) {
	var yes bool
	err := f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+f.table+` WHERE id=$1 AND complete)`, string(ref.ID)).Scan(&yes)
	return yes, err
}
func (f *controlledRetirement) PurgeRetired(_ context.Context, limit int) (int, error) {
	f.passes++
	return limit, errRetirementFixture
}

// Storage can spend its complete attempt limit on failures. Entity claims and
// success marking must still visit later IDs, using one storage pass per sweep.
func TestRetirementFailureDoesNotStarveLaterEntityClaims(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	table := f.s.table("test_media_retirements")
	if _, err := f.pool.Exec(ctx, `CREATE TABLE `+table+`(id text PRIMARY KEY,complete boolean NOT NULL DEFAULT false)`); err != nil {
		t.Fatal(err)
	}
	media := &controlledRetirement{pool: f.pool, table: table}
	f.s.folders = media
	const count = 205
	ids := make([]string, count)
	for i := range ids {
		ids[i] = cid(i + 1)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO `+f.s.table("content_entities")+`(tenant_id,id,kind,slug,state,deleted_at,owner_user_id)
SELECT $1,id,'channel','retiring-'||n,'deleted',now()-interval '31 days','owner' FROM unnest($2::text[]) WITH ORDINALITY x(id,n)`, tenant, ids); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.PurgeFolders(ctx, 100); n != 0 || !errors.Is(err, errRetirementFixture) {
		t.Fatalf("failed storage pass: %d %v", n, err)
	}
	var claimed, marked int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&claimed); err != nil || claimed != count {
		t.Fatalf("later entity intents starved: %d %v", claimed, err)
	}
	if media.passes != 1 {
		t.Fatalf("storage was scanned repeatedly in one sweep: %d", media.passes)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM `+f.s.table("content_entities")+` WHERE tenant_id=$1 AND media_purged_at IS NOT NULL`, tenant).Scan(&marked); err != nil || marked != 0 {
		t.Fatalf("failed storage marked purged: %d %v", marked, err)
	}
	// A later ID has completed independently; a still-failing prefix must not
	// prevent its durable success marker or prematurely mark the failed ones.
	if _, err := f.pool.Exec(ctx, `UPDATE `+table+` SET complete=true WHERE id=$1`, ids[count-1]); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.PurgeFolders(ctx, 100); n != 1 || !errors.Is(err, errRetirementFixture) {
		t.Fatalf("later completed folder not confirmed: %d %v", n, err)
	}
	if media.passes != 2 {
		t.Fatalf("unexpected storage passes: %d", media.passes)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM `+f.s.table("content_entities")+` WHERE tenant_id=$1 AND media_purged_at IS NOT NULL`, tenant).Scan(&marked); err != nil || marked != 1 {
		t.Fatalf("incorrect completion markers: %d %v", marked, err)
	}
}
