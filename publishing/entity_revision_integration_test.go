package publishing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
)

func TestEntityMembershipChangesInvalidatePublishingWithoutRestampingContent(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	in := request()
	in.CreateMissingEntities = true
	in.Entities = []EntityReference{{Kind: "tag", Name: "Revision guard"}}
	op, err := f.s.Put(ctx, f.actor, uuid.NewString(), in)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.s.load(ctx, f.s.o.Pool, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	sourceTime := time.Date(2020, 1, 3, 0, 0, 0, 0, time.UTC)
	if _, err = f.s.o.Pool.Exec(ctx, `UPDATE `+f.s.items+` SET updated_at=$3 WHERE tenant_id=$1 AND id=$2`, f.s.o.Tenant, op.ContentID, sourceTime); err != nil {
		t.Fatal(err)
	}
	if err = f.s.o.Entities.RemoveMemberships(ctx, []entities.Membership{{EntityID: entities.EntityID(op.Entities[0].ID), ContentID: op.ContentID}}); err != nil {
		t.Fatal(err)
	}
	var revision int64
	var updated time.Time
	if err = f.s.o.Pool.QueryRow(ctx, `SELECT revision,updated_at FROM `+f.s.items+` WHERE tenant_id=$1 AND id=$2`, f.s.o.Tenant, op.ContentID).Scan(&revision, &updated); err != nil {
		t.Fatal(err)
	}
	if revision != before.contentRevision+1 || !updated.Equal(sourceTime) {
		t.Fatalf("membership edit must advance revision and retain source timestamp: revision=%d updated_at=%s", revision, updated)
	}
	err = f.s.o.Catalog.PublishChecked(ctx, f.actor, []catalog.Publish{{ContentID: op.ContentID}}, func(ctx context.Context, tx pgx.Tx) error {
		return f.s.checkRevision(ctx, tx, before)
	})
	var changed *OperationError
	if !errors.As(err, &changed) || changed.Code != "content_changed" {
		t.Fatalf("stale publishing operation was not refused: %v", err)
	}
	var state string
	if err = f.s.o.Pool.QueryRow(ctx, `SELECT state FROM `+f.s.items+` WHERE tenant_id=$1 AND id=$2`, f.s.o.Tenant, op.ContentID).Scan(&state); err != nil || state != catalog.StateDraft {
		t.Fatalf("stale operation published content: state=%s err=%v", state, err)
	}
}
