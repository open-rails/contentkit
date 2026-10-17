package catalog

import (
	"context"
	"slices"
	"testing"
	"time"
)

// Purging erases an item's or a version's interactions with it; the code stays
// so its URL keeps answering 410, and the outbox names what went.
func TestPurgeErasesInteractionsIntegration(t *testing.T) {
	f := newFixture(t, nil)
	var events []Event
	f.s.Subscribe(func(_ context.Context, batch []Event) error {
		events = append(events, batch...)
		return nil
	})
	gone, kept := f.create("staff", gallery("Gone", "en", "ja")), f.create("staff", gallery("Kept", "en", "ja"))
	f.publish("staff", gone, nil)
	f.publish("staff", kept, nil)
	read := f.read("staff", "main.test", gone, kept)
	goneVersion, keptVersion, purgedVersion := read[0].Versions[0].ID, read[1].Versions[0].ID, read[1].Versions[1].ID

	revision := int64(0)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// interact leaves every kind of interaction on a work (vid "") or a version.
	interact := func(id, vid string) {
		t.Helper()
		var version any
		if vid != "" {
			version = vid
		}
		revision++
		exec(`INSERT INTO `+f.table("content_reactions")+` (tenant_id, content_kind, content_id, content_version_id, user_id, value, revision) VALUES ($1, 'gallery', $2, $3, 'fan', 1, $4)`, tenant, id, version, revision)
		exec(`INSERT INTO `+f.table("content_favorites")+` (tenant_id, user_id, content_kind, content_id, content_version_id, value, revision) VALUES ($1, 'fan', 'gallery', $2, $3, 1, $4)`, tenant, id, version, revision)
		exec(`INSERT INTO `+f.table("content_interaction_counts")+` (tenant_id, content_kind, content_id, content_version_id, likes, favorites) VALUES ($1, 'gallery', $2, $3, 1, 1)`, tenant, id, version)
		var comment string
		if err := f.pool.QueryRow(f.ctx, `INSERT INTO `+f.table("content_comments")+` (tenant_id, content_kind, content_id, content_version_id, user_id, body) VALUES ($1, 'gallery', $2, $3, 'fan', 'nice') RETURNING id::text`, tenant, id, version).Scan(&comment); err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO `+f.table("content_comments")+` (tenant_id, content_kind, content_id, content_version_id, reply_to_id, user_id, body) VALUES ($1, 'gallery', $2, $3, $4::uuid, 'other', 'agreed')`, tenant, id, version, comment)
		revision++
		exec(`INSERT INTO `+f.table("content_reactions")+` (tenant_id, content_kind, content_id, user_id, value, revision) VALUES ($1, 'comment', $2, 'other', 1, $3)`, tenant, comment, revision)
		exec(`INSERT INTO `+f.table("content_interaction_counts")+` (tenant_id, content_kind, content_id, likes) VALUES ($1, 'comment', $2, 1)`, tenant, comment)
		if vid != "" {
			exec(`INSERT INTO `+f.table("content_access_history")+` (tenant_id, content_id, version_id, occurred_at, snapshot) VALUES ($1, $2, $3, now(), '{}')`, tenant, id, vid)
		}
	}
	interact(gone, "")
	interact(gone, goneVersion)
	interact(kept, "")
	interact(kept, keptVersion)
	interact(kept, purgedVersion)

	// left counts rows of a table about content id (and version vid when set),
	// including reactions and counts on their comments.
	left := func(table, id, vid string) int {
		t.Helper()
		where := `tenant_id = $1 AND content_id = $2`
		args := []any{tenant, id}
		if table == "content_access_history" {
			if vid != "" {
				where, args = where+` AND version_id = $3`, append(args, vid)
			}
		} else {
			if vid != "" {
				where, args = where+` AND content_version_id = $3`, append(args, vid)
			}
			if table == "content_reactions" || table == "content_interaction_counts" {
				where = `tenant_id = $1 AND ((` + where[len(`tenant_id = $1 AND `):] + `) OR (content_kind = 'comment' AND content_id IN (
 SELECT id::text FROM ` + f.table("content_comments") + ` WHERE tenant_id = $1 AND content_id = $2` + map[bool]string{true: ` AND content_version_id = $3`}[vid != ""] + `)))`
			}
		}
		var n int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.table(table)+` WHERE `+where, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	tables := []string{"content_reactions", "content_favorites", "content_interaction_counts", "content_comments", "content_access_history"}
	codes := func(id string) int {
		var n int
		if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.table("content_codes")+` WHERE tenant_id = $1 AND content_id = $2`, tenant, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	goneCodes := codes(gone)

	if err := f.s.Delete(f.ctx, f.as("staff"), []string{gone}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteVersions(f.ctx, f.as("staff"), []VersionKey{{ContentID: kept, VersionID: purgedVersion}}); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.PurgeDeleted(f.ctx); err != nil || n != 0 {
		t.Fatalf("purge inside the retention: %d %v", n, err)
	}
	for _, table := range tables {
		if left(table, gone, "") == 0 || left(table, kept, purgedVersion) == 0 {
			t.Fatalf("%s erased inside the retention", table)
		}
	}

	f.advance(DefaultDeletedRetention + time.Hour)
	if n, err := f.s.PurgeDeleted(f.ctx); err != nil || n != 2 {
		t.Fatalf("purge: %d %v, want the item and the version", n, err)
	}
	for _, table := range tables {
		if n := left(table, gone, ""); n != 0 {
			t.Errorf("%s keeps %d rows of the purged item", table, n)
		}
		if n := left(table, kept, purgedVersion); n != 0 {
			t.Errorf("%s keeps %d rows of the purged version", table, n)
		}
		if n := left(table, kept, keptVersion); n == 0 {
			t.Errorf("%s lost the kept version's rows", table)
		}
	}
	if n := left("content_favorites", kept, ""); n != 2 {
		t.Errorf("the kept item's work and version favorites: %d, want 2", n)
	}
	got := f.read("staff", "main.test", kept)[0]
	if got == nil || len(got.Versions) != 1 || got.Versions[0].ID != keptVersion {
		t.Fatalf("the kept item after purging its deleted version: %+v", got)
	}
	if codes(gone) != goneCodes {
		t.Errorf("the purged item's code went: %d, want %d", codes(gone), goneCodes)
	}
	if n, err := f.s.PurgeDeleted(f.ctx); err != nil || n != 0 {
		t.Fatalf("purging again: %d %v", n, err)
	}

	if _, err := f.s.DeliverEvents(f.ctx); err != nil {
		t.Fatal(err)
	}
	purged := slices.ContainsFunc(events, func(e Event) bool { return e.Type == EventContentPurged && e.ContentID == gone })
	version := slices.ContainsFunc(events, func(e Event) bool {
		return e.Type == EventVersionPurged && e.ContentID == kept && e.VersionID == purgedVersion && e.Kind == "gallery"
	})
	if !purged || !version {
		t.Fatalf("outbox: item purged %v, version purged %v: %+v", purged, version, events)
	}
}

// A backlog larger than one batch drains in one run; a sold item's deleted
// versions stay for its holders.
func TestPurgeDrainsBacklogIntegration(t *testing.T) {
	f := newFixture(t, nil)
	var ids []string
	for range purgeBatch + 3 {
		ids = append(ids, f.create("staff", gallery("Old", "en")))
	}
	sold := f.create("staff", gallery("Sold", "en", "ja"))
	f.publish("staff", sold, nil)
	if _, err := f.pool.Exec(f.ctx, `UPDATE `+f.table("content_items")+` SET ever_sold = true WHERE id = $1`, sold); err != nil {
		t.Fatal(err)
	}
	soldVersion := f.read("staff", "main.test", sold)[0].Versions[1].ID
	if err := f.s.DeleteVersions(f.ctx, f.as("staff"), []VersionKey{{ContentID: sold, VersionID: soldVersion}}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Delete(f.ctx, f.as("staff"), ids); err != nil {
		t.Fatal(err)
	}
	f.advance(DefaultDeletedRetention + time.Hour)
	if n, err := f.s.PurgeDeleted(f.ctx); err != nil || n != len(ids) {
		t.Fatalf("purge: %d %v, want %d", n, err, len(ids))
	}
	var items, versions int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.table("content_items")).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.table("content_versions")+` WHERE id = $1`, soldVersion).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if items != 1 || versions != 1 {
		t.Fatalf("after draining: %d items, sold item's deleted version kept %d", items, versions)
	}
}
