package content

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/userinfo"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/avatar"
)

// countingDirectory counts Get calls and can fail them.
type countingDirectory struct {
	commentsDirectory
	gets atomic.Int32
	down bool
}

func (d *countingDirectory) Get(ctx context.Context, ids []string) (map[string]userinfo.User, error) {
	d.gets.Add(1)
	if d.down {
		return nil, errors.New("directory down")
	}
	return d.commentsDirectory.Get(ctx, ids)
}

// A listing shows its authors with one directory read and no avatar
// lookup: names and usernames from the directory, never its email; every
// person's avatar at its stable URL (the gateway serves the default there
// until one is uploaded); an account the directory no longer holds without
// an author. An outage leaves bare ids and never fails the listing.
func TestAuthorsResolveInOneCall(t *testing.T) {
	dir := &countingDirectory{}
	avatars, err := media.NewRegistry(media.Config{Namespace: "site", BaseURL: "https://media.test", Kinds: []media.Kind{avatar.User()},
		Avatars: media.Avatars{Kind: "user", Preset: "avatar"}})
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := newMediaTest(t, Options{UserInfo: dir, Avatars: avatars, Resolver: resolverWith("gallery", cid(1)), Kinds: testKinds("gallery")})
	alice, bob := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	for _, who := range []string{alice, bob, "gone", alice} {
		mustComment(t, rt, access.Actor{ID: who, Kind: "user"}, "gallery", cid(1), CommentInput{Body: "from " + who})
	}
	dir.gets.Store(0)

	var list []Comment
	if code := send(t, rt, access.Actor{Anonymous: true, IP: "10.0.0.1"}, "GET", "/contents/"+cid(1)+"/comments?sort=newest", nil, &list); code != http.StatusOK || len(list) != 4 {
		t.Fatalf("list: %d %d", code, len(list))
	}
	if dir.gets.Load() != 1 {
		t.Fatalf("%d directory reads; want one", dir.gets.Load())
	}
	by := map[string]*PublicUser{}
	for _, c := range list {
		by[c.UserID] = c.Author
	}
	for _, id := range []string{alice, bob} {
		u, base := by[id], "https://media.test/v1/accounts/user/"+id+"/public/"
		if u == nil || u.Name != "Name "+id || u.Username != "name-"+id || u.Avatar != base+"avatar-64.webp" || !strings.Contains(u.AvatarSrcSet, base+"avatar-128.webp 128w") {
			t.Fatalf("%s %+v", id, u)
		}
	}
	if g := by["gone"]; g != nil {
		t.Fatalf("an account the directory does not hold: %+v", g)
	}

	dir.down = true
	list = nil
	if code := send(t, rt, access.Actor{Anonymous: true, IP: "10.0.0.1"}, "GET", "/contents/"+cid(1)+"/comments", nil, &list); code != http.StatusOK || len(list) != 4 {
		t.Fatalf("outage: %d %d", code, len(list))
	}
	for _, c := range list {
		if c.Author != nil || c.UserID == "" {
			t.Fatalf("outage author %+v", c)
		}
	}
}
