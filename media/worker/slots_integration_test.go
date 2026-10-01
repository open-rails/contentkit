package worker_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

// selfAvatar is a host's account-avatar policy: users write their own avatar
// slot and nothing else.
type selfAvatar struct{}

func (selfAvatar) CanUpload(_ context.Context, a access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	own := t.Ref.ContentKind == media.UserKind && t.Ref.ContentID == a.ID && t.Slot == media.AvatarSlotName
	return media.UploadGrant{Allowed: own, Owner: a.ID}, nil
}

func (h *host) avatar(t *testing.T, user contentref.ContentRef, width int) media.Picture {
	t.Helper()
	pics, err := h.reader.SlotImages(context.Background(), h.Tenant, media.UserKind, media.AvatarSlotName, width, user.ContentID)
	if err != nil {
		t.Fatal(err)
	}
	return pics[user.ContentID]
}

// object returns an object's bytes and ETag; ok is false when it is absent.
func (h *host) object(t *testing.T, key string) ([]byte, string, bool) {
	t.Helper()
	rc, obj, err := h.Store.Get(context.Background(), key, media.GetOptions{})
	if errors.Is(err, media.ErrNotFound) {
		return nil, "", false
	} else if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b, obj.ETag, true
}

// fixedKeys are the public/ keys of the avatar preset's rungs.
func fixedKeys(item media.Item) []string {
	var keys []string
	for _, w := range media.AvatarSlot.Widths {
		keys = append(keys, item.PublicPrefix()+media.AvatarSlotName+"-"+strconv.Itoa(w)+".webp")
	}
	return keys
}

