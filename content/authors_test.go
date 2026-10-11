package content

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/open-rails/helpers/userinfo"

	"github.com/open-rails/contentkit/access"
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

// A listing shows its authors with one directory read and one avatar query:
// names and usernames from the directory, never its email; the avatar from
// media; an account the directory no longer holds without an author. An
// outage leaves bare ids and never fails the listing.
func TestAuthorsResolveInTwoCalls(t *testing.T) {
	dir := &countingDirectory{}
	rt, m := newMediaTest(t, Options{UserInfo: dir, Resolver: resolverWith("gallery", cid(1)), ContentKinds: []string{"gallery"}})
	m.publish("avatar-alice")
	for _, who := range []string{"alice", "bob", "gone", "alice"} {
		mustComment(t, rt, access.Actor{ID: who, Kind: "user"}, "gallery", cid(1), CommentInput{Body: "from " + who})
	}
	dir.gets.Store(0)
	m.mu.Lock()
	m.avatarLookups = 0
	m.mu.Unlock()

	var list []Comment
	if code := send(t, rt, access.Actor{Anonymous: true, IP: "10.0.0.1"}, "GET", "/gallery/"+cid(1)+"/comments?sort=newest", nil, &list); code != http.StatusOK || len(list) != 4 {
		t.Fatalf("list: %d %d", code, len(list))
	}
	m.mu.Lock()
	avatars := m.avatarLookups
	m.mu.Unlock()
	if dir.gets.Load() != 1 || avatars != 1 {
		t.Fatalf("%d directory reads, %d avatar queries; want one each", dir.gets.Load(), avatars)
	}
	by := map[string]*PublicUser{}
	for _, c := range list {
		by[c.UserID] = c.Author
	}
	a := by["alice"]
	if a == nil || a.Name != "Name alice" || a.Username != "name-alice" || !strings.HasSuffix(a.Avatar, "-64.webp") || !strings.Contains(a.AvatarSrcSet, "-128.webp 128w") {
		t.Fatalf("alice %+v", a)
	}
	if b := by["bob"]; b == nil || b.Username != "name-bob" || b.Avatar != "" || b.AvatarSrcSet != "" {
		t.Fatalf("bob %+v", b)
	}
	if g := by["gone"]; g != nil {
		t.Fatalf("an account the directory does not hold: %+v", g)
	}

	dir.down = true
	list = nil
	if code := send(t, rt, access.Actor{Anonymous: true, IP: "10.0.0.1"}, "GET", "/gallery/"+cid(1)+"/comments", nil, &list); code != http.StatusOK || len(list) != 4 {
		t.Fatalf("outage: %d %d", code, len(list))
	}
	for _, c := range list {
		if c.Author != nil || c.UserID == "" {
			t.Fatalf("outage author %+v", c)
		}
	}
}
