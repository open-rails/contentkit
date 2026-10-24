package catalog

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestVersionDeletionOwnsParentLifecycle(t *testing.T) {
	f := newFixture(t, nil)
	id := f.create("staff", gallery("Edition lifecycle", "en", "ja"))
	f.publish("staff", id, nil)
	versions := f.read("staff", "main.test", id)[0].Versions
	remove := func(version string) {
		t.Helper()
		if err := f.s.DeleteVersions(f.ctx, f.as("staff"), []VersionKey{{ContentID: id, VersionID: version}}); err != nil {
			t.Fatal(err)
		}
	}
	remove(versions[0].ID)
	if f.read("staff", "main.test", id)[0].State != StatePublished {
		t.Fatal("removing one edition deleted a nonempty parent")
	}
	remove(versions[1].ID)
	assertParentCause(t, f, id, StateDeleted, true)
	if err := f.s.Restore(f.ctx, f.as("staff"), []string{id}); !errors.Is(err, ErrConflict) {
		t.Fatalf("empty parent restored independently: %v", err)
	}
	if _, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionID: versions[0].ID, VersionInput: VersionInput{State: StatePublished}}}); err != nil {
		t.Fatal(err)
	}
	assertParentCause(t, f, id, StatePublished, false)
	var lifecycle []string
	for _, event := range readEventLog(t, f) {
		if event.Type == EventContentDeleted || event.Type == EventContentRestored {
			if event.ContentID != id || event.Kind != "gallery" {
				t.Errorf("invalid parent lifecycle event: %+v", event)
			}
			lifecycle = append(lifecycle, event.Type)
		}
	}
	if !slices.Equal(lifecycle, []string{EventContentDeleted, EventContentRestored}) {
		t.Fatalf("parent lifecycle events: %v", lifecycle)
	}
	got := f.read("staff", "main.test", id)[0]
	for _, version := range got.Versions {
		if version.ID == versions[1].ID && version.State != StateDeleted {
			t.Fatal("restoring one edition restored its deleted sibling")
		}
	}
	if err := f.s.DeleteForModeration(f.ctx, f.as("staff"), []string{id}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionID: versions[1].ID, VersionInput: VersionInput{State: StatePublished}}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("version change bypassed moderation deletion: %v", err)
	}
	assertParentCause(t, f, id, StateDeleted, false)
}

// Hidden is a presentation choice: its sole edition owns deletion as well.
func TestHiddenVersionDeletionAndRestoration(t *testing.T) {
	for _, state := range []string{"draft", "published", "scheduled"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t, nil)
			id := f.create("staff", ContentInput{Kind: "blog", Text: map[string]Text{"en": {Title: "Hidden edition"}}})
			if state == "published" {
				f.publish("staff", id, nil)
			} else if state == "scheduled" {
				at := f.now().Add(time.Hour)
				f.publish("staff", id, &at)
			}
			before := f.read("staff", "main.test", id)[0]
			if len(before.Versions) != 1 || before.Versions[0].ID == "" {
				t.Fatalf("hidden edition missing from editor read: %+v", before)
			}
			key := VersionKey{ContentID: id, VersionID: before.Versions[0].ID}
			if err := f.s.DeleteVersions(f.ctx, f.as("bob"), []VersionKey{key}); !errors.Is(err, ErrForbidden) {
				t.Fatalf("unauthorized hidden deletion: %v", err)
			}
			if err := f.s.DeleteVersions(f.ctx, f.as("staff"), []VersionKey{key}); err != nil {
				t.Fatal(err)
			}
			assertParentCause(t, f, id, StateDeleted, true)
			deleted := f.read("staff", "main.test", id)[0]
			if len(deleted.Versions) != 1 || deleted.Versions[0].State != StateDeleted {
				t.Fatalf("deleted hidden edition cannot be restored by its id: %+v", deleted)
			}
			if err := f.s.Restore(f.ctx, f.as("staff"), []string{id}); !errors.Is(err, ErrConflict) {
				t.Fatalf("hidden parent restored independently: %v", err)
			}
			if _, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionID: key.VersionID, VersionInput: VersionInput{State: StatePublished}}}); err != nil {
				t.Fatal(err)
			}
			assertParentCause(t, f, id, before.State, false)
			after := f.read("staff", "main.test", id)[0]
			if len(after.Versions) != 1 || after.Versions[0].State != StatePublished || after.Live != before.Live || !sameTime(after.PublishAt, before.PublishAt) {
				t.Fatalf("restoring hidden edition changed its publication: before=%+v after=%+v", before, after)
			}
			var events []string
			for _, event := range readEventLog(t, f) {
				if event.Type == EventVersionDeleted || event.Type == EventContentDeleted || event.Type == EventContentRestored {
					events = append(events, event.Type)
				}
			}
			if !slices.Equal(events, []string{EventVersionDeleted, EventContentDeleted, EventContentRestored}) {
				t.Fatalf("hidden lifecycle events: %v", events)
			}
		})
	}
}

