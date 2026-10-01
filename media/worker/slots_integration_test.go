package worker_test

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

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

const avatarDefault = "https://app.example/static/avatar.svg"

func (h *host) avatar(t *testing.T, user contentref.ContentRef, width int) media.Picture {
	t.Helper()
	pics, err := h.reader.SlotImages(context.Background(), h.Tenant, media.UserKind, media.AvatarSlotName, width, user.ContentID)
	if err != nil {
		t.Fatal(err)
	}
	return pics[user.ContentID]
}

// link GETs a slot link through the read API.
func (h *host) link(t *testing.T, path string, withDefault bool) *httptest.ResponseRecorder {
	t.Helper()
	o := media.HandlerOptions{Tenant: h.Tenant}
	if withDefault {
		o.SlotDefault = func(kind, slot string) string {
			if kind == media.UserKind && slot == media.AvatarSlotName {
				return avatarDefault
			}
			return ""
		}
	}
	rec := httptest.NewRecorder()
	h.reader.Handler(o).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// An account avatar end to end: the worker renders it, the host's slot index
// job lists it once its hook succeeds, the stable link follows every change,
// only the user may change it, removal falls back to the default, and erasure
// drops the row with the account.
func TestAvatarSlotIndexLinksAndRemoval(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	user := contentref.New(h.Tenant, media.UserKind, newID())
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
	h.mu.Lock()
	h.failChanges = 1 // the first SlotChanged fails: the row waits for the retried job
	h.mu.Unlock()
	upload(41)
	eventually(t, "the avatar's row", time.Minute, func() bool { return h.avatar(t, user, 100).URL != "" })
	set := user.String() + "#" + media.AvatarSlotName + " set"
	if got := h.slotChanges(); !slices.Equal(got, []string{set}) {
		t.Fatalf("SlotChanged %v, want one set after the retry", got)
	}
	m, err := h.manifests.SlotManifest(ctx, media.OutputURLs{BaseURL: mediaURL}, user, media.AvatarSlotName)
	if err != nil || len(m.Outputs) != 4 {
		t.Fatalf("avatar manifest %+v %v", m, err)
	}
	first := h.avatar(t, user, 100)
	if first.URL != m.Outputs[1].URL || first.W != 128 || first.H != 128 {
		t.Fatalf("avatar at 100 px: %+v, outputs %+v", first, m.Outputs)
	}
	if n := strings.Count(first.SrcSet, "w,") + 1; n != 4 { // the 512 rung is capped at the 300 px upload
		t.Fatalf("srcset %q", first.SrcSet)
	}
	if key, ok := strings.CutPrefix(first.URL, mediaURL+"/"); !ok || !h.exists(t, key) {
		t.Fatalf("listed URL %s is not a public object", first.URL)
	}

	// The stable link redirects to the current image.
	link := h.reader.SlotLink(user, media.AvatarSlotName)
	path, ok := strings.CutPrefix(link, appURL)
	if !ok || path != "/user/"+user.ContentID+"/slots/avatar/image" {
		t.Fatalf("SlotLink %q", link)
	}
	if rec := h.link(t, path+"?w=100", true); rec.Code != http.StatusFound || rec.Header().Get("Location") != first.URL ||
		!strings.HasPrefix(rec.Header().Get("Cache-Control"), "public, max-age=60") {
		t.Fatalf("link?w=100: %d %v", rec.Code, rec.Header())
	}
	if rec := h.link(t, path, false); rec.Code != http.StatusFound || rec.Header().Get("Location") != m.Outputs[3].URL {
		t.Fatalf("link without w: %d %s, want the widest %s", rec.Code, rec.Header().Get("Location"), m.Outputs[3].URL)
	}
	for p, code := range map[string]int{path + "?w=0": http.StatusBadRequest, path + "?w=x": http.StatusBadRequest,
		"/user/" + user.ContentID + "/slots/cover/image": http.StatusNotFound, "/gallery/" + user.ContentID + "/slots/avatar/image": http.StatusNotFound} {
		if rec := h.link(t, p, true); rec.Code != code {
			t.Fatalf("GET %s: %d, want %d", p, rec.Code, code)
		}
	}
	if got := media.AvatarSlot.LinkSrcSet(link); !strings.HasPrefix(got, link+"?w=64 64w, ") || !strings.HasSuffix(got, link+"?w=512 512w") {
		t.Fatalf("LinkSrcSet %q", got)
	}

	// A new image replaces the row; the link follows.
	upload(42)
	eventually(t, "the replaced row", time.Minute, func() bool { u := h.avatar(t, user, 100).URL; return u != "" && u != first.URL })
	if rec := h.link(t, path+"?w=100", true); rec.Header().Get("Location") != h.avatar(t, user, 100).URL {
		t.Fatalf("link after replace: %s", rec.Header().Get("Location"))
	}

	// Only the user writes their folder, and only the avatar slot.
	bob := access.Actor{ID: newID(), Kind: "user"}
	if err := self.DeleteSlot(ctx, bob, user, media.AvatarSlotName); !isCode(err, media.CodeForbidden) {
		t.Fatalf("another user's removal: %v", err)
	}
	sum := sha256.Sum256([]byte("page"))
	if _, err := self.Presign(ctx, owner, media.PresignRequest{Ref: user, Type: "image/png", Size: 4, SHA256: sum[:]}); !isCode(err, media.CodeForbidden) {
		t.Fatalf("a file in the user's folder: %v", err)
	}

	// Removal: the row goes, the hook hears it, the link falls back.
	if err := self.DeleteSlot(ctx, owner, user, media.AvatarSlotName); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the removed row", time.Minute, func() bool { return h.avatar(t, user, 100).URL == "" })
	clear := user.String() + "#" + media.AvatarSlotName + " clear"
	if got := h.slotChanges(); !slices.Equal(got, []string{set, set, clear}) {
		t.Fatalf("SlotChanged %v", got)
	}
	if rec := h.link(t, path, true); rec.Code != http.StatusFound || rec.Header().Get("Location") != avatarDefault {
		t.Fatalf("link without an avatar: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := h.link(t, path, false); rec.Code != http.StatusNotFound {
		t.Fatalf("link without an avatar or default: %d", rec.Code)
	}
	if err := self.DeleteSlot(ctx, owner, user, media.AvatarSlotName); err != nil {
		t.Fatalf("removing an unset avatar: %v", err)
	}

	// Erasing the account drops its row in the host's transaction; the folder
	// deletion keeps it dropped.
	upload(43)
	var last media.Picture
	eventually(t, "the new row", time.Minute, func() bool { last = h.avatar(t, user, 100); return last.URL != "" })
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
	key, _ := strings.CutPrefix(last.URL, mediaURL+"/")
	eventually(t, "the erased account's folder and row", time.Minute, func() bool {
		return !h.exists(t, key) && h.avatar(t, user, 100).URL == ""
	})
}

// Hiding an item takes its slots out of the index (and its links to the
// default); unhiding brings them back.
func TestHiddenItemLeavesTheSlotIndex(t *testing.T) {
	h := newHost(t)
	ctx := context.Background()
	work := contentref.New(h.Tenant, "gallery", newID())
	h.upload(t, work, "cover", "image/png", pngImage(t, 600, 200, 51))
	listed := func() bool {
		pics, err := h.reader.SlotImages(ctx, h.Tenant, "gallery", "cover", 300, work.ContentID)
		if err != nil {
			t.Fatal(err)
		}
		return pics[work.ContentID].URL != ""
	}
	eventually(t, "the cover's row", time.Minute, listed)
	h.hidden.Store(work.Key(), true)
	if err := h.jobs.Expose(ctx, work); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the hidden cover leaving the index", time.Minute, func() bool { return !listed() })
	if rec := h.link(t, "/gallery/"+work.ContentID+"/slots/cover/image", true); rec.Code != http.StatusNotFound {
		t.Fatalf("hidden cover link: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	h.hidden.Delete(work.Key())
	if err := h.jobs.Expose(ctx, work); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the unhidden cover's row", time.Minute, listed)
	cover := work.String() + "#cover"
	if got := h.slotChanges(); !slices.Equal(got, []string{cover + " set", cover + " clear", cover + " set"}) {
		t.Fatalf("SlotChanged %v", got)
	}
}

func isCode(err error, code string) bool {
	ue, ok := media.AsUploadError(err)
	return ok && ue.Code == code
}
