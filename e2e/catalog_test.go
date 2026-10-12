package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/helpers/notify"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/contract"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/media"
)

// One mount serves every module at its sub-path, and every route answers
// what the catalog declares: each response of this scenario is checked
// against its route, and every route of the catalog is reached.
func TestHandlerServesTheCatalogIntegration(t *testing.T) {
	w := newWorld(t)
	_, staff := w.person(w.staff)
	o, owner := w.person(iam.Role{})
	u, user := w.person(iam.Role{})
	w.owner = o.ID
	anon := ""
	f := serve(t, w.runtime(nil))
	item := "/gallery/" + cid(1)

	// What signed-out visitors may do: nothing here (the default).
	var cfg content.Config
	f.want(http.StatusOK, anon, "GET", "/config", nil, &cfg)
	if cfg.Anonymous != (content.Anonymous{}) {
		t.Fatalf("config %+v", cfg)
	}

	// Contents: a staff member creates, writes, publishes and schedules; a
	// person without content:write is refused; everyone reads what is live.
	var post catalog.Content
	f.want(http.StatusUnauthorized, anon, "POST", "/v1/contents", catalog.ContentInput{Kind: "blog"}, nil)
	f.want(http.StatusForbidden, user, "POST", "/v1/contents", catalog.ContentInput{Kind: "blog", Text: map[string]catalog.Text{"en": {Title: "Hello"}}}, nil)
	f.want(http.StatusCreated, staff, "POST", "/v1/contents", catalog.ContentInput{Kind: "blog", Text: map[string]catalog.Text{"en": {Title: "Hello", Body: "World"}}}, &post)
	f.want(http.StatusNotFound, anon, "GET", "/v1/contents/"+post.ID, nil, nil)
	f.want(http.StatusOK, staff, "PUT", "/v1/contents/"+post.ID+"/text/en", catalog.Text{Title: "Hello", Description: "Short", Body: "World"}, nil)
	f.want(http.StatusOK, staff, "PATCH", "/v1/contents/"+post.ID, catalog.ContentPatch{Attributes: map[string]any{}}, nil)
	f.want(http.StatusOK, staff, "PUT", "/v1/contents/"+post.ID+"/creators", catalog.CreatorsInput{Creators: []catalog.Creator{{UserID: o.ID}}}, nil)
	f.want(http.StatusNotImplemented, staff, "POST", "/v1/contents/"+post.ID+"/images", content.ImageInput{Image: ptr("i-" + uuid.NewString())}, nil)
	f.want(http.StatusBadRequest, staff, "POST", "/v1/contents/"+post.ID+"/versions", catalog.VersionInput{Languages: []string{"en"}}, nil)
	f.want(http.StatusOK, staff, "POST", "/v1/contents/"+post.ID+"/publish", catalog.PublishInput{}, &post)
	f.want(http.StatusOK, anon, "GET", "/v1/contents/"+post.ID+"?language=en", nil, &post)
	if !post.Live || post.Title != "Hello" || post.Editor || len(post.Versions) != 1 || post.Code == "" {
		t.Fatalf("published %+v", post)
	}
	f.want(http.StatusOK, anon, "GET", "/v1/contents/"+post.ID+"/versions/"+post.Versions[0].ID, nil, nil)
	f.want(http.StatusOK, owner, "GET", "/v1/manage/contents?kind=blog&state=published", nil, nil)
	var managed []catalog.Content
	f.want(http.StatusOK, staff, "GET", "/v1/manage/contents?q=hello", nil, &managed)
	if len(managed) != 1 || managed[0].ID != post.ID {
		t.Fatalf("managed %+v", managed)
	}
	f.want(http.StatusOK, staff, "POST", "/v1/contents/"+post.ID+"/unpublish", nil, nil)
	f.want(http.StatusNotFound, anon, "GET", "/v1/contents/"+post.ID, nil, nil)
	f.want(http.StatusOK, staff, "POST", "/v1/contents/"+post.ID+"/publish", catalog.PublishInput{}, nil)

	// An album has versions in order, with segments; one stays.
	var album catalog.Content
	f.want(http.StatusCreated, staff, "POST", "/v1/contents", catalog.ContentInput{Kind: "album", Text: map[string]catalog.Text{"en": {Title: "Album"}},
		Versions: []catalog.VersionInput{{Languages: []string{"en"}}}}, &album)
	var second catalog.Version
	f.want(http.StatusCreated, staff, "POST", "/v1/contents/"+album.ID+"/versions", catalog.VersionInput{Languages: []string{"ja"}, Traits: &[]string{"colored"}}, &second)
	f.want(http.StatusOK, staff, "PATCH", "/v1/contents/"+album.ID+"/versions/"+second.ID, catalog.VersionInput{Label: ptr("Colored")}, nil)
	f.want(http.StatusOK, staff, "PUT", "/v1/contents/"+album.ID+"/versions/"+second.ID+"/segments", catalog.SegmentsInput{Segments: []catalog.Segment{{Start: 0, End: 10, Title: "One"}}}, nil)
	f.want(http.StatusOK, staff, "PUT", "/v1/contents/"+album.ID+"/versions/order", catalog.OrderInput{IDs: []string{second.ID, album.Versions[0].ID}}, nil)
	f.want(http.StatusNoContent, staff, "DELETE", "/v1/contents/"+album.ID+"/versions/"+album.Versions[0].ID, nil, nil)
	f.want(http.StatusConflict, staff, "DELETE", "/v1/contents/"+album.ID+"/versions/"+second.ID, nil, nil)
	f.want(http.StatusOK, staff, "PUT", "/v1/contents/"+album.ID+"/text/ja", catalog.Text{Title: "アルバム"}, nil)
	f.want(http.StatusOK, staff, "DELETE", "/v1/contents/"+album.ID+"/text/ja", nil, nil)

	// Codes resolve the item's code; its URL is gone once it is deleted.
	var resolved contenturl.Resolved
	f.want(http.StatusOK, anon, "GET", "/codes/"+strings.ToLower(post.Code)+"?lang=en", nil, &resolved)
	if resolved.ContentID != post.ID || resolved.Path != "/en/blog/"+post.Code+"/hello" {
		t.Fatalf("resolved %+v", resolved)
	}
	f.want(http.StatusBadRequest, anon, "GET", "/codes/not-a-code", nil, nil)
	f.want(http.StatusNotFound, anon, "GET", "/codes/ZZZZZZZZ9", nil, nil)
	f.want(http.StatusNoContent, staff, "DELETE", "/v1/contents/"+post.ID, nil, nil)
	f.want(http.StatusGone, anon, "GET", "/codes/"+post.Code, nil, nil)
	f.want(http.StatusNotFound, anon, "GET", "/v1/contents/"+post.ID, nil, nil)
	f.want(http.StatusOK, staff, "POST", "/v1/contents/"+post.ID+"/restore", nil, nil)
	f.want(http.StatusOK, anon, "GET", "/codes/"+post.Code, nil, nil)
	f.want(http.StatusNotFound, staff, "POST", "/v1/contents/"+post.ID+"/restore", nil, nil)

	// The catalog's totals, open to anyone.
	var totals catalog.Totals
	f.want(http.StatusOK, anon, "GET", "/v1/catalog/totals?kinds=blog&languages=en", nil, &totals)
	if b := totals.Kinds["blog"]; b.Public != 1 || b.Tiers["premium"] != 1 || b.Languages["en"].Public != 1 {
		t.Fatalf("totals %+v", totals)
	}
	f.want(http.StatusBadRequest, anon, "GET", "/v1/catalog/totals?kinds=post", nil, nil)

	// Comments.
	var root, reply content.Comment
	f.want(http.StatusCreated, user, "POST", item+"/comments", content.CommentInput{Body: "first"}, &root)
	f.want(http.StatusUnauthorized, anon, "POST", item+"/comments", content.CommentInput{Body: "who?", AnonName: "Guest"}, nil)
	f.want(http.StatusCreated, owner, "POST", item+"/comments", content.CommentInput{Body: "reply", ReplyToID: root.ID}, &reply)
	f.want(http.StatusOK, anon, "GET", item+"/comments?sort=best&limit=5", nil, nil)
	f.want(http.StatusOK, anon, "GET", "/comments/"+root.ID+"/replies", nil, nil)
	f.want(http.StatusOK, anon, "GET", "/comments/latest", nil, nil)
	f.want(http.StatusForbidden, user, "GET", "/comments/admin", nil, nil)
	f.want(http.StatusOK, staff, "GET", "/comments/admin?content_kind=gallery", nil, nil)
	f.want(http.StatusOK, user, "PATCH", "/comments/"+root.ID, content.CommentEdit{Body: "first, edited"}, nil)
	f.want(http.StatusForbidden, owner, "PATCH", "/comments/"+root.ID, content.CommentEdit{Body: "not yours"}, nil)
	for _, verb := range []string{"like", "dislike"} {
		f.want(http.StatusOK, owner, "POST", "/comments/"+root.ID+"/"+verb, nil, nil)
	}
	f.want(http.StatusOK, owner, "DELETE", "/comments/"+root.ID+"/reaction", nil, nil)
	f.want(http.StatusNoContent, owner, "DELETE", "/comments/"+reply.ID, nil, nil)
	f.want(http.StatusOK, staff, "POST", "/comments/"+reply.ID+"/restore", nil, nil)
	f.want(http.StatusNotFound, anon, "GET", "/gallery/"+cid(2)+"/comments", nil, nil)

	// Native reply/like notices use the same authenticated user and mount.
	var notices notify.Page
	f.want(http.StatusUnauthorized, anon, "GET", "/notifications/inbox", nil, nil)
	f.want(http.StatusOK, user, "GET", "/notifications/inbox?limit=20&unread=true", nil, &notices)
	if len(notices.Items) != 2 {
		t.Fatalf("native notifications = %+v", notices)
	}
	id := notices.Items[0].ID
	f.want(http.StatusNotFound, owner, "PUT", "/notifications/"+id+"/read", nil, nil)
	f.want(http.StatusOK, user, "PUT", "/notifications/"+id+"/read", nil, nil)
	f.want(http.StatusOK, user, "DELETE", "/notifications/"+id+"/read", nil, nil)
	f.want(http.StatusOK, user, "PUT", "/notifications/"+id+"/dismiss", nil, nil)
	f.want(http.StatusOK, user, "GET", "/notifications/inbox?dismissed=true", nil, nil)
	f.want(http.StatusOK, user, "DELETE", "/notifications/"+id+"/dismiss", nil, nil)

	// Reactions on host content.
	for _, verb := range []string{"like", "dislike"} {
		f.want(http.StatusOK, user, "POST", item+"/"+verb, nil, nil)
	}
	f.want(http.StatusOK, user, "DELETE", item+"/reaction", nil, nil)
	f.want(http.StatusOK, anon, "GET", item+"/reaction", nil, nil)

	// Favorites.
	f.want(http.StatusUnauthorized, anon, "POST", item+"/favorite", nil, nil)
	f.want(http.StatusOK, user, "POST", item+"/favorite", nil, nil)
	f.want(http.StatusOK, user, "GET", item+"/favorite", nil, nil)
	f.want(http.StatusOK, user, "GET", "/favorites", nil, nil)
	f.want(http.StatusOK, user, "DELETE", item+"/favorite", nil, nil)

	// Polls.
	var poll content.Poll
	f.want(http.StatusCreated, staff, "POST", "/polls", content.PollInput{Question: "Best?", Language: "en",
		Options: []content.PollOptionInput{{Label: "A"}, {Label: "B", Position: 1}}}, &poll)
	f.want(http.StatusNotImplemented, staff, "POST", "/polls", content.PollInput{Kind: content.PollFreeText, Question: "Why?", Language: "en"}, nil)
	f.want(http.StatusOK, anon, "GET", "/polls?language=en", nil, nil)
	f.want(http.StatusOK, staff, "GET", "/polls/admin", nil, nil)
	f.want(http.StatusOK, anon, "GET", "/polls/"+poll.ID, nil, nil)
	f.want(http.StatusOK, user, "POST", "/polls/"+poll.ID+"/vote", content.PollVote{OptionID: poll.Options[0].ID}, nil)
	f.want(http.StatusUnauthorized, anon, "POST", "/polls/"+poll.ID+"/answer", content.PollAnswerInput{Text: "because"}, nil)
	f.want(http.StatusOK, staff, "PATCH", "/polls/"+poll.ID, content.PollUpdate{Question: ptr("Best one?")}, nil)
	var option content.PollOption
	f.want(http.StatusCreated, staff, "POST", "/polls/"+poll.ID+"/options", content.PollOptionPatch{Label: ptr("C")}, &option)
	f.want(http.StatusOK, staff, "PATCH", "/polls/"+poll.ID+"/options/"+option.ID, content.PollOptionPatch{Position: ptr(5)}, nil)
	f.want(http.StatusNotImplemented, staff, "PUT", "/polls/"+poll.ID+"/image", content.ImageInput{Image: ptr("")}, nil)
	f.want(http.StatusNotImplemented, staff, "PUT", "/polls/"+poll.ID+"/options/"+option.ID+"/image", content.ImageInput{Image: ptr("")}, nil)
	f.want(http.StatusNoContent, staff, "DELETE", "/polls/"+poll.ID+"/options/"+option.ID, nil, nil)
	f.want(http.StatusBadRequest, staff, "DELETE", "/polls/"+poll.ID+"/options/"+poll.Options[0].ID, nil, nil)
	f.want(http.StatusNoContent, staff, "DELETE", "/polls/"+poll.ID, nil, nil)

	// Bans: the owner's own scope, and the tenant-wide one.
	f.want(http.StatusOK, owner, "PUT", "/comment-bans/"+u.ID, content.BanInput{Reason: "spam"}, nil)
	f.want(http.StatusOK, owner, "GET", "/comment-bans", nil, nil)
	var standing content.CommentStanding
	f.want(http.StatusOK, user, "GET", item+"/can-comment", nil, &standing)
	if standing.CanComment || standing.Ban == nil {
		t.Fatalf("banned standing %+v", standing)
	}
	f.want(http.StatusForbidden, user, "POST", item+"/comments", content.CommentInput{Body: "again"}, nil)
	f.want(http.StatusNoContent, owner, "DELETE", "/comment-bans/"+u.ID, nil, nil)
	f.want(http.StatusUnauthorized, anon, "GET", "/comment-bans", nil, nil)
	f.want(http.StatusOK, staff, "PUT", "/global-comment-bans/u1", nil, nil)
	f.want(http.StatusOK, staff, "GET", "/global-comment-bans/u1", nil, nil)
	f.want(http.StatusOK, staff, "GET", "/global-comment-bans", nil, nil)
	f.want(http.StatusNoContent, staff, "DELETE", "/global-comment-bans/u1", nil, nil)
	f.want(http.StatusForbidden, user, "GET", "/global-comment-bans", nil, nil)

	// Moderation.
	f.want(http.StatusOK, staff, "GET", "/moderation/held?kind=comment", nil, nil)
	f.want(http.StatusOK, staff, "GET", "/moderation/held?kind=content", nil, nil)
	f.want(http.StatusBadRequest, staff, "GET", "/moderation/held?kind=gallery", nil, nil)
	f.want(http.StatusNotFound, staff, "POST", "/moderation/comment/"+uuid.NewString()+"/resolve", content.ResolveInput{Decision: content.DecisionApprove, Revision: 1}, nil)
	f.want(http.StatusForbidden, user, "GET", "/moderation/held?kind=comment", nil, nil)

	// Groups: public reads, writes behind root:groups:write.
	w.galleries(1, 2)
	f.want(http.StatusForbidden, user, "POST", "/v1/groups", []groups.GroupInput{}, nil)
	name := func(n string) groups.Name { return groups.Name{Language: "en", Kind: groups.NameCanonical, Name: n} }
	var made []groups.Group
	f.want(http.StatusCreated, staff, "POST", "/v1/groups", []groups.GroupInput{
		{Kind: "tag", Slug: "color", Names: []groups.Name{name("Color")}},
		{Kind: "tag", Slug: "colour", Names: []groups.Name{name("Colour")}},
		{Kind: "series", Slug: "saga", Names: []groups.Name{name("Saga")}},
	}, &made)
	color, colour, saga := string(made[0].ID), string(made[1].ID), string(made[2].ID)
	f.want(http.StatusConflict, staff, "POST", "/v1/groups", []groups.GroupInput{{Kind: "tag", Slug: "color"}}, nil)
	var page groups.GroupPage
	f.want(http.StatusOK, anon, "GET", "/v1/groups?kind=tag&language=en&sort=name&limit=1", nil, &page)
	if len(page.Groups) != 1 || page.Groups[0].Name != "Color" || page.Groups[0].Path == "" || page.NextCursor == "" {
		t.Fatalf("groups page %+v", page)
	}
	f.want(http.StatusOK, anon, "GET", "/v1/groups/"+color+"?language=en", nil, nil)
	f.want(http.StatusNotFound, anon, "GET", "/v1/groups/"+uuid.Must(uuid.NewV7()).String(), nil, nil)
	f.want(http.StatusBadRequest, anon, "GET", "/v1/groups/not-an-id", nil, nil)
	f.want(http.StatusOK, staff, "PATCH", "/v1/groups/"+colour, groups.GroupUpdate{Slug: ptr("colour-uk")}, nil)
	f.want(http.StatusOK, staff, "POST", "/v1/groups/"+color+"/names", []groups.Name{{Language: "en", Kind: groups.NameAlias, Name: "Hue"}}, nil)
	f.want(http.StatusOK, staff, "DELETE", "/v1/groups/"+color+"/names", []groups.Name{{Language: "en", Name: "Hue"}}, nil)
	f.want(http.StatusOK, staff, "PUT", "/v1/groups/"+color+"/names", []groups.Name{name("Color")}, nil)
	edges := []groups.EdgeInput{{Relation: "related", To: groups.GroupID(colour)}}
	f.want(http.StatusOK, staff, "POST", "/v1/groups/"+color+"/edges", edges, nil)
	f.want(http.StatusOK, staff, "DELETE", "/v1/groups/"+color+"/edges", edges, nil)
	f.want(http.StatusOK, staff, "PUT", "/v1/contents/"+cid(1)+"/groups", groups.ContentGroups{Memberships: []groups.Membership{
		{GroupID: groups.GroupID(color)}, {GroupID: groups.GroupID(saga)}, {GroupID: groups.GroupID(colour), VersionID: cid(10)}}}, nil)
	var held groups.ContentMemberships
	f.want(http.StatusOK, staff, "GET", "/v1/contents/"+cid(1)+"/groups?language=en", nil, &held)
	if len(held.Memberships) != 3 || len(held.Groups) != 3 {
		t.Fatalf("content groups %+v", held)
	}
	f.want(http.StatusOK, staff, "POST", "/v1/groups/"+saga+"/members", []groups.Membership{{ContentID: cid(2)}}, nil)
	f.want(http.StatusOK, staff, "PUT", "/v1/groups/"+saga+"/members/order", groups.Order{Members: []groups.MemberRef{{ContentID: cid(2)}, {ContentID: cid(1)}}}, nil)
	var members groups.MemberPage
	f.want(http.StatusOK, staff, "GET", "/v1/groups/"+saga+"/members?limit=10", nil, &members)
	if len(members.Members) != 2 || members.Members[0].ContentID != cid(2) {
		t.Fatalf("members %+v", members)
	}
	f.want(http.StatusOK, staff, "DELETE", "/v1/groups/"+saga+"/members", []groups.Membership{{ContentID: cid(2)}}, nil)
	f.want(http.StatusOK, staff, "POST", "/v1/groups/"+colour+"/merge", groups.MergeInput{Into: groups.GroupID(color)}, nil)
	f.want(http.StatusOK, staff, "DELETE", "/v1/groups/"+saga, nil, nil)

	mediaScenario(t, f, user, anon)

	served := f.checker.Served()
	var missing []string
	for _, r := range httpapi.Catalog() {
		if !served[contract.RouteID(r)] {
			missing = append(missing, contract.RouteID(r))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("routes this scenario never reached:\n  %s", strings.Join(missing, "\n  "))
	}
}

func mediaScenario(t *testing.T, f *server, user, anon string) {
	ref := media.RefBody{Kind: "gallery", ID: cid(1)}
	body := pngBytes(t, 16)
	sum := sha256.Sum256(body)
	presign := func(path string) media.PresignBody { return presignPNG(ref, path, body) }
	upload := func(path string) media.PresignReply { return upload(t, f, user, ref, path, body) }
	commit := func(plan media.PresignReply) *httptest.ResponseRecorder {
		return f.call(user, "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
			Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}})
	}
	f.want(http.StatusUnauthorized, anon, "POST", "/media/upload/presign", presign("originals/001"), nil)
	if rec := commit(upload("originals/001")); rec.Code != http.StatusOK {
		t.Fatalf("commit: %d %s", rec.Code, rec.Body)
	}
	// too_many_files says how many the path holds.
	if rec := commit(upload("extras/a")); rec.Code != http.StatusOK {
		t.Fatalf("commit extras/a: %d %s", rec.Code, rec.Body)
	}
	var refused media.ErrorReply
	rec := commit(upload("extras/b"))
	if err := json.Unmarshal(rec.Body.Bytes(), &refused); err != nil || rec.Code != http.StatusConflict ||
		refused.Code != media.CodeTooManyFiles || refused.Details == nil || refused.Details.Max != 1 {
		t.Fatalf("second extra: %d %s", rec.Code, rec.Body)
	}
	f.want(http.StatusUnsupportedMediaType, user, "POST", "/media/upload/presign", media.PresignBody{Ref: ref, Path: "cover", Type: "image/gif", Size: 10, SHA256: hex.EncodeToString(sum[:])}, nil)
	for _, p := range []string{"/media/upload/parts/list", "/media/upload/complete", "/media/upload/abort"} {
		f.want(http.StatusBadRequest, user, "POST", p, media.TicketBody{Ticket: "forged"}, nil)
	}
	f.want(http.StatusBadRequest, user, "POST", "/media/upload/parts", media.PartsBody{Ticket: "forged", Parts: []media.PartBody{{Number: 1, Size: 1, SHA256: hex.EncodeToString(sum[:])}}}, nil)
	f.want(http.StatusNotFound, user, "GET", "/media/upload/frame?kind=gallery&id="+cid(1)+"&path=originals/001&t=1", nil, nil)

	// An editor read carries the kind's upload rules; a viewer's does not.
	var read media.ReadResult
	f.want(http.StatusOK, user, "GET", "/media/gallery/"+cid(1)+"?editor", nil, &read)
	if len(read.Files) != 2 || !read.Files[0].Upload || len(read.Uploads) != 3 {
		t.Fatalf("editor read %+v", read)
	}
	if r := read.Uploads[0]; r.Path != "originals/{name}" || r.MaxBytes != 1<<20 || r.Max != 10 || len(r.Types) != 1 || r.Video != nil {
		t.Fatalf("originals rule %+v", r)
	}
	if r := read.Uploads[1]; r.Path != "cover" || r.MinWidth != 8 {
		t.Fatalf("cover rule %+v", r)
	}
	var viewer media.ReadResult
	f.want(http.StatusOK, anon, "GET", "/media/gallery/"+cid(1)+"?limit=10&editor", nil, &viewer)
	if viewer.Uploads != nil {
		t.Fatalf("a viewer read the upload rules: %+v", viewer.Uploads)
	}
	f.want(http.StatusNotFound, anon, "GET", "/media/gallery/"+cid(2), nil, nil)
	f.want(http.StatusNotFound, user, "GET", "/media/gallery/"+cid(1)+"/hls/master.m3u8?audio=ja", nil, nil)

	// The preset rules, without a read.
	var presets []media.PresetRule
	f.want(http.StatusOK, anon, "GET", "/media/presets", nil, &presets)
	if len(presets) != 2 || presets[0].Kind != "gallery" || presets[0].Name != "cover" || presets[0].MinWidth != 8 ||
		presets[0].Base != "https://media.test" || presets[0].To != "cover-{w}.webp" || len(presets[0].Widths) != 1 || presets[1].Name != "avatar" {
		t.Fatalf("presets %+v", presets)
	}
}

func presignPNG(ref media.RefBody, path string, body []byte) media.PresignBody {
	sum := sha256.Sum256(body)
	return media.PresignBody{Ref: ref, Path: path, Type: "image/png", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
}

// upload presigns path and PUTs the bytes where the plan says, as a browser does.
func upload(t *testing.T, f *server, bearer string, ref media.RefBody, path string, body []byte) media.PresignReply {
	t.Helper()
	var plan media.PresignReply
	f.want(http.StatusOK, bearer, "POST", "/media/upload/presign", presignPNG(ref, path, body), &plan)
	if plan.Put == nil {
		return plan // the folder already holds these bytes
	}
	req, _ := http.NewRequest(plan.Put.Method, plan.Put.URL, bytes.NewReader(body))
	for k, v := range plan.Put.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT %s: %d %s", path, resp.StatusCode, b)
	}
	return plan
}
