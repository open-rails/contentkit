package authkit_test

import (
	"context"
	"errors"
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

// registry is a site importing the shared account kind.
func registry(t *testing.T) *media.Registry {
	t.Helper()
	images := []string{"image/png", "image/jpeg", "image/webp"}
	r, err := media.NewRegistry(media.Config{Namespace: "doujins", BaseURL: "https://media.doujins.test", Kinds: []media.Kind{
		{Name: "user", Namespace: "accounts", KeepOriginals: true,
			Uploads: []media.Upload{{Path: "avatar", Types: images, MaxBytes: 10 << 20}},
			Public: []media.Public{{Name: "avatar", From: "avatar", To: "avatar-{w}.webp", Widths: []int{64, 128, 256, 512},
				Image: media.Image{Aspect: media.Square, Quality: 85}, Default: "avatar.png"}}},
		{Name: "artist", Uploads: []media.Upload{{Path: "avatar", Types: images, MaxBytes: 10 << 20}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A user changes only their own account's media, staff anyone's.
func TestAvatars(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	avatars := &ckauthkit.Avatars{Directory: w.auth, Staff: w.staff}
	actor := func(u authtest.User) access.Actor { return access.Actor{ID: u.ID, Kind: "user"} }
	aliceCtx, bobCtx, bossCtx := w.signedIn(t, w.alice), w.signedIn(t, w.bob), w.signedIn(t, w.boss)
	avatarOf := func(id string) media.UploadTarget {
		return media.UploadTarget{Ref: contentref.New("accounts", "user", id), Path: "avatar"}
	}
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
		{"another kind", aliceCtx, actor(w.alice), media.UploadTarget{Ref: contentref.New("doujins", "artist", w.alice.ID), Path: "avatar"}, false, false},
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
	if g, err := (&ckauthkit.Avatars{Directory: w.auth}).CanUpload(bossCtx, actor(w.boss), avatarOf(w.alice.ID)); err != nil || g.Allowed {
		t.Fatalf("staff without Staff: %+v %v", g, err)
	}
}

type down struct{ ckauthkit.Directory }

func (down) PublicUsers(context.Context, []string) (map[string]iam.PublicUser, error) {
	return nil, errors.New("directory down")
}

// Authors gives comment authors their names and their avatar's fixed URL.
func TestAuthors(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	reg := registry(t)
	unknown := contentref.NewID()
	got, err := (&ckauthkit.Authors{Directory: w.auth, Media: reg}).UsersByIDs(ctx, []string{w.alice.ID, unknown})
	if err != nil {
		t.Fatal(err)
	}
	base := "https://media.doujins.test/v1/accounts/user/" + w.alice.ID + "/public/avatar-"
	a := got[w.alice.ID]
	if a.Username != w.alice.Username || a.Avatar != base+"64.webp" || !strings.HasPrefix(a.AvatarSrcSet, base+"64.webp 64w, ") || !strings.HasSuffix(a.AvatarSrcSet, base+"512.webp 512w") {
		t.Fatalf("alice %+v", a)
	}
	if u := got[unknown]; u.ID != unknown || !strings.HasPrefix(u.Username, "user-") || !strings.Contains(u.Avatar, unknown) {
		t.Fatalf("unknown %+v", u)
	}
	got, err = (&ckauthkit.Authors{Directory: w.auth, Media: reg, Width: 100}).UsersByIDs(ctx, []string{w.alice.ID})
	if err != nil || got[w.alice.ID].Avatar != base+"128.webp" {
		t.Fatalf("width 100: %+v %v", got, err)
	}
	// A directory outage never fails a listing.
	got, err = (&ckauthkit.Authors{Directory: down{w.auth}, Media: reg}).UsersByIDs(ctx, []string{w.alice.ID})
	if err != nil || len(got) != 1 || !strings.HasPrefix(got[w.alice.ID].Username, "user-") {
		t.Fatalf("outage: %+v %v", got, err)
	}
}
