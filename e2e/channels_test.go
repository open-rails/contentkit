package e2e

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"

	contentkit "github.com/open-rails/contentkit"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
	ckopenrails "github.com/open-rails/contentkit/adapters/openrails"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/offers"
)

// channel is OnlyDemo's channel: user-owned, sellable, AuthKit-scoped, one
// per person, its creators' uploads limited.
var channel = kinds.EntityKind{Route: "c", Owner: "user", Sellable: true, AuthScope: true, MaxPerOwner: 1,
	Uploads: kinds.UploadLimits{FilesPerHour: 2, QuotaBytes: 1 << 20}}

// channelBilling is the site's merchant on an embedded OpenRails: its client
// makes customers too.
func channelBilling(w *world) *openrails.Client {
	w.t.Helper()
	ctx := w.t.Context()
	schema := "ckor_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	w.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = w.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	engine, err := openrails.New(ctx, openrails.Config{Database: openrails.DatabaseConfig{Schema: schema, RiverSchema: schema},
		ProviderWriteMode: openrails.ProviderWritesReadOnly,
		Merchants:         map[string]openrails.MerchantDeclaration{w.tenant: {DisplayName: "E2E"}}}, openrails.Deps{Postgres: w.pool})
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = engine.Close(context.Background()) })
	merchant, err := engine.MerchantBySlug(w.tenant)
	if err != nil {
		w.t.Fatal(err)
	}
	return merchant
}