func TestVersionDeletionBatchRollsBackParentAndEvents(t *testing.T) {
	f := newFixture(t, nil)
	id := f.create("staff", ContentInput{Kind: "blog", Text: map[string]Text{"en": {Title: "Atomic hidden deletion"}}})
	version := f.read("staff", "main.test", id)[0].Versions[0].ID
	before := len(readEventLog(t, f))
	if err := f.s.DeleteVersions(f.ctx, f.as("staff"), []VersionKey{{ContentID: id, VersionID: version}, {ContentID: id, VersionID: "missing"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("batch with missing edition: %v", err)
	}
	assertParentCause(t, f, id, StateDraft, false)
	if got := f.read("staff", "main.test", id)[0].Versions[0]; got.State == StateDeleted || len(readEventLog(t, f)) != before {
		t.Fatal("failed batch left deletion or retained events behind")
	}
}

func sameTime(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}

func TestConcurrentLastVersionDeletionsConverge(t *testing.T) {
	f := newFixture(t, nil)
	id := f.create("staff", gallery("Concurrent edition deletion", "en", "ja"))
	f.publish("staff", id, nil)
	versions := f.read("staff", "main.test", id)[0].Versions
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, len(versions))
	for i, version := range versions {
		wg.Add(1)
		go func(i int, version Version) {
			defer wg.Done()
			<-start
			errs[i] = f.s.DeleteVersions(f.ctx, f.as("staff"), []VersionKey{{ContentID: id, VersionID: version.ID}})
		}(i, version)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertParentCause(t, f, id, StateDeleted, true)
	if _, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionInput: VersionInput{Languages: []string{"en"}}}}); err != nil {
		t.Fatal("adding an edition to an automatically deleted parent:", err)
	}
	assertParentCause(t, f, id, StatePublished, false)
}