// An account avatar end to end: the worker renders it to fixed public names,
// the slot index lists it once the host's hook succeeds, a replacement
// overwrites the same names (purged, same URLs), only the user may change it,
// removal deletes the names, and erasure drops the row.
func TestAvatarSlotIndexLinksAndRemoval(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	user := contentref.New(h.Tenant, media.UserKind, newID())
	item, _ := h.kinds.Item(user)
	owner := access.Actor{ID: user.ContentID, Kind: "user"}
	self, err := media.NewUploads(media.UploadOptions{Store: h.Store, Kinds: h.kinds, Manifests: h.manifests, Authorizer: selfAvatar{}, Queue: h.queue})
	if err != nil {
		t.Fatal(err)
	}
	upload := func(seed uint8) {
		t.Helper()
		body := pngImage(t, 300, 300, seed)
		sum := sha256.Sum256(body)
		p, err := self.Presign(ctx, owner, media.PresignRequest{Ref: user, Type: "image/png", Size: int64(len(body)), SHA256: sum[:], Slot: media.AvatarSlotName})
		if err != nil {
			t.Fatal(err)
		}
		if p.Put != nil {
			h.put(t, p.Put, body)
		}
		if err := self.CommitSlot(ctx, owner, media.SlotCommit{Ref: user, Slot: media.AvatarSlotName, SHA256: sum[:]}); err != nil {
			t.Fatal(err)
		}
	}
	keys := fixedKeys(item)
	h.mu.Lock()
	h.failChanges = 1 // the first SlotChanged fails: the row waits for the retried job
	h.mu.Unlock()
	upload(41)
	eventually(t, "the avatar's row", time.Minute, func() bool { return h.avatar(t, user, 100).URL != "" })
	rec, err := h.manifests.Slot(ctx, user, media.AvatarSlotName)
	if err != nil {
		t.Fatal(err)
	}
	set := user.String() + "#avatar set"
	if got := h.slotChanges(); !slices.Equal(got, []string{set}) {
		t.Fatalf("SlotChanged %v, want one set after the retry", got)
	}

	// Fixed names, each a copy of its private rendition; the manifest records them.
	pic := h.avatar(t, user, 100)
	if want := mediaURL + "/" + keys[1]; pic.URL != want || pic.W != 128 || pic.H != 128 {
		t.Fatalf("avatar at 100 px: %+v, want %s", pic, want)
	}
	if link := h.reader.SlotLink(user, media.AvatarSlotName, 100); link != pic.URL {
		t.Fatalf("SlotLink %q, want %q", link, pic.URL)
	}
	if n := strings.Count(pic.SrcSet, "w,") + 1; n != 4 || !strings.Contains(pic.SrcSet, "/avatar-512.webp 300w") {
		t.Fatalf("srcset %q", pic.SrcSet)
	}
	etags := map[string]string{}
	for i, o := range rec.Result.Outputs {
		if o.Public != media.AvatarSlotName+"-"+strconv.Itoa(o.Rung)+".webp" {
			t.Fatalf("output %d records public name %q", i, o.Public)
		}
		private, _ := item.Private(o.Blob)
		want, _, _ := h.object(t, private)
		got, etag, ok := h.object(t, keys[i])
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("%s is not a copy of %s", keys[i], private)
		}
		etags[keys[i]] = etag
	}

	// A replacement overwrites the same names and purges them; URLs stay.
	upload(42)
	eventually(t, "the replaced row", time.Minute, func() bool { return len(h.slotChanges()) == 2 })
	if got := h.slotChanges(); got[1] != set || h.avatar(t, user, 100) != pic {
		t.Fatalf("replaced: SlotChanged %v, avatar %+v", got, h.avatar(t, user, 100))
	}
	for _, key := range keys {
		if _, etag, ok := h.object(t, key); !ok || etag == etags[key] {
			t.Fatalf("%s was not overwritten", key)
		}
	}
	eventually(t, "the replaced names purged", 10*time.Second, func() bool {
		purged := h.purgedKeys()
		return !slices.ContainsFunc(keys, func(k string) bool { return !slices.Contains(purged, k) })
	})

	// Only the user writes their folder, and only the avatar slot.
	bob := access.Actor{ID: newID(), Kind: "user"}
	if err := self.DeleteSlot(ctx, bob, user, media.AvatarSlotName); !isCode(err, media.CodeForbidden) {
		t.Fatalf("another user's removal: %v", err)
	}
	sum := sha256.Sum256([]byte("page"))
	if _, err := self.Presign(ctx, owner, media.PresignRequest{Ref: user, Type: "image/png", Size: 4, SHA256: sum[:]}); !isCode(err, media.CodeForbidden) {
		t.Fatalf("a file in the user's folder: %v", err)
	}

	// Removal: the names are deleted (nothing at the URL), the row goes, the
	// hook hears it.
	before := len(h.purgedKeys())
	if err := self.DeleteSlot(ctx, owner, user, media.AvatarSlotName); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the removed avatar", time.Minute, func() bool {
		if h.avatar(t, user, 100).URL != "" {
			return false
		}
		for _, key := range keys {
			if _, _, ok := h.object(t, key); ok {
				return false
			}
		}
		return true
	})
	if got := h.slotChanges(); got[len(got)-1] != user.String()+"#avatar clear" {
		t.Fatalf("SlotChanged %v", got)
	}
	if purged := h.purgedKeys()[before:]; !slices.Contains(purged, keys[0]) {
		t.Fatalf("removal purged %v", purged)
	}
	if err := self.DeleteSlot(ctx, owner, user, media.AvatarSlotName); err != nil {
		t.Fatalf("removing an unset avatar: %v", err)
	}

	// Erasing the account drops its row in the host's transaction; the folder
	// deletion keeps it dropped.
	upload(43)
	eventually(t, "the new row", time.Minute, func() bool { return h.avatar(t, user, 100).URL != "" })
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.jobs.EraseUserTx(ctx, tx, h.Tenant, user.ContentID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the erased account's folder and row", time.Minute, func() bool {
		_, _, ok := h.object(t, keys[0])
		return !ok && h.avatar(t, user, 100).URL == ""
	})
}

// Hiding an item deletes its fixed public names and takes its slots out of
// the index; unhiding brings both back.
func TestHiddenItemLeavesTheSlotIndex(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	work := contentref.New(h.Tenant, "gallery", newID())
	item, _ := h.kinds.Item(work)
	h.upload(t, work, "cover", "image/png", pngImage(t, 600, 200, 51))
	cover := item.PublicPrefix() + "cover-300.webp"
	listed := func() bool {
		pics, err := h.reader.SlotImages(ctx, h.Tenant, "gallery", "cover", 300, work.ContentID)
		if err != nil {
			t.Fatal(err)
		}
		_, _, public := h.object(t, cover)
		return pics[work.ContentID].URL != "" && public
	}
	eventually(t, "the cover's row", time.Minute, listed)
	h.hidden.Store(work.Key(), true)
	if err := h.jobs.Expose(ctx, work); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the hidden cover leaving public/ and the index", time.Minute, func() bool {
		pics, err := h.reader.SlotImages(ctx, h.Tenant, "gallery", "cover", 300, work.ContentID)
		if err != nil {
			t.Fatal(err)
		}
		_, _, public := h.object(t, cover)
		return pics[work.ContentID].URL == "" && !public
	})
	if !slices.Contains(h.purgedKeys(), cover) {
		t.Fatalf("hiding purged %v", h.purgedKeys())
	}
	h.hidden.Delete(work.Key())
	if err := h.jobs.Expose(ctx, work); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the unhidden cover's row", time.Minute, listed)
	got := h.slotChanges()
	if set, clear := work.String()+"#cover set", work.String()+"#cover clear"; !slices.Equal(got, []string{set, clear, set}) {
		t.Fatalf("SlotChanged %v", got)
	}
}

