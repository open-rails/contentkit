package main

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/taxonomy"
)

// The /ck mount serves every module the way contentkit.Runtime.Handler does
// (content at the root, the others at their fixed sub-paths) over this
// fixture's schema, bucket and media handlers, for the SDK's content tests.
// It composes the modules itself: no package may depend on the root
// (internal/boundaries). Callers:
//
//	X-Test-Actor: alice   a signed-in user (none: anonymous, keyed by X-Test-IP)
//	staff                 holds every permission
//	moderator             CommentModerate, ModerationReview and CommentBan
//	editor                PostWrite and PollWrite
//	creator               owns every video and gallery (owner bans)
//
// Items of the content kinds "video" and "gallery" (UUIDv7 ids) are visible
// unless their id ends in "dead", accessible unless it ends in "10cced". A comment
// or post containing "[hold]" is held for review, "[reject]" is refused.
// Free-text answers group by their first word.

// The media kinds of post and poll folders: their images are server-named.
var contentFolders = []media.Kind{
	{Name: "ckpost", Uploads: []media.Upload{{Path: "{name}", Types: images, MaxBytes: 1 << 20, Named: true, Max: 100}},
		Public: []media.Public{{Name: "inline", From: "{name}", To: "{name}.webp", Image: media.Image{Width: 1600}}}},
	{Name: "ckpoll", Uploads: []media.Upload{{Path: "{name}", Types: images, MaxBytes: 1 << 20, Named: true, Max: 100}},
		Public: []media.Public{{Name: "inline", From: "{name}", To: "{name}.webp", Image: media.Image{Width: 800}}}},
}

var testPerms = content.Perms{PostWrite: "post:write", PollWrite: "poll:write", CommentModerate: "comment:moderate",
	ModerationReview: "moderation:review", CommentBan: "comment:ban", Taxonomy: "taxonomy"}

type testAuthz struct{}

func (testAuthz) Can(_ context.Context, a access.Actor, perm string) (bool, error) {
	switch a.ID {
	case "staff":
		return true, nil
	case "moderator":
		return perm == testPerms.CommentModerate || perm == testPerms.ModerationReview || perm == testPerms.CommentBan, nil
	case "editor":
		return perm == testPerms.PostWrite || perm == testPerms.PollWrite, nil
	}
	return false, nil
}

type testResolver struct{}

func (testResolver) Resolve(_ context.Context, refs []contentref.ContentRef, _ access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, ref := range refs {
		id := ref.ContentID
		out[ref.Key()] = access.Resolution{Visible: !strings.HasSuffix(id, "dead"), Accessible: !strings.HasSuffix(id, "10cced"), Owner: "creator"}
	}
	return out, nil
}

type testUsers struct{}

func (testUsers) UsersByIDs(_ context.Context, ids []string) (map[string]content.PublicUser, error) {
	out := make(map[string]content.PublicUser, len(ids))
	for _, id := range ids {
		out[id] = content.PublicUser{ID: id, Username: id}
	}
	return out, nil
}

type testModerator struct{}

func (testModerator) Screen(_ context.Context, in content.ModerationInput) (content.Verdict, error) {
	switch {
	case strings.Contains(in.Text, "[reject]"):
		return content.Verdict{Decision: content.DecisionReject, Reason: "not allowed here"}, nil
	case strings.Contains(in.Text, "[hold]"):
		return content.Verdict{Decision: content.DecisionReview, Reason: "needs a look"}, nil
	}
	return content.Verdict{Decision: content.DecisionApprove}, nil
}

func (testModerator) StatelessPolicy() {}

type testClassifier struct{}

func (testClassifier) Classify(_ context.Context, a content.Answer) (content.GroupAssignment, error) {
	word := strings.ToLower(strings.Fields(a.Text + " other")[0])
	return content.GroupAssignment{GroupID: word, Label: strings.ToUpper(word[:1]) + word[1:]}, nil
}

func (testClassifier) StatelessPolicy() {}

// inlineURLs is the registry's URL of an inline image's public file.
type inlineURLs struct{ reg *media.Registry }

func (u inlineURLs) InlineURL(_ context.Context, ref contentref.ContentRef, name string) (string, error) {
	return u.reg.PublicURL(ref, name+".webp"), nil
}

// noFolders: the fixture has no media Jobs; visibility and deletion jobs are dropped.
type noFolders struct{}