func TestEmptyParentRetentionAlsoPurgesPurchasedContent(t *testing.T) {
	f := newFixture(t, nil)
	plain := f.create("staff", gallery("Never sold", "en"))
	sold := f.create("staff", gallery("Purchased", "en"))
	for _, id := range []string{plain, sold} {
		version := f.read("staff", "main.test", id)[0].Versions[0].ID
		if err := f.s.DeleteVersions(f.ctx, f.as("staff"), []VersionKey{{ContentID: id, VersionID: version}}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := f.s.PurgeDeleted(f.ctx); err != nil || n != 0 {
		t.Fatalf("purged inside retention: %d %v", n, err)
	}
	f.advance(30*24*time.Hour + time.Hour)
	if n, err := f.s.PurgeDeleted(f.ctx); err != nil || n != 2 {
		t.Fatalf("expired parent purge: %d %v", n, err)
	}
	var retained int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM `+f.table("content_versions")+` WHERE tenant_id=$1 AND content_id=$2`, tenant, sold).Scan(&retained); err != nil || retained != 0 {
		t.Fatalf("purchased edition was retained: %d %v", retained, err)
	}
}

func TestImportedLastVersionDeletionFollowsParent(t *testing.T) {
	for _, kind := range []string{"gallery", "blog"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, nil)
			in := gallery("Imported deletion", "en")
			if kind == "blog" {
				in = ContentInput{Kind: kind, Text: map[string]Text{"en": {Title: "Imported deletion"}}}
			}
			id := f.create("staff", in)
			version := f.read("staff", "main.test", id)[0].Versions[0].ID
			if err := pgx.BeginFunc(f.ctx, f.pool, func(tx pgx.Tx) error {
				return f.s.ImportDeletes(f.ctx, tx, nil, []string{version})
			}); err != nil {
				t.Fatal(err)
			}
			assertParentCause(t, f, id, StateDeleted, true)
		})
	}
}

func TestImportedParentDeletionSupersedesEmptyCause(t *testing.T) {
	f := newFixture(t, nil)
	id := f.create("staff", gallery("Imported parent deletion", "en"))
	version := f.read("staff", "main.test", id)[0].Versions[0].ID
	if err := f.s.DeleteVersions(f.ctx, f.as("staff"), []VersionKey{{ContentID: id, VersionID: version}}); err != nil {
		t.Fatal(err)
	}
	assertParentCause(t, f, id, StateDeleted, true)
	if err := pgx.BeginFunc(f.ctx, f.pool, func(tx pgx.Tx) error {
		return f.s.ImportDeletes(f.ctx, tx, []string{id}, nil)
	}); err != nil {
		t.Fatal(err)
	}
	assertParentCause(t, f, id, StateDeleted, false)
	if _, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionID: version, VersionInput: VersionInput{State: StatePublished}}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("edition restore bypassed imported parent deletion: %v", err)
	}
}

func TestAutomaticParentRestoreDoesNotBypassModerationOrOwnerDeletion(t *testing.T) {
	f := newFixture(t, nil)
	owner := f.entity("channel", "owner")
	in := gallery("Owner and moderation", "en")
	in.OwnerEntityID = owner
	id := f.create("staff", in)
	version := f.read("staff", "main.test", id)[0].Versions[0].ID
	if err := f.s.DeleteVersions(f.ctx, f.as("staff"), []VersionKey{{ContentID: id, VersionID: version}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE `+f.table("content_entities")+` SET state='deleted' WHERE tenant_id=$1 AND id=$2`, tenant, owner); err != nil {
		t.Fatal(err)
	}
	restore := func() error {
		_, err := f.s.PutVersions(f.ctx, f.as("staff"), []VersionPut{{ContentID: id, VersionID: version, VersionInput: VersionInput{State: StatePublished}}})
		return err
	}
	if err := restore(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restored content beneath a deleted owner: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE `+f.table("content_entities")+` SET state='active' WHERE tenant_id=$1 AND id=$2`, tenant, owner); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteForModeration(f.ctx, f.as("staff"), []string{id}); err != nil {
		t.Fatal("moderation did not supersede automatic deletion:", err)
	}
	assertParentCause(t, f, id, StateDeleted, false)
	if err := restore(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restored content beneath an independent moderation deletion: %v", err)
	}
}

func assertParentCause(t *testing.T, f *fixture, id, wantState string, wantAutomatic bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	var state string
	var automatic bool
	if err := f.pool.QueryRow(ctx, `SELECT state,deleted_without_versions FROM `+f.table("content_items")+` WHERE tenant_id=$1 AND id=$2`, tenant, id).Scan(&state, &automatic); err != nil {
		t.Fatal(err)
	}
	if state != wantState || automatic != wantAutomatic {
		t.Fatalf("parent=%s automatic=%t, want %s/%t", state, automatic, wantState, wantAutomatic)
	}
}
