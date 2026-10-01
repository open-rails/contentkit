package authkit_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	ak "github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/verify"

	"github.com/open-rails/contentkit/access"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
)

const mediaURL = "https://media.doujins.test"

// links is a host Reader's SlotLink on mediaURL.
type links struct{}

func (links) SlotLink(ref contentref.ContentRef, slot string, width int, version string) string {
	u := mediaURL + "/" + ref.TenantID + "/" + ref.ContentKind + "/" + ref.ContentID + "/public/" + slot + "-" +
		strconv.Itoa(media.AvatarSlot.Rung(width)) + ".webp"
	if version != "" {
		u += "?v=" + version
	}
	return u
}

func set(ref contentref.ContentRef, version string) media.SlotChange {
	return media.SlotChange{Ref: ref, Slot: media.AvatarSlotName, Version: version}
}

type world struct {
	auth  *ak.Client
	staff iam.Perm
	alice authtest.User
	bob   authtest.User
	boss  authtest.User
}

func newWorld(t *testing.T) *world {
	t.Helper()
	rbac := ak.NewRoles()
	manage := rbac.Root.Permission("content", "manage")
	staff := rbac.Root.Role("staff", manage)
	auth, _ := authtest.New(t, authtest.WithConfig(func(c *ak.Config) { c.Roles = rbac }))
	w := &world{auth: auth, staff: manage, alice: authtest.NewUser(t, auth), bob: authtest.NewUser(t, auth), boss: authtest.NewUser(t, auth)}
	authtest.GrantRole(t, auth, iam.RootGroup(), iam.UserSubject(w.boss.ID), staff)
	return w
}

