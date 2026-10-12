package e2e

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"

	contentkit "github.com/open-rails/contentkit"
	ckauthkit "github.com/open-rails/contentkit/adapters/authkit"
	ckopenrails "github.com/open-rails/contentkit/adapters/openrails"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
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

// channelMedia gives the fixtures' registry the channel kind's media, as
// the engine derives it (media.ForContent), for publish.
func channelMedia(w *world, cfg contentkit.Config) {
	w.t.Helper()
	mc := cfg.Media.Config
	mc.Namespace, mc.Hooks = w.tenant, media.Hooks{Resolver: w, CanUpload: w}
	mc.Kinds = append(mc.Kinds, media.GroupKind("channel", channel))
	var err error
	if w.reg, err = media.NewRegistry(mc); err != nil {
		w.t.Fatal(err)
	}
	if w.queue, err = workqueue.New(w.pool, w.reg, w.schema+"_media_worker"); err != nil {
		w.t.Fatal(err)
	}
	journal, err := media.NewPGJournal(w.pool, w.schema, w.queue)
	if err != nil {
		w.t.Fatal(err)
	}
	if w.jobs, err = media.NewJobs(media.JobsConfig{Store: w.env.Store, Registry: w.reg, Locker: media.PGLocker(w.pool), Journal: journal,
		Processes: w.queue, Pool: w.pool}); err != nil {
		w.t.Fatal(err)
	}
}

// A person's channel over the real engine, AuthKit, OpenRails and media: the
// creator owns it within the cap and AuthKit scopes it; an editor of the
// scope uploads and selects its avatar and sells it; a stranger may do
// neither; cards carry the avatar, the offer and the viewer's membership;
// the viewer joins it free and finds it among their groups; uploads are rate
// limited and capped per channel; the host's Go client reads and writes it;
// deleting it retires its scope and its media.
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
	var cfg contentkit.Config
	e := w.engine(func(c *contentkit.Config, d *contentkit.Deps) {
		c.Content.GroupKinds["channel"] = channel
		d.Billing, d.Scopes = ckopenrails.Billing(merchant), ckauthkit.Scopes(w.ak)
		cfg = *c
	})
	channelMedia(w, cfg)
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
	if a := card.Access; a == nil || a.Member || a.Offer == nil || a.Offer.Amount != 0 || a.Key.Group() != string(ch.ID) {
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

	// Deleting retires the scope and queues the media folder's deletion.
	var deleted groups.Group
	f.want(http.StatusOK, creatorToken, "DELETE", path, nil, &deleted)
	if deleted.State != groups.StateDeleted || deleted.AuthScope != "" {
		t.Fatalf("deleted %+v", deleted)
	}
	if g, err := w.ak.Group(ctx, iam.GroupByID(string(ch.ID))); err != nil || g.DeletedAt == nil {
		t.Fatalf("the scope after delete %+v %v", g, err)
	}
	var queued int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{w.schema + "_river", "river_job"}.Sanitize()+
		` WHERE kind = 'contentkit_media_delete_folder' AND args->>'prefix' LIKE '%' || $1 || '%'`, string(ch.ID)).Scan(&queued); err != nil || queued == 0 {
		t.Fatalf("the media folder's deletion: %d queued, %v", queued, err)
	}
}
