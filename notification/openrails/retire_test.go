package openrails

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
)

func TestRetireInboxSyncRequiresHandoffAndPreservesOtherJobs(t *testing.T) {
	ctx := t.Context()
	pool, schema := testStorage(t) // size-one pool proves no nested pool acquire
	if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	source := sourceFor(t, &eventServer{events: []Event{event("old-failure", "customer")}})
	destination := inbox(t, pool, schema, "accounts")
	key := fmt.Sprintf("%x", sha256.Sum256([]byte("accounts\x00billing\x00m")))
	table := pgx.Identifier{schema, "river_job"}.Sanitize()
	if _, err := pool.Exec(ctx, `INSERT INTO `+table+` (kind,args,queue,max_attempts,state,finalized_at)
	VALUES ('contentkit_openrails_inbox_sync',jsonb_build_object('key',$1::text),'legacy',1,'available',NULL),
	 ('contentkit_openrails_inbox_sync','{"key":"other-source"}','legacy',1,'available',NULL),
	 ('contentkit_openrails_inbox_sync',jsonb_build_object('key',$1::text),'legacy',1,'completed',clock_timestamp()),
	 ('host_custom_event_reader',jsonb_build_object('key',$1::text),'legacy',1,'available',NULL)`, key); err != nil {
		t.Fatal(err)
	}
	if err := source.RetireInboxSync(ctx, pool, schema, "accounts", destination); !errors.Is(err, ErrInboxHandoffRequired) {
		t.Fatalf("unfinished handoff accepted: %v", err)
	}
	if result, err := source.Consume(ctx, destination, mapped, ConsumeOptions{Filter: BillingLifecycle}); err != nil || result.Delivered != 1 {
		t.Fatalf("handoff: %+v %v", result, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE `+table+` SET state='running' WHERE kind='contentkit_openrails_inbox_sync' AND args->>'key'=$1 AND state='available'`, key); err != nil {
		t.Fatal(err)
	}
	if err := source.RetireInboxSync(ctx, pool, schema, "accounts", destination); err == nil {
		t.Fatal("running legacy worker was retired")
	}
	if _, err := pool.Exec(ctx, `UPDATE `+table+` SET state='available' WHERE kind='contentkit_openrails_inbox_sync' AND args->>'key'=$1 AND state='running'`, key); err != nil {
		t.Fatal(err)
	}
	if err := source.RetireInboxSync(ctx, pool, schema, "accounts", destination); err != nil {
		t.Fatal(err)
	}
	if err := source.RetireInboxSync(ctx, pool, schema, "accounts", destination); err != nil {
		t.Fatalf("repeated retirement: %v", err)
	}
	var cancelled, completed, available int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='cancelled'),count(*) FILTER (WHERE state='completed'),count(*) FILTER (WHERE state='available') FROM `+table).Scan(&cancelled, &completed, &available); err != nil || cancelled != 1 || completed != 1 || available != 2 {
		t.Fatalf("retirement touched wrong jobs: cancelled=%d completed=%d available=%d err=%v", cancelled, completed, available, err)
	}
}

func TestRetireInboxSyncChecksBacklogWithoutLiveJobs(t *testing.T) {
	for _, state := range []string{"no_table", "between_ticks", "discarded"} {
		t.Run(state, func(t *testing.T) {
			ctx := t.Context()
			pool, schema := testStorage(t)
			if state != "no_table" {
				if err := riverhelpers.ApplyMigrations(ctx, pool, schema); err != nil {
					t.Fatal(err)
				}
			}
			if state == "discarded" {
				key := fmt.Sprintf("%x", sha256.Sum256([]byte("accounts\x00billing\x00m")))
				if _, err := pool.Exec(ctx, `INSERT INTO `+pgx.Identifier{schema, "river_job"}.Sanitize()+`
				(kind,args,queue,max_attempts,state,finalized_at) VALUES
				('contentkit_openrails_inbox_sync',jsonb_build_object('key',$1::text),'legacy',1,'discarded',clock_timestamp())`, key); err != nil {
					t.Fatal(err)
				}
			}
			source := sourceFor(t, &eventServer{events: []Event{event("unconsumed", "customer")}})
			destination := inbox(t, pool, schema, "accounts")
			if err := source.RetireInboxSync(ctx, pool, schema, "accounts", destination); !errors.Is(err, ErrInboxHandoffRequired) {
				t.Fatalf("backlog without a live job bypassed handoff: %v", err)
			}
			if _, err := source.Consume(ctx, destination, mapped, ConsumeOptions{Filter: BillingLifecycle}); err != nil {
				t.Fatal(err)
			}
			if err := source.RetireInboxSync(ctx, pool, schema, "accounts", destination); err != nil {
				t.Fatalf("finished source refused retirement: %v", err)
			}
		})
	}
}
