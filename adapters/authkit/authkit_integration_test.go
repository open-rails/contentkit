package authkit_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// signedIn is a request context as AuthKit's gate leaves it.
func (w *world) signedIn(t *testing.T, u authtest.User) context.Context {
	t.Helper()
	var ctx context.Context
	gate := verify.Required(w.auth)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { ctx = r.Context() }))
	r := httptest.NewRequest(http.MethodGet, "https://doujins.test/upload", nil).WithContext(t.Context())
	r.Header.Set("Authorization", "Bearer "+authtest.SignIn(t, w.auth, u).AccessToken)
	gate.ServeHTTP(httptest.NewRecorder(), r)
	if ctx == nil {
		t.Fatal("the gate refused the sign-in")
	}
	return ctx
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
	stored, err := w.auth.Verify(ctx, authtest.SignIn(t, w.auth, w.alice).AccessToken)
	if err != nil {
		t.Fatal(err)
	}
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
		{"claims stored outside a gate", verify.SetClaims(ctx, stored), actor(w.alice), avatarOf(w.alice.ID), false, false},
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
	if _, err := w.auth.RevokeAccountSessions(ctx, iam.SystemIdentity(), w.alice.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.auth.Start(ctx); err != nil {
		t.Fatal(err)
	}
	results, err := w.auth.PurgeUsers(ctx, []string{w.bob.ID})
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatalf("purge: %+v %v", results, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := w.auth.User(ctx, iam.UserByID(w.bob.ID), ak.IncludeDeleted())
		if errors.Is(err, iam.ErrUserNotFound) {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("wait for account purge: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		user authtest.User
	}{
		{"revoked session", aliceCtx, w.alice},
		{"purged account", bobCtx, w.bob},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := avatars.CanUpload(tc.ctx, actor(tc.user), avatarOf(tc.user.ID))
			if err != nil || g != (media.UploadGrant{}) {
				t.Fatalf("stale claims: %+v %v", g, err)
			}
		})
	}
	if g, err := (&ckauthkit.Avatars{Directory: w.auth}).CanUpload(bossCtx, actor(w.boss), avatarOf(w.boss.ID)); err != nil || !g.Allowed || g.Exempt || g.Owner != w.boss.ID {
		t.Fatalf("active owner without Staff: %+v %v", g, err)
	}
	canceledCtx, cancel := context.WithCancel(bossCtx)
	cancel()
	if g, err := avatars.CanUpload(canceledCtx, actor(w.boss), avatarOf(w.boss.ID)); !errors.Is(err, context.Canceled) || g != (media.UploadGrant{}) {
		t.Fatalf("failed session check: %+v %v", g, err)
	}
}

type down struct{ ckauthkit.Directory }

func (down) PublicUsers(context.Context, []string) (map[string]iam.PublicUser, error) {
	return nil, errors.New("directory down")
}

type images struct {
	reg  *media.Registry
	byID map[string][]media.PublicImage
	err  error
}

func (s images) Registry() *media.Registry { return s.reg }

func (s images) PublicImages(_ context.Context, ref contentref.ContentRef) ([]media.PublicImage, error) {
	if _, err := s.reg.Item(ref); err != nil {
		return nil, err
	}
	return s.byID[ref.ContentID], s.err
}

// Authors gives comment authors their names and currently published avatar.
func TestAuthors(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	reg := registry(t)
	unknown := contentref.NewID()
	base := "https://media.doujins.test/v1/accounts/user/" + w.alice.ID + "/public/avatar-"
	suffix := "-0192a3b4-c5d6-7e8f-9a0b-1c2d3e4f5a6b.webp"
	store := images{reg: reg, byID: map[string][]media.PublicImage{w.alice.ID: {
		{Preset: "avatar", Renditions: []media.PublicRendition{
			{URL: base + "64" + suffix, W: 64, H: 64},
			{URL: base + "128" + suffix, W: 100, H: 100},
			{URL: base + "256" + suffix, W: 100, H: 100},
		}},
	}}}
	got, err := (&ckauthkit.Authors{Directory: w.auth, Media: store}).UsersByIDs(ctx, []string{w.alice.ID, unknown})
	if err != nil {
		t.Fatal(err)
	}
	a := got[w.alice.ID]
	if a.Username != w.alice.Username || a.Avatar != base+"64"+suffix || a.AvatarSrcSet != base+"64"+suffix+" 64w, "+base+"128"+suffix+" 100w" {
		t.Fatalf("alice %+v", a)
	}
	if u := got[unknown]; u.ID != unknown || !strings.HasPrefix(u.Username, "user-") || u.Avatar != "" || u.AvatarSrcSet != "" {
		t.Fatalf("unknown %+v", u)
	}
	got, err = (&ckauthkit.Authors{Directory: w.auth, Media: store, Width: 100}).UsersByIDs(ctx, []string{w.alice.ID})
	if err != nil || got[w.alice.ID].Avatar != base+"128"+suffix {
		t.Fatalf("width 100: %+v %v", got, err)
	}
	// A directory outage never fails a listing.
	got, err = (&ckauthkit.Authors{Directory: down{w.auth}, Media: store}).UsersByIDs(ctx, []string{w.alice.ID})
	if err != nil || len(got) != 1 || !strings.HasPrefix(got[w.alice.ID].Username, "user-") {
		t.Fatalf("outage: %+v %v", got, err)
	}
	store.err = errors.New("media down")
	got, err = (&ckauthkit.Authors{Directory: w.auth, Media: store}).UsersByIDs(ctx, []string{w.alice.ID})
	if err != nil || got[w.alice.ID].Username != w.alice.Username || got[w.alice.ID].Avatar != "" || got[w.alice.ID].AvatarSrcSet != "" {
		t.Fatalf("media outage: %+v %v", got, err)
	}
}
