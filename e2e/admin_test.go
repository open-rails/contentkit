package e2e

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/media"
)

// A user page reads the person's site-wide comment ban: none (404), the ban
// with its reason, end and banned user, expired once its end passed, and none
// again once lifted. Owner-scope bans are not it.
func TestGlobalCommentBanOfUser(t *testing.T) {
	w := newWorld(t)
	staffUser, staff := w.person(w.staff)
	o, owner := w.person(iam.Role{})
	u, user := w.person(iam.Role{})
	w.owner = o.ID
	f := serve(t, w.runtime(nil))
	target := "/global-comment-bans/" + u.ID

	f.want(http.StatusNotFound, staff, "GET", target, nil, nil)
	f.want(http.StatusOK, owner, "PUT", "/comment-bans/"+u.ID, content.BanInput{Reason: "rude"}, nil)
	f.want(http.StatusNotFound, staff, "GET", target, nil, nil)
	f.want(http.StatusForbidden, user, "GET", target, nil, nil)
	f.want(http.StatusUnauthorized, "", "GET", target, nil, nil)

	until := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	f.want(http.StatusOK, staff, "PUT", target, content.BanInput{Reason: "spam links", Until: &until}, nil)
	var ban content.CommentBan
	f.want(http.StatusOK, staff, "GET", target, nil, &ban)
	if ban.UserID != u.ID || ban.Scope != content.ScopeGlobal || ban.Reason != "spam links" || ban.Until == nil || !ban.Until.Equal(until) ||
		ban.BannedBy != staffUser.ID || ban.Expired || ban.User == nil || ban.User.Username != u.Username {
		t.Fatalf("ban %+v", ban)
	}

	if _, err := w.pool.Exec(t.Context(), `UPDATE `+pgx.Identifier{w.schema, "content_comment_bans"}.Sanitize()+`
		SET until = now() - interval '1 minute' WHERE user_id = $1 AND scope = 'global'`, u.ID); err != nil {
		t.Fatal(err)
	}
	f.want(http.StatusOK, staff, "GET", target, nil, &ban)
	if !ban.Expired {
		t.Fatalf("expired ban %+v", ban)
	}

	f.want(http.StatusNoContent, staff, "DELETE", target, nil, nil)
	f.want(http.StatusNotFound, staff, "GET", target, nil, nil)
}

// A user page lists the person's comments for moderators: theirs only,
// newest first, deleted ones with their text, paged, and by content kind.
func TestAdminCommentsByAuthor(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	o, owner := w.person(iam.Role{})
	u, user := w.person(iam.Role{})
	w.owner = o.ID
	f := serve(t, w.runtime(nil))
	item := "/gallery/" + cid(1)

	var first, reply, gone content.Comment
	f.want(http.StatusCreated, user, "POST", item+"/comments", content.CommentInput{Body: "one"}, &first)
	f.want(http.StatusCreated, owner, "POST", item+"/comments", content.CommentInput{Body: "the owner's"}, nil)
	f.want(http.StatusCreated, user, "POST", item+"/comments", content.CommentInput{Body: "two", ReplyToID: first.ID}, &reply)
	f.want(http.StatusCreated, user, "POST", item+"/comments", content.CommentInput{Body: "three"}, &gone)
	f.want(http.StatusNoContent, user, "DELETE", "/comments/"+gone.ID, nil, nil)

	list := func(q url.Values) []content.AdminComment {
		t.Helper()
		var out []content.AdminComment
		f.want(http.StatusOK, staff, "GET", "/comments/admin?"+q.Encode(), nil, &out)
		return out
	}
	ids := func(cs []content.AdminComment) string {
		s := ""
		for _, c := range cs {
			s += c.Body + ","
		}
		return s
	}
	all := list(url.Values{"author": {u.ID}})
	if ids(all) != "three,two,one," {
		t.Fatalf("by author: %s", ids(all))
	}
	for _, c := range all {
		if c.UserID != u.ID || c.Author == nil || c.Author.Username != u.Username || c.ContentKind != "gallery" || c.ContentID != cid(1) {
			t.Fatalf("comment %s by %s (%+v) on %s/%s", c.ID, c.UserID, c.Author, c.ContentKind, c.ContentID)
		}
	}
	if !all[0].Deleted || all[0].DeletedAt == nil || all[1].ReplyToID != first.ID {
		t.Fatalf("deleted and reply %+v %+v", all[0], all[1])
	}
	page1 := list(url.Values{"author": {u.ID}, "limit": {"2"}})
	page2 := list(url.Values{"author": {u.ID}, "limit": {"2"}, "offset": {"2"}})
	if ids(page1) != "three,two," || ids(page2) != "one," {
		t.Fatalf("pages %s | %s", ids(page1), ids(page2))
	}
	if got := list(url.Values{"author": {u.ID}, "content_kind": {"gallery"}}); len(got) != 3 {
		t.Fatalf("by author and kind: %s", ids(got))
	}
	if got := list(url.Values{"author": {u.ID}, "content_kind": {"video"}}); len(got) != 0 {
		t.Fatalf("by author on another kind: %s", ids(got))
	}
	if got := list(url.Values{"author": {uuid.NewString()}}); len(got) != 0 {
		t.Fatalf("by a stranger: %s", ids(got))
	}
	if got := list(nil); len(got) != 4 {
		t.Fatalf("everyone's: %s", ids(got))
	}
	f.want(http.StatusForbidden, user, "GET", "/comments/admin?author="+u.ID, nil, nil)
}

