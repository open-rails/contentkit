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
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/offers"
)

// channel is OnlyDemo's channel: user-owned, sellable, AuthKit-scoped, one
// per person, its creators' uploads limited.
var channel = kinds.GroupKind{Route: "c", Owner: "user", Sellable: true, AuthScope: true, MaxPerOwner: 1,
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
		TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesReadOnly,
		Merchants: map[string]openrails.MerchantDeclaration{w.tenant: {DisplayName: "E2E"}}}, openrails.Deps{Postgres: w.pool})
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
		c.Content.GroupKinds["channel"] = channel
		d.Billing, d.Scopes = ckopenrails.Billing(merchant), ckauthkit.Scopes(w.ak)
	})
	f := serve(t, e)

	var made []groups.Group
	f.want(http.StatusCreated, creatorToken, "POST", "/groups", []groups.GroupInput{{Kind: "channel", Slug: "studio",
		Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Studio"}}}}, &made)
	ch := made[0]
	if ch.OwnerUserID != creator.ID || ch.AuthScope != string(ch.ID) {
		t.Fatalf("channel %+v", ch)
	}
	f.want(http.StatusConflict, creatorToken, "POST", "/groups", []groups.GroupInput{{Kind: "channel", Slug: "another"}}, nil)
	authtest.GrantRole(t, w.ak, iam.GroupByID(ch.AuthScope), iam.UserSubject(editor.ID), w.channelEditor)
	path := "/groups/" + string(ch.ID)

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
	f.want(http.StatusForbidden, strangerToken, "PATCH", path, groups.GroupUpdate{Avatar: &groups.Avatar{Image: image}}, nil)
	f.want(http.StatusOK, editorToken, "PATCH", path, groups.GroupUpdate{Avatar: &groups.Avatar{Image: image}}, nil)

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

	var page groups.GroupPage
	f.want(http.StatusOK, viewerToken, "GET", "/groups?kind=channel&language=en", nil, &page)
	card := page.Groups[0]
	if len(page.Groups) != 1 || card.Avatar == nil || card.Avatar.Image != image || len(card.Avatar.Renditions) == 0 ||
		card.Avatar.Renditions[0].URL != published.Renditions[0].URL {
		t.Fatalf("card avatar %+v, published %+v", card.Avatar, published)
	}
	if a := card.Access; a == nil || a.Member || a.Offer == nil || a.Offer.Terms.Amount != 0 || a.Key.Group() != string(ch.ID) {
		t.Fatalf("card access %+v", a)
	}
	f.want(http.StatusOK, viewerToken, "POST", path+"/join", nil, nil)
	var d groups.GroupDetail
	f.want(http.StatusOK, viewerToken, "GET", path, nil, &d)
	if d.Access == nil || !d.Access.Member || d.Avatar == nil || len(d.Avatar.Renditions) == 0 {
		t.Fatalf("detail after joining: access %+v avatar %+v", d.Access, d.Avatar)
	}
	var mine groups.MyGroupPage
	f.want(http.StatusOK, viewerToken, "GET", "/me/groups", nil, &mine)
	if len(mine.Groups) != 1 || !mine.Groups[0].Member || mine.Groups[0].Owner || mine.Groups[0].Editor {
		t.Fatalf("the viewer's groups %+v", mine)
	}
	f.want(http.StatusOK, editorToken, "GET", "/me/groups?kind=channel", nil, &mine)
	if len(mine.Groups) != 1 || !mine.Groups[0].Editor || mine.Groups[0].Owner {
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
	if _, err := c.PatchGroup(ctx, string(ch.ID), groups.GroupUpdate{Text: []groups.Text{{Language: "en", Description: "Films"}}}); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetGroup(ctx, string(ch.ID), contentkit.GetGroupQuery{Language: "en"})
	if err != nil || len(got.Text) != 1 || got.Text[0].Description != "Films" || got.Name != "Studio" {
		t.Fatalf("client detail %+v %v", got, err)
	}
	var ce *contentkit.Error
	if _, err := c.GetGroup(ctx, contentref.NewID(), contentkit.GetGroupQuery{}); !errors.As(err, &ce) || ce.Code != "not_found" {
		t.Fatalf("a missing group: %v", err)
	}

	// A holder of channel:content:write writes the channel's posts; nobody
	// else does.
	writer, writerToken := w.person(iam.Role{})
	authtest.GrantRole(t, w.ak, iam.GroupByID(ch.AuthScope), iam.UserSubject(writer.ID), w.channelWriter)
	post := catalog.ContentInput{Kind: "blog", OwnerGroupID: string(ch.ID), Text: map[string]catalog.Text{"en": {Title: "Hello"}}}
	f.want(http.StatusForbidden, strangerToken, "POST", "/contents", post, nil)
	var made2 catalog.Content
	f.want(http.StatusCreated, writerToken, "POST", "/contents", post, &made2)
	listing := "unlisted"
	f.want(http.StatusOK, writerToken, "PATCH", "/contents/"+made2.ID, catalog.ContentPatch{Listing: &listing}, nil)
	f.want(http.StatusForbidden, strangerToken, "PATCH", "/contents/"+made2.ID, catalog.ContentPatch{Listing: &listing}, nil)
	f.want(http.StatusForbidden, writerToken, "PATCH", path, groups.GroupUpdate{Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Mine"}}}, nil)

	// Renaming the channel renames its product on OpenRails.
	f.want(http.StatusOK, editorToken, "PATCH", path, groups.GroupUpdate{Names: []groups.Name{{Language: "en", Kind: groups.NameCanonical, Name: "Studio Two"}}}, nil)
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
	var deleted groups.Group
	f.want(http.StatusOK, creatorToken, "DELETE", path, nil, &deleted)
	if deleted.State != groups.StateDeleted || deleted.AuthScope != "" {
		t.Fatalf("deleted %+v", deleted)
	}
	if g, err := w.ak.Group(ctx, iam.GroupByID(string(ch.ID))); err != nil || g.DeletedAt == nil {
		t.Fatalf("the scope after delete %+v %v", g, err)
	}
	items := pgx.Identifier{w.schema, "content_items"}.Sanitize()
	stateOf := func(id string) (state, cause string) {
		t.Helper()
		if err := w.pool.QueryRow(ctx, `SELECT state, coalesce(deleted_with_group, '') FROM `+items+` WHERE id = $1`, id).Scan(&state, &cause); err != nil {
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
	var back groups.Group
	f.want(http.StatusOK, creatorToken, "PATCH", path, groups.GroupUpdate{State: ptr(groups.StateActive)}, &back)
	if back.State != groups.StateActive || back.AuthScope != string(ch.ID) {
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

// A signed-in reader of a published channel post costs AuthKit no editor
// check, and a failing one never fails it; the channel's writer still sees
// its draft, and a failing check hides a draft rather than answering 503.
func TestChannelPostReads(t *testing.T) {
	w := newWorld(t)
	writer, writerToken := w.person(iam.Role{})
	_, readerToken := w.person(iam.Role{})
	creator, creatorToken := w.person(iam.Role{})
	counter := scopeCounter{Authenticator: w.ak.Authenticator(), root: w.scope, asked: new(atomic.Int32), fail: new(atomic.Bool)}
	f := serve(t, w.engine(func(c *contentkit.Config, d *contentkit.Deps) {
		c.Content.GroupKinds["channel"] = channel
		d.Auth, d.Scopes = counter, ckauthkit.Scopes(w.ak)
	}))
	var made []groups.Group
	f.want(http.StatusCreated, creatorToken, "POST", "/groups", []groups.GroupInput{{Kind: "channel", Slug: "studio"}}, &made)
	ch := made[0]
	if ch.OwnerUserID != creator.ID || ch.AuthScope == "" {
		t.Fatalf("channel %+v", ch)
	}
	authtest.GrantRole(t, w.ak, iam.GroupByID(ch.AuthScope), iam.UserSubject(writer.ID), w.channelWriter)
	in := catalog.ContentInput{Kind: "blog", OwnerGroupID: string(ch.ID), Creators: &[]catalog.Creator{},
		Text: map[string]catalog.Text{"en": {Title: "Hello"}}}
	var post, draft catalog.Content
	f.want(http.StatusCreated, writerToken, "POST", "/contents", in, &post)
	f.want(http.StatusOK, writerToken, "POST", "/contents/"+post.ID+"/publish", catalog.PublishInput{}, nil)
	f.want(http.StatusCreated, writerToken, "POST", "/contents", in, &draft)

	counter.asked.Store(0)
	var got catalog.Content
	f.want(http.StatusOK, readerToken, "GET", "/contents/"+post.ID, nil, &got)
	if got.ID != post.ID || got.Editor || counter.asked.Load() != 0 {
		t.Fatalf("published read %+v: %d group checks, want none", got, counter.asked.Load())
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