func isCode(err error, code string) bool {
	ue, ok := media.AsUploadError(err)
	return ok && ue.Code == code
}

// idle waits until neither the host's River nor the worker's has a job
// waiting or running (jobs scheduled for later, such as sweeps, aside).
func (h *host) idle(t *testing.T) {
	t.Helper()
	eventually(t, "idle queues", time.Minute, func() bool {
		var n int
		for _, schema := range []string{h.schema, h.workers} {
			var m int
			if err := h.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+pgx.Identifier{schema, "river_job"}.Sanitize()+
				" WHERE state IN ('available', 'running', 'retryable', 'pending')").Scan(&m); err != nil {
				t.Fatal(err)
			}
			n += m
		}
		return n == 0
	})
}

// An upgrade: the slot index starts empty and the fixed public names absent
// beside slots set before they existed. A host replica starting (its media
// jobs bound to River) backfills both at once, without the daily sweep,
// records completion, and a later start schedules no backfill.
func TestSlotIndexBackfillsAfterAnUpgrade(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	work := contentref.New(h.Tenant, "gallery", newID())
	user := contentref.New(h.Tenant, media.UserKind, newID())
	h.upload(t, work, "cover", "image/png", pngImage(t, 600, 200, 61))
	h.upload(t, user, media.AvatarSlotName, "image/png", pngImage(t, 200, 200, 62))
	workItem, _ := h.kinds.Item(work)
	userItem, _ := h.kinds.Item(user)
	fixed := []string{workItem.PublicPrefix() + "cover-150.webp", userItem.PublicPrefix() + "avatar-64.webp"}
	listed := func() int {
		t.Helper()
		n := 0
		for _, c := range []struct{ kind, slot, id string }{{"gallery", "cover", work.ContentID}, {media.UserKind, media.AvatarSlotName, user.ContentID}} {
			pics, err := h.reader.SlotImages(ctx, h.Tenant, c.kind, c.slot, 100, c.id)
			if err != nil {
				t.Fatal(err)
			}
			n += len(pics)
		}
		for _, key := range fixed {
			if _, _, ok := h.object(t, key); ok {
				n++
			}
		}
		return n
	}
	marker := pgx.Identifier{h.content, "content_media_slot_backfill"}.Sanitize()
	completed := func() bool {
		var done bool
		err := h.pool.QueryRow(ctx, "SELECT completed_at IS NOT NULL FROM "+marker+" WHERE tenant_id = $1", h.Tenant).Scan(&done)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		return done
	}
	backfills := func() int {
		var n int
		if err := h.pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{h.schema, "river_job"}.Sanitize()+
			" WHERE kind = 'contentkit_media_slot_backfill'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	eventually(t, "both slots public and indexed, and the first start's backfill done", time.Minute, func() bool { return listed() == 4 && completed() })
	h.idle(t)

	// Before the upgrade: no rows, no fixed names and no backfill record.
	for _, table := range []string{"content_media_slots", "content_media_slot_backfill"} {
		if _, err := h.pool.Exec(ctx, "DELETE FROM "+pgx.Identifier{h.content, table}.Sanitize()); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range fixed {
		if err := h.Store.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	if n := listed(); n != 0 {
		t.Fatalf("%d slots listed before the upgrade", n)
	}
	before := backfills()
	h.startJobs(t) // the upgraded replica starts
	eventually(t, "the backfill", 30*time.Second, func() bool { return listed() == 4 && completed() })
	if n := backfills(); n != before+1 {
		t.Fatalf("%d backfill jobs scheduled at start, want 1", n-before)
	}
	h.idle(t)
	h.startJobs(t) // another replica: the backfill is done
	if n := backfills(); n != before+1 {
		t.Fatalf("a completed backfill was scheduled again (%d jobs)", n-before)
	}
}