func (noFolders) ExposeTx(context.Context, pgx.Tx, ...contentref.ContentRef) error { return nil }
func (noFolders) DeleteItemsTx(context.Context, pgx.Tx, ...media.Deletion) error   { return nil }

// uploadHooks resolves and authorizes post and poll folders through the
// content runtime (a draft's folder shows to its editors only), every other
// item as allow does.
type uploadHooks struct{ content *content.Runtime }

func (h *uploadHooks) Resolve(ctx context.Context, refs []contentref.ContentRef, a access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	var folders, other []contentref.ContentRef
	for _, ref := range refs {
		if k := ref.ContentKind; (k == "ckpost" || k == "ckpoll") && h.content != nil {
			folders = append(folders, ref)
		} else {
			other = append(other, ref)
		}
	}
	out, err := allow{}.Resolve(ctx, other, a)
	if err != nil || len(folders) == 0 {
		return out, err
	}
	res, err := h.content.MediaResolver().Resolve(ctx, folders, a)
	if err != nil {
		return nil, err
	}
	for k, r := range res {
		out[k] = r
	}
	return out, nil
}

func (h *uploadHooks) CanUpload(ctx context.Context, a access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	if k := t.Ref.ContentKind; (k == "ckpost" || k == "ckpoll") && h.content != nil {
		return h.content.CanUpload(ctx, a, t)
	}
	return allow{}.CanUpload(ctx, a, t)
}

// testActor puts the X-Test-Actor caller in the context, or an anonymous one keyed by X-Test-IP.
func testActor(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := r.Header.Get("X-Test-IP")
		if ip == "" {
			ip, _, _ = net.SplitHostPort(r.RemoteAddr)
		}
		a := access.Actor{Anonymous: true, IP: ip}
		if id := r.Header.Get("X-Test-Actor"); id != "" {
			a = access.Actor{ID: id, Kind: "user", IP: ip}
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, a)))
	})
}

func contentHandler(ctx context.Context, pool *pgxpool.Pool, schema, tenant string, reg *media.Registry, hooks *uploadHooks,
	uploads *media.Uploads, reader *media.Reader, anonymous content.Anonymous) http.Handler {
	codes, err := contenturl.New(contenturl.Options{Pool: pool, Schema: schema, Tenant: tenant})
	must(err)
	router, err := contenturl.NewRouter(codes, contenturl.RouterOptions{Routes: contenturl.Routes{"post": "blog", "video": "watch"}, Languages: []string{"en", "ja"}})
	must(err)
	tax, err := taxonomy.New(taxonomy.Options{Pool: pool, Schema: schema, Tenant: tenant, Kinds: []string{"tag"}, Languages: []string{"en"}})
	must(err)
	rt, err := content.New(ctx, content.Options{
		Pool: pool, Schema: schema, Tenant: tenant,
		Identity: identity{}, Authz: testAuthz{}, Resolver: testResolver{}, Users: testUsers{},
		Moderator: testModerator{}, Classifier: testClassifier{}, Perms: testPerms,
		Media:        &content.Media{URLs: inlineURLs{reg}, Folders: noFolders{}, PostKind: "ckpost", PollKind: "ckpoll"},
		ContentKinds: []string{"video", "gallery", "post"}, Anonymous: anonymous,
		// Generous for a shared suite; a test hits the comment limit with its own caller.
		Limits: content.Limits{Comment: []content.Rate{{Count: 20, Per: time.Minute}}},
	})
	must(err)
	if hooks != nil {
		hooks.content = rt
	}
	mux := http.NewServeMux()
	mount := func(m httpapi.Module, h http.Handler) { mux.Handle(m.Prefix()+"/", http.StripPrefix(m.Prefix(), h)) }
	mux.Handle("/", rt.Handler())
	mount(httpapi.Upload, media.UploadHandler(uploads, media.UploadHandlerOptions{Actor: func(r *http.Request) (access.Actor, bool) {
		a, ok := identity{}.Actor(r.Context())
		return a, ok && !a.Anonymous && a.ID != ""
	}}))
	mount(httpapi.Media, reader.Handler(media.HandlerOptions{Identity: identity{}, Limit: media.RateLimit{Disabled: true}}))
	mount(httpapi.Codes, router.Handler())
	mount(httpapi.Taxonomy, rt.Guard(testPerms.Taxonomy, taxonomy.Handler(tax)))
	return testActor(mux)
}