// signedIn is a request context as AuthKit's middleware leaves it.
func (w *world) signedIn(t *testing.T, u authtest.User) context.Context {
	t.Helper()
	cl, err := w.auth.Verify(t.Context(), authtest.SignIn(t, w.auth, u).AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	return verify.SetClaims(t.Context(), cl)
}

func (w *world) metadata(t *testing.T, id string) map[string]any {
	t.Helper()
	users, err := w.auth.PublicUsers(t.Context(), []string{id})
	if err != nil {
		t.Fatal(err)
	}
	return users[id].PublicMetadata
}

func avatarOf(id string) media.UploadTarget {
	return media.UploadTarget{Ref: contentref.New("doujins", media.UserKind, id), Slot: media.AvatarSlotName}
}

// A user changes only their own avatar, staff anyone's; the account's public
// metadata names the avatar's stable link once it is set and keeps it.
func TestAvatars(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	avatars := &ckauthkit.Avatars{Directory: w.auth, Links: links{}, Staff: w.staff}
	actor := func(u authtest.User) access.Actor { return access.Actor{ID: u.ID, Kind: "user"} }
	aliceCtx, bobCtx, bossCtx := w.signedIn(t, w.alice), w.signedIn(t, w.bob), w.signedIn(t, w.boss)

	for _, tc := range []struct {
		name    string
		ctx     context.Context
		actor   access.Actor
		target  media.UploadTarget
		allowed bool
		exempt  bool
	}{
		{"own avatar", aliceCtx, actor(w.alice), avatarOf(w.alice.ID), true, false},
		{"another user's avatar", bobCtx, actor(w.bob), avatarOf(w.alice.ID), false, false},
		{"staff, anyone's avatar", bossCtx, actor(w.boss), avatarOf(w.alice.ID), true, true},
		{"a file in one's own folder", aliceCtx, actor(w.alice), media.UploadTarget{Ref: avatarOf(w.alice.ID).Ref}, false, false},
		{"another slot of one's own folder", aliceCtx, actor(w.alice), media.UploadTarget{Ref: avatarOf(w.alice.ID).Ref, Slot: "banner"}, false, false},
		{"another kind's avatar slot", aliceCtx, actor(w.alice), media.UploadTarget{Ref: contentref.New("doujins", "artist", w.alice.ID), Slot: media.AvatarSlotName}, false, false},
		{"no verified claims", ctx, actor(w.alice), avatarOf(w.alice.ID), false, false},
		{"claims of another user", bobCtx, actor(w.alice), avatarOf(w.alice.ID), false, false},
		{"anonymous", ctx, access.Actor{Anonymous: true}, avatarOf(w.alice.ID), false, false},
	} {
		g, err := avatars.CanUpload(tc.ctx, tc.actor, tc.target)
		if err != nil || g.Allowed != tc.allowed || g.Exempt != tc.exempt {
			t.Errorf("%s: %+v %v, want allowed %v exempt %v", tc.name, g, err, tc.allowed, tc.exempt)
		}
		if tc.allowed && !tc.exempt && g.Owner != tc.actor.ID {
			t.Errorf("%s: quota owner %q", tc.name, g.Owner)
		}
	}
	// Without Staff nobody else may.
	if g, err := (&ckauthkit.Avatars{Directory: w.auth, Links: links{}}).CanUpload(bossCtx, actor(w.boss), avatarOf(w.alice.ID)); err != nil || g.Allowed {
		t.Fatalf("staff without Staff: %+v %v", g, err)
	}

	alice := avatarOf(w.alice.ID).Ref
	link := links{}.SlotLink(alice, media.AvatarSlotName, 256, "v1")
	if err := avatars.SlotChanged(ctx, nil, set(alice, "v1")); err != nil {
		t.Fatal(err)
	}
	if got := w.metadata(t, w.alice.ID)["avatar"]; got != link || !strings.HasSuffix(link, "/public/avatar-256.webp?v=v1") {
		t.Fatalf("public_metadata.avatar = %v, want %s", got, link)
	}
	// Idempotent; a replacement names the new version; other slots pass.
	if err := avatars.SlotChanged(ctx, nil, set(alice, "v1")); err != nil {
		t.Fatal(err)
	}
	if err := avatars.SlotChanged(ctx, nil, set(alice, "v2")); err != nil {
		t.Fatal(err)
	}
	if got := w.metadata(t, w.alice.ID)["avatar"]; got != links.SlotLink(links{}, alice, media.AvatarSlotName, 256, "v2") {
		t.Fatalf("replaced public_metadata.avatar = %v", got)
	}
	bob := avatarOf(w.bob.ID).Ref
	if err := avatars.SlotChanged(ctx, nil, media.SlotChange{Ref: bob, Slot: "banner", Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.metadata(t, w.bob.ID)["avatar"]; ok {
		t.Fatal("another slot wrote the avatar")
	}
	// A removal clears this site's avatar, so clients show their default.
	if err := avatars.SlotChanged(ctx, nil, set(alice, "")); err != nil {
		t.Fatal(err)
	}
	if got, ok := w.metadata(t, w.alice.ID)["avatar"]; ok {
		t.Fatalf("removed avatar still named: %v", got)
	}
	// Another site's avatar stays named.
	other := "https://media.hentai0.test/hentai0/user/" + w.bob.ID + "/public/avatar-256.webp?v=h1"
	if err := w.auth.PatchPublicMetadata(ctx, iam.SystemActor(), w.bob.ID, map[string]any{"avatar": other}); err != nil {
		t.Fatal(err)
	}
	if err := avatars.SlotChanged(ctx, nil, set(bob, "")); err != nil {
		t.Fatal(err)
	}
	if got := w.metadata(t, w.bob.ID)["avatar"]; got != other {
		t.Fatalf("a removal here cleared another site's avatar: %v", got)
	}
	// The key and width are the host's; an unknown account is done.
	if err := (&ckauthkit.Avatars{Directory: w.auth, Links: links{}, Key: "picture", Width: 64}).SlotChanged(ctx, nil, set(bob, "v3")); err != nil {
		t.Fatal(err)
	}
	if got := w.metadata(t, w.bob.ID)["picture"]; got != links.SlotLink(links{}, bob, media.AvatarSlotName, 64, "v3") {
		t.Fatalf("public_metadata.picture = %v", got)
	}
	if err := avatars.SlotChanged(ctx, nil, set(avatarOf(contentref.NewID()).Ref, "v1")); err != nil {
		t.Fatalf("unknown account: %v", err)
	}
}

type down struct{ ckauthkit.Directory }

func (down) PublicUsers(context.Context, []string) (map[string]iam.PublicUser, error) {
	return nil, errors.New("directory down")
}

// Authors gives comment authors their names and the avatar their public
// metadata names; anything but an http(s) link is ignored.
func TestAuthors(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	alice := avatarOf(w.alice.ID).Ref
	if err := (&ckauthkit.Avatars{Directory: w.auth, Links: links{}}).SlotChanged(ctx, nil, set(alice, "v1")); err != nil {
		t.Fatal(err)
	}
	if err := w.auth.PatchPublicMetadata(ctx, iam.SystemActor(), w.bob.ID, map[string]any{"avatar": "javascript:alert(1)"}); err != nil {
		t.Fatal(err)
	}
	unknown := contentref.NewID()
	authors := &ckauthkit.Authors{Directory: w.auth}
	got, err := authors.UsersByIDs(ctx, []string{w.alice.ID, w.bob.ID, unknown})
	if err != nil {
		t.Fatal(err)
	}
	link := links{}.SlotLink(alice, media.AvatarSlotName, 256, "v1")
	a := got[w.alice.ID]
	if a.Username != w.alice.Username || a.Avatar != links.SlotLink(links{}, alice, media.AvatarSlotName, 64, "v1") ||
		a.AvatarSrcSet != media.AvatarSlot.LinkSrcSet(link) || !strings.Contains(a.AvatarSrcSet, "/avatar-512.webp?v=v1 512w") {
		t.Fatalf("alice %+v", a)
	}
	if b := got[w.bob.ID]; b.Username != w.bob.Username || b.Avatar != "" || b.AvatarSrcSet != "" {
		t.Fatalf("bob's unsafe avatar was used: %+v", b)
	}
	if u := got[unknown]; u.ID != unknown || !strings.HasPrefix(u.Username, "user-") || u.Avatar != "" {
		t.Fatalf("unknown %+v", u)
	}
	got, err = (&ckauthkit.Authors{Directory: w.auth, Width: 128}).UsersByIDs(ctx, []string{w.alice.ID})
	if err != nil || got[w.alice.ID].Avatar != links.SlotLink(links{}, alice, media.AvatarSlotName, 128, "v1") {
		t.Fatalf("width 128: %+v %v", got, err)
	}

	// A directory outage never fails a listing.
	got, err = (&ckauthkit.Authors{Directory: down{w.auth}}).UsersByIDs(ctx, []string{w.alice.ID})
	if err != nil || len(got) != 1 || got[w.alice.ID].Avatar != "" || !strings.HasPrefix(got[w.alice.ID].Username, "user-") {
		t.Fatalf("outage: %+v %v", got, err)
	}
}