// A person's channel over the real engine, AuthKit, OpenRails and media: the
// creator owns it within the cap and AuthKit scopes it; an editor of the
// scope uploads and selects its avatar and sells it; a stranger may do
// neither; cards carry the avatar, the offer and the viewer's membership;
// the viewer joins it free and finds it among their groups; uploads are rate
// limited and capped per channel; the host's Go client reads and writes it;
// a holder of channel:content:write writes its posts; renaming it renames
// its OpenRails product; deleting it retires its scope, its posts and hides
// its media; restoring it brings back its scope, media and the posts its
// deletion took, not one deleted before.
func TestChannels(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	creator, creatorToken := w.person(iam.Role{})
	editor, editorToken := w.person(iam.Role{})
	viewer, viewerToken := w.person(iam.Role{})
	_, strangerToken := w.person(iam.Role{})
	merchant := channelBilling(w)
	if _, err := merchant.UpdateCustomer(ctx, billing.CustomerID(uuid.MustParse(viewer.ID)), billing.UpdateCustomerParams{}); err != nil {
		t.Fatal(err)
	}
	e := w.engine(func(c *contentkit.Config, d *contentkit.Deps) {
		c.Content.EntityKinds["channel"] = channel
		d.Billing, d.Scopes = ckopenrails.Billing(merchant), ckauthkit.Scopes(w.ak)
	})
	f := serve(t, e)

	var made []entities.Entity
	f.want(http.StatusCreated, creatorToken, "POST", "/entities", []entities.EntityInput{{Kind: "channel", Slug: "studio",
		Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Studio"}}}}, &made)
	ch := made[0]
	if ch.OwnerUserID != creator.ID || ch.AuthScope != string(ch.ID) {
		t.Fatalf("channel %+v", ch)
	}
	f.want(http.StatusConflict, creatorToken, "POST", "/entities", []entities.EntityInput{{Kind: "channel", Slug: "another"}}, nil)
	authtest.GrantRole(t, w.ak, iam.GroupByID(ch.AuthScope), iam.UserSubject(editor.ID), w.channelEditor)
	path := "/entities/" + string(ch.ID)

	// The editor uploads and selects the avatar; the stranger may not.
	ref := media.RefBody{Kind: "channel", ID: string(ch.ID)}
	body := pngBytes(t, 32)
	f.want(http.StatusForbidden, strangerToken, "POST", "/media/upload/presign", presignPNG(ref, "avatar/me.png", body), nil)
	plan := upload(t, f, editorToken, ref, "avatar/me.png", body)
	f.want(http.StatusOK, editorToken, "POST", "/media/upload/commit", media.CommitBody{Ref: ref, OperationID: uuid.NewString(),
		Ops: []media.Op{{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}}}, nil)
	itemRef, err := w.reg.Ref("channel", string(ch.ID))
	if err != nil {
		t.Fatal(err)
	}
	published := publish(t, w, itemRef, plan.Path, "avatar")
	image := strings.TrimSuffix(strings.TrimPrefix(plan.Path, "avatar/"), ".png")
	f.want(http.StatusForbidden, strangerToken, "PATCH", path, entities.EntityUpdate{Avatar: &entities.Avatar{Image: image}}, nil)
	f.want(http.StatusOK, editorToken, "PATCH", path, entities.EntityUpdate{Avatar: &entities.Avatar{Image: image}}, nil)

	// Uploads: two files an hour per uploader, a megabyte per channel.
	big := presignPNG(ref, "cover/big.png", body)
	big.Size = 2 << 20
	f.want(http.StatusRequestEntityTooLarge, editorToken, "POST", "/media/upload/presign", big, nil)
	f.want(http.StatusOK, editorToken, "POST", "/media/upload/presign", presignPNG(ref, "cover/c.png", pngBytes(t, 33)), nil)
	f.want(http.StatusTooManyRequests, editorToken, "POST", "/media/upload/presign", presignPNG(ref, "cover/d.png", pngBytes(t, 34)), nil)

	// Selling: the editor sets it free to join; the stranger may not.
	free := offers.Terms{Currency: "USD"}
	f.want(http.StatusForbidden, strangerToken, "PUT", path+"/offer", free, nil)
	f.want(http.StatusOK, editorToken, "PUT", path+"/offer", free, nil)

	var page entities.EntityPage
	f.want(http.StatusOK, viewerToken, "GET", "/entities?kind=channel&language=en", nil, &page)
	card := page.Entities[0]
	if len(page.Entities) != 1 || card.Avatar == nil || card.Avatar.Image != image || len(card.Avatar.Renditions) == 0 ||
		card.Avatar.Renditions[0].URL != published.Renditions[0].URL {
		t.Fatalf("card avatar %+v, published %+v", card.Avatar, published)
	}
	if a := card.Access; a == nil || a.Member || a.Offer == nil || a.Offer.Terms.Amount != 0 || a.Key.Entity() != string(ch.ID) {
		t.Fatalf("card access %+v", a)
	}
	f.want(http.StatusOK, viewerToken, "POST", path+"/join", nil, nil)
	var d entities.EntityDetail
	f.want(http.StatusOK, viewerToken, "GET", path, nil, &d)
	if d.Access == nil || !d.Access.Member || d.Avatar == nil || len(d.Avatar.Renditions) == 0 {
		t.Fatalf("detail after joining: access %+v avatar %+v", d.Access, d.Avatar)
	}
	var mine entities.MyEntityPage
	f.want(http.StatusOK, viewerToken, "GET", "/me/entities", nil, &mine)
	if len(mine.Entities) != 1 || !mine.Entities[0].Member || mine.Entities[0].Owner || mine.Entities[0].Editor {
		t.Fatalf("the viewer's groups %+v", mine)
	}
	f.want(http.StatusOK, editorToken, "GET", "/me/entities?kind=channel", nil, &mine)
	if len(mine.Entities) != 1 || !mine.Entities[0].Editor || mine.Entities[0].Owner {
		t.Fatalf("the editor's groups %+v", mine)
	}

	// A paid subscription, then free again: the sync stops renewals.
	monthly := 720
	f.want(http.StatusOK, editorToken, "PUT", path+"/offer", offers.Terms{Amount: 4_990_000, Currency: "USD", RenewHours: &monthly}, nil)
	f.want(http.StatusOK, creatorToken, "PUT", path+"/offer", free, nil)
	c := e.Client()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(200 * time.Millisecond) {
		list, err := c.GetOffers(ctx, contentkit.GetOffersQuery{Unsynced: ptr(true)})
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Offers) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("offers never synced: %+v", list.Offers)
		}
	}

	// The host's in-process client: staff, without a credential.
	if _, err := c.PatchEntity(ctx, string(ch.ID), entities.EntityUpdate{Text: []entities.Text{{Language: "en", Description: "Films"}}}); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetEntity(ctx, string(ch.ID), contentkit.GetEntityQuery{Language: "en"})
	if err != nil || len(got.Text) != 1 || got.Text[0].Description != "Films" || got.Name != "Studio" {
		t.Fatalf("client detail %+v %v", got, err)
	}
	var ce *contentkit.Error
	if _, err := c.GetEntity(ctx, contentref.NewID(), contentkit.GetEntityQuery{}); !errors.As(err, &ce) || ce.Code != "not_found" {
		t.Fatalf("a missing group: %v", err)
	}

	// A holder of channel:content:write writes the channel's posts; nobody
	// else does.
	writer, writerToken := w.person(iam.Role{})
	authtest.GrantRole(t, w.ak, iam.GroupByID(ch.AuthScope), iam.UserSubject(writer.ID), w.channelWriter)
	post := catalog.ContentInput{Kind: "blog", OwnerEntityID: string(ch.ID), Text: map[string]catalog.Text{"en": {Title: "Hello"}}}
	f.want(http.StatusForbidden, strangerToken, "POST", "/contents", post, nil)
	var made2 catalog.Content
	f.want(http.StatusCreated, writerToken, "POST", "/contents", post, &made2)
	comments := "closed"
	f.want(http.StatusOK, writerToken, "PATCH", "/contents/"+made2.ID, catalog.ContentPatch{Comments: &comments}, nil)
	f.want(http.StatusForbidden, strangerToken, "PATCH", "/contents/"+made2.ID, catalog.ContentPatch{Comments: &comments}, nil)
	f.want(http.StatusForbidden, writerToken, "PATCH", path, entities.EntityUpdate{Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Mine"}}}, nil)

	// Renaming the channel renames its product on OpenRails.
	f.want(http.StatusOK, editorToken, "PATCH", path, entities.EntityUpdate{Names: []entities.Name{{Language: "en", Kind: entities.NameCanonical, Name: "Studio Two"}}}, nil)
	bill := ckopenrails.Billing(merchant)
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(200 * time.Millisecond) {
		products, err := bill.Products(ctx, []string{card.Access.Key.String()})
		if err != nil {
			t.Fatal(err)
		}
		if p, ok := products[card.Access.Key.String()]; ok && p.DisplayName == "Studio Two" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the product keeps its name: %+v", products)
		}
	}

	// Deleting retires the scope, deletes the channel's posts and hides its
	// media, keeping its folder for a restore.
	var earlier catalog.Content
	f.want(http.StatusCreated, writerToken, "POST", "/contents", post, &earlier)
	f.want(http.StatusNoContent, writerToken, "DELETE", "/contents/"+earlier.ID, nil, nil)
	var deleted entities.Entity
	f.want(http.StatusOK, creatorToken, "DELETE", path, nil, &deleted)
	if deleted.State != entities.StateDeleted || deleted.AuthScope != "" {
		t.Fatalf("deleted %+v", deleted)
	}
	if g, err := w.ak.Group(ctx, iam.GroupByID(string(ch.ID))); err != nil || g.DeletedAt == nil {
		t.Fatalf("the scope after delete %+v %v", g, err)
	}
	items := pgx.Identifier{w.schema, "content_items"}.Sanitize()
	stateOf := func(id string) (state, cause string) {
		t.Helper()
		if err := w.pool.QueryRow(ctx, `SELECT state, coalesce(deleted_with_entity, '') FROM `+items+` WHERE id = $1`, id).Scan(&state, &cause); err != nil {
			t.Fatal(err)
		}
		return state, cause
	}
	if state, cause := stateOf(made2.ID); state != "deleted" || cause != string(ch.ID) {
		t.Fatalf("the channel's post after delete: %q by %q", state, cause)
	}
	if state, cause := stateOf(earlier.ID); state != "deleted" || cause != "" {
		t.Fatalf("the post deleted before: %q by %q", state, cause)
	}
	jobs := func(kind string) int {
		t.Helper()
		var n int
		if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize()+
			` WHERE kind = $1 AND args::text LIKE '%' || $2 || '%'`, kind, string(ch.ID)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := jobs("contentkit_media_delete_folder"); n != 0 {
		t.Fatalf("the media folder's deletion was queued %d times on delete", n)
	}
	hides := jobs("contentkit_media_expose")
	if hides == 0 {
		t.Fatal("deleting queued no hiding of the channel's media")
	}

	// Restoring brings back the scope, the media and the post its deletion
	// took; the one deleted before stays deleted.
	var back entities.Entity
	f.want(http.StatusOK, creatorToken, "PATCH", path, entities.EntityUpdate{State: ptr(entities.StateActive)}, &back)
	if back.State != entities.StateActive || back.AuthScope != string(ch.ID) {
		t.Fatalf("restored %+v", back)
	}
	if state, cause := stateOf(made2.ID); state != "draft" || cause != "" {
		t.Fatalf("the channel's post after restore: %q by %q", state, cause)
	}
	if state, _ := stateOf(earlier.ID); state != "deleted" {
		t.Fatalf("the post deleted before the channel: %q after restore", state)
	}
	if n := jobs("contentkit_media_expose"); n <= hides {
		t.Fatalf("restoring queued no showing of the channel's media (%d exposes)", n)
	}
	var restored catalog.Content
	f.want(http.StatusOK, creatorToken, "GET", "/contents/"+made2.ID, nil, &restored)
	if !restored.Editor || restored.State != catalog.StateDraft {
		t.Fatalf("the restored post as its channel's owner sees it: %+v", restored)
	}
}

// scopeCounter is AuthKit's Authenticator, counting the permission checks
// asked in group scopes (any but the root) and failing them while fail is
// set.
type scopeCounter struct {
	auth.Authenticator
	root  auth.Scope
	asked *atomic.Int32
	fail  *atomic.Bool
}

func (c scopeCounter) RootScope(ctx context.Context) (auth.Scope, error) {
	return c.Authenticator.(auth.Root).RootScope(ctx)
}

func (c scopeCounter) KnownPermission(p string) bool {
	return c.Authenticator.(auth.PermissionCatalog).KnownPermission(p)
}

func (c scopeCounter) Authenticate(r *http.Request) (auth.Verified, error) {
	v, err := c.Authenticator.Authenticate(r)
	if err != nil {
		return nil, err
	}
	return countedVerified{Verified: v, c: c}, nil
}

type countedVerified struct {
	auth.Verified
	c scopeCounter
}

func (v countedVerified) Can(ctx context.Context, scope auth.Scope, perm string) (bool, error) {
	if scope != v.c.root {
		v.c.asked.Add(1)
		if v.c.fail.Load() {
			return false, errors.Join(auth.ErrUnavailable, errors.New("authkit down"))
		}
	}
	return v.Verified.(auth.PermissionChecker).Can(ctx, scope, perm)
}

// A signed-in reader's detail of a published channel post asks AuthKit once
// whether they edit it, and a failing check never fails it; the channel's
// writer still sees its draft, and a failing check hides a draft rather than
// answering 503.
func TestChannelPostReads(t *testing.T) {
	w := newWorld(t)
	writer, writerToken := w.person(iam.Role{})
	_, readerToken := w.person(iam.Role{})
	creator, creatorToken := w.person(iam.Role{})
	counter := scopeCounter{Authenticator: w.ak.Authenticator(), root: w.scope, asked: new(atomic.Int32), fail: new(atomic.Bool)}
	f := serve(t, w.engine(func(c *contentkit.Config, d *contentkit.Deps) {
		c.Content.EntityKinds["channel"] = channel
		d.Auth, d.Scopes = counter, ckauthkit.Scopes(w.ak)
	}))
	var made []entities.Entity
	f.want(http.StatusCreated, creatorToken, "POST", "/entities", []entities.EntityInput{{Kind: "channel", Slug: "studio"}}, &made)
	ch := made[0]
	if ch.OwnerUserID != creator.ID || ch.AuthScope == "" {
		t.Fatalf("channel %+v", ch)
	}
	authtest.GrantRole(t, w.ak, iam.GroupByID(ch.AuthScope), iam.UserSubject(writer.ID), w.channelWriter)
	in := catalog.ContentInput{Kind: "blog", OwnerEntityID: string(ch.ID), Creators: &[]catalog.Creator{},
		Text: map[string]catalog.Text{"en": {Title: "Hello"}}}
	var post, draft catalog.Content
	f.want(http.StatusCreated, writerToken, "POST", "/contents", in, &post)
	f.want(http.StatusOK, writerToken, "POST", "/contents/"+post.ID+"/publish", catalog.PublishInput{}, nil)
	f.want(http.StatusCreated, writerToken, "POST", "/contents", in, &draft)

	counter.asked.Store(0)
	var got catalog.Content
	f.want(http.StatusOK, readerToken, "GET", "/contents/"+post.ID, nil, &got)
	if got.ID != post.ID || got.Editor || counter.asked.Load() != 1 {
		t.Fatalf("published read %+v: %d group checks, want one", got, counter.asked.Load())
	}
	f.want(http.StatusOK, writerToken, "GET", "/contents/"+draft.ID, nil, &got)
	if !got.Editor || counter.asked.Load() == 0 {
		t.Fatalf("the writer's draft %+v after %d group checks", got, counter.asked.Load())
	}

	counter.fail.Store(true)
	f.want(http.StatusOK, readerToken, "GET", "/contents/"+post.ID, nil, nil)
	f.want(http.StatusOK, writerToken, "GET", "/contents/"+post.ID, nil, nil)
	f.want(http.StatusNotFound, writerToken, "GET", "/contents/"+draft.ID, nil, nil)
	f.want(http.StatusServiceUnavailable, writerToken, "PATCH", "/contents/"+draft.ID, catalog.ContentPatch{}, nil)
}

// A channel's comment moderators (channel:comments:moderate, and its owner)
// moderate comments on its posts, asked only when they act: they delete
// anyone's, hold one out of its thread and release it, list what is held,
// and ban a commenter from the channel's content only; nobody else does,
// nor a moderator of another channel. They find the channel among their
// groups as its moderator.
func TestChannelCommentModeration(t *testing.T) {
	w := newWorld(t)
	_, creatorToken := w.person(iam.Role{})
	moderator, moderatorToken := w.person(iam.Role{})
	commenter, commenterToken := w.person(iam.Role{})
	_, strangerToken := w.person(iam.Role{})
	_, otherToken := w.person(iam.Role{})
	_, staff := w.person(w.staff)
	f := serve(t, w.engine(func(c *contentkit.Config, d *contentkit.Deps) {
		c.Content.EntityKinds["channel"] = channel
		d.Scopes = ckauthkit.Scopes(w.ak)
	}))
	var made []entities.Entity
	f.want(http.StatusCreated, creatorToken, "POST", "/entities", []entities.EntityInput{{Kind: "channel", Slug: "studio"}}, &made)
	ch := made[0]
	authtest.GrantRole(t, w.ak, iam.GroupByID(ch.AuthScope), iam.UserSubject(moderator.ID), w.channelModerator)
	f.want(http.StatusCreated, otherToken, "POST", "/entities", []entities.EntityInput{{Kind: "channel", Slug: "other"}}, nil)
	var post catalog.Content
	f.want(http.StatusCreated, creatorToken, "POST", "/contents", catalog.ContentInput{Kind: "blog", OwnerEntityID: string(ch.ID),
		Text: map[string]catalog.Text{"en": {Title: "Hello"}}}, &post)
	f.want(http.StatusOK, creatorToken, "POST", "/contents/"+post.ID+"/publish", catalog.PublishInput{}, nil)
	elsewhere, _ := w.published(f, staff, "blog")
	item := "/contents/" + post.ID + "/comments"
	comment := func(token, target, body string) content.Comment {
		t.Helper()
		var c content.Comment
		f.want(http.StatusCreated, token, "POST", target, content.CommentInput{Body: body}, &c)
		return c
	}
	listed := func(id string) bool {
		t.Helper()
		var list []content.Comment
		f.want(http.StatusOK, "", "GET", item, nil, &list)
		for _, c := range list {
			if c.ID == id {
				return true
			}
		}
		return false
	}

	rude := comment(commenterToken, item, "rude")
	f.want(http.StatusForbidden, strangerToken, "DELETE", "/comments/"+rude.ID, nil, nil)
	f.want(http.StatusForbidden, otherToken, "DELETE", "/comments/"+rude.ID, nil, nil)
	f.want(http.StatusNoContent, moderatorToken, "DELETE", "/comments/"+rude.ID, nil, nil)
	f.want(http.StatusForbidden, moderatorToken, "PATCH", "/comments/"+comment(commenterToken, item, "mine").ID, content.CommentEdit{Body: "theirs"}, nil)

	doubtful := comment(commenterToken, item, "doubtful")
	f.want(http.StatusForbidden, commenterToken, "POST", "/comments/"+doubtful.ID+"/hold", nil, nil)
	f.want(http.StatusNoContent, moderatorToken, "POST", "/comments/"+doubtful.ID+"/hold", nil, nil)
	f.want(http.StatusConflict, moderatorToken, "POST", "/comments/"+doubtful.ID+"/hold", nil, nil)
	if listed(doubtful.ID) {
		t.Fatal("a held comment stays in its thread")
	}
	var held content.HeldPage
	f.want(http.StatusForbidden, strangerToken, "GET", "/entities/"+string(ch.ID)+"/held-comments", nil, nil)
	f.want(http.StatusOK, creatorToken, "GET", "/entities/"+string(ch.ID)+"/held-comments", nil, &held)
	if len(held.Items) != 1 || held.Items[0].ID != doubtful.ID {
		t.Fatalf("the channel's held comments %+v", held.Items)
	}
	f.want(http.StatusNoContent, moderatorToken, "POST", "/comments/"+doubtful.ID+"/release", nil, nil)
	if !listed(doubtful.ID) {
		t.Fatal("a released comment is not back in its thread")
	}

	bans := "/entities/" + string(ch.ID) + "/comment-bans"
	f.want(http.StatusForbidden, strangerToken, "PUT", bans+"/"+commenter.ID, content.BanInput{Reason: "spam"}, nil)
	f.want(http.StatusOK, moderatorToken, "PUT", bans+"/"+commenter.ID, content.BanInput{Reason: "spam"}, nil)
	var refusal struct{ Code string }
	f.want(http.StatusForbidden, commenterToken, "POST", item, content.CommentInput{Body: "again"}, &refusal)
	if refusal.Code != "comment_banned" {
		t.Fatalf("a banned commenter answered %q", refusal.Code)
	}
	comment(commenterToken, "/contents/"+elsewhere+"/comments", "elsewhere")
	var list []content.CommentBan
	f.want(http.StatusOK, creatorToken, "GET", bans, nil, &list)
	if len(list) != 1 || list[0].UserID != commenter.ID || list[0].Scope != content.EntityScope(string(ch.ID)) {
		t.Fatalf("the channel's bans %+v", list)
	}
	f.want(http.StatusNoContent, moderatorToken, "DELETE", bans+"/"+commenter.ID, nil, nil)
	comment(commenterToken, item, "again")

	var mine entities.MyEntityPage
	f.want(http.StatusOK, moderatorToken, "GET", "/me/entities", nil, &mine)
	if g := mine.Entities; len(g) != 1 || g[0].ID != ch.ID || !g[0].Moderator || g[0].Writer || g[0].Editor {
		t.Fatalf("the moderator's groups %+v", mine)
	}
}