// A group's page lists its memberships: content- and version-level, every
// role and state, paged by cursor in member order.
func TestGroupMembers(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	_, user := w.person(iam.Role{})
	f := serve(t, w.runtime(nil))
	w.galleries(1, 2, 3)

	var made []groups.Group
	f.want(http.StatusCreated, staff, "POST", "/v1/groups", []groups.GroupInput{
		{Kind: "tag", Slug: "color", Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Color"}}},
		{Kind: "tag", Slug: "empty", Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Empty"}}},
	}, &made)
	color, empty := string(made[0].ID), string(made[1].ID)
	f.want(http.StatusOK, staff, "POST", "/v1/groups/"+color+"/members", []groups.Membership{
		{ContentID: cid(3)}, {ContentID: cid(1)}, {ContentID: cid(2), VersionID: cid(20)}, {ContentID: cid(2), State: groups.MembershipProposed},
		{ContentID: cid(1), Role: "featured"},
	}, nil)

	page := func(id string, q url.Values) groups.MemberPage {
		t.Helper()
		var p groups.MemberPage
		f.want(http.StatusOK, staff, "GET", "/v1/groups/"+id+"/members?"+q.Encode(), nil, &p)
		return p
	}
	key := func(ms []groups.Membership) string {
		s := ""
		for _, m := range ms {
			s += m.ContentID[len(m.ContentID)-2:]
			if m.VersionID != "" {
				s += "@" + m.VersionID[len(m.VersionID)-2:]
			}
			if m.Role != "" {
				s += ":" + m.Role
			}
			if m.State == groups.MembershipProposed {
				s += "?"
			}
			s += " "
		}
		return s
	}
	want := "01 01:featured 02? 02@20 03 "
	var walked string
	cursor := ""
	for i := 0; ; i++ {
		p := page(color, url.Values{"limit": {"2"}, "cursor": {cursor}})
		walked += key(p.Members)
		if cursor = p.NextCursor; cursor == "" {
			if i != 2 {
				t.Fatalf("%d pages", i+1)
			}
			break
		}
	}
	if walked != want {
		t.Fatalf("walked\n %s\nwant\n %s", walked, want)
	}
	if got := key(page(color, nil).Members); got != want {
		t.Fatalf("one page: %s", got)
	}
	if p := page(empty, nil); len(p.Members) != 0 || p.NextCursor != "" {
		t.Fatalf("empty group: %+v", p)
	}
	f.want(http.StatusNotFound, staff, "GET", "/v1/groups/"+uuid.Must(uuid.NewV7()).String()+"/members", nil, nil)
	f.want(http.StatusBadRequest, staff, "GET", "/v1/groups/not-an-id/members", nil, nil)
	f.want(http.StatusBadRequest, staff, "GET", "/v1/groups/"+color+"/members?cursor=%21", nil, nil)
	f.want(http.StatusForbidden, user, "GET", "/v1/groups/"+color+"/members", nil, nil)
}

// An item's editor (a writer, not exempt) regenerates its media: an upload
// that failed for its current blob is retried, its presets pending again and
// a processing job queued. Forcing every output stays staff's.
func TestEditorRegeneratesMedia(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	_, editor := w.person(iam.Role{})
	f := serve(t, w.runtime(nil))
	ref := media.RefBody{Kind: "gallery", ID: cid(1)}
	item, err := w.reg.Ref(ref.Kind, ref.ID)
	if err != nil {
		t.Fatal(err)
	}

	plan := upload(t, f, editor, ref, "cover", pngBytes(t, 16))
	f.want(http.StatusOK, editor, "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
		Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}}, nil)
	// The worker places the upload, then its producer fails.
	if _, err := w.jobs.Manifests().Place(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, err := w.jobs.Manifests().EditExisting(ctx, item, func(m *media.Manifest) error {
		i := m.Find(plan.Path)
		if i < 0 || m.Files[i].Blob == "" {
			return fmt.Errorf("cover not placed: %+v", m.Files)
		}
		m.Files[i].Failed, m.Files[i].Pending = media.NewFailure(m.Files[i], errors.New("vips: unreadable")), nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	jobs := func() int {
		var n int
		if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{w.queue.Schema(), "river_job"}.Sanitize()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := jobs()

	var reply media.CommitReply
	f.want(http.StatusOK, editor, "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
		Ops: []media.Op{{Op: media.OpRegenerate}}}, &reply)
	if len(reply.Files) != 1 || reply.Files[0].Failed != nil || len(reply.Files[0].Pending) != 1 || reply.Files[0].Pending[0] != "cover" {
		t.Fatalf("regenerated %+v", reply)
	}
	m, _, err := w.jobs.Manifests().Get(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	if f := m.Files[m.Find(plan.Path)]; f.Failed != nil || len(f.Pending) != 1 {
		t.Fatalf("manifest %+v", f)
	}
	if after := jobs(); after <= before {
		t.Fatalf("no processing queued: %d jobs, %d before", after, before)
	}

	f.want(http.StatusForbidden, editor, "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
		Ops: []media.Op{{Op: media.OpRegenerate, Force: true}}}, nil)
	f.want(http.StatusUnauthorized, "", "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
		Ops: []media.Op{{Op: media.OpRegenerate}}}, nil)
}

// The poll manager lists polls by state and question: live, scheduled,
// closed and inactive ones, each only where it belongs.
func TestAdminPollsByStateAndSearch(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	_, user := w.person(iam.Role{})
	f := serve(t, w.runtime(nil))
	create := func(question string, liveAt *time.Time) content.Poll {
		var p content.Poll
		f.want(http.StatusCreated, staff, "POST", "/polls", content.PollInput{Question: question, Language: "en", LiveAt: liveAt,
			Options: []content.PollOptionInput{{Label: "Yes"}, {Label: "No", Position: 1}}}, &p)
		return p
	}
	create("Alpha, live?", nil)
	create("Beta, scheduled?", ptr(time.Now().Add(24*time.Hour)))
	gamma := create("Gamma, inactive?", nil)
	f.want(http.StatusOK, staff, "PATCH", "/polls/"+gamma.ID, content.PollUpdate{IsActive: ptr(false)}, nil)
	delta := create("Delta, closed?", nil)
	if _, err := w.pool.Exec(t.Context(), `UPDATE `+pgx.Identifier{w.schema, "content_poll_questions"}.Sanitize()+`
		SET closes_at = now() - interval '1 minute' WHERE id = $1`, delta.ID); err != nil {
		t.Fatal(err)
	}

	list := func(q url.Values) string {
		t.Helper()
		var out []content.Poll
		f.want(http.StatusOK, staff, "GET", "/polls/admin?"+q.Encode(), nil, &out)
		s := ""
		for _, p := range out {
			s += p.Question + " "
		}
		return s
	}
	for state, want := range map[string]string{"live": "Alpha, live? ", "scheduled": "Beta, scheduled? ", "inactive": "Gamma, inactive? ", "closed": "Delta, closed? "} {
		if got := list(url.Values{"state": {state}}); got != want {
			t.Errorf("state %s: %q", state, got)
		}
	}
	if got := list(url.Values{"q": {"ALPHA"}}); got != "Alpha, live? " {
		t.Errorf("search: %q", got)
	}
	if got := list(url.Values{"q": {"a, "}, "state": {"inactive"}}); got != "Gamma, inactive? " {
		t.Errorf("search and state: %q", got)
	}
	if got := list(url.Values{"language": {"ja"}}); got != "" {
		t.Errorf("another language: %q", got)
	}
	if got := list(nil); got != "Beta, scheduled? Delta, closed? Gamma, inactive? Alpha, live? " {
		t.Errorf("every poll: %q", got)
	}
	f.want(http.StatusBadRequest, staff, "GET", "/polls/admin?state=open", nil, nil)
	f.want(http.StatusForbidden, user, "GET", "/polls/admin?state=live", nil, nil)
}
