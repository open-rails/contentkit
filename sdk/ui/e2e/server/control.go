package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/helpers/notify"

	contentkit "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/notification"
)

// mount serves an engine's route groups under prefix.
func mount(e *contentkit.Engine, prefix string, groups contentkit.RouteGroups) http.Handler {
	routes, err := e.Routes(contentkit.Routes{Prefix: prefix, Groups: groups})
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	for _, r := range routes {
		mux.Handle(strings.TrimSpace(r.Method+" "+r.Path), r.Handler)
	}
	return mux
}

// routes is the app origin: AuthKit, ContentKit at apiPrefix, the bucket's
// presigned uploads, the test-control surface and the built demo apps.
func (h *harness) routes(static string) http.Handler {
	mux := http.NewServeMux()
	if err := h.auth.Mount(mux); err != nil {
		panic(err)
	}
	mux.Handle(apiPrefix+"/", h.faults.api(behindProxy(mount(h.ck, apiPrefix, h.ck.RouteGroups()))))
	mux.Handle(membersPrefix+"/", h.faults.api(behindProxy(mount(h.members, membersPrefix, h.members.RouteGroups()))))
	mux.Handle(onUploadPrefix+"/", h.faults.api(behindProxy(mount(h.onUpload, onUploadPrefix, contentkit.UploadRoutes))))
	// The bucket, for presigned uploads and the media worker (its S3 endpoint).
	bucket := h.faults.bucket(h.s3Endpoint)
	mux.Handle("/"+h.bucket, bucket)
	mux.Handle("/"+h.bucket+"/", bucket)

	mux.HandleFunc("GET /__test/config", h.config)
	mux.HandleFunc("POST /__test/users", h.createUser)
	mux.HandleFunc("POST /__test/notifications", h.createNotification)
	mux.HandleFunc("POST /__test/items", h.createItem)
	mux.HandleFunc("PATCH /__test/items/{kind}/{id}", h.updateItem)
	mux.HandleFunc("GET /__test/object", h.object)
	mux.HandleFunc("GET /__test/publication", h.publication)
	mux.HandleFunc("GET /__test/outbox", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, h.outbox.Messages("", r.URL.Query().Get("to")))
	})
	mux.HandleFunc("GET /__test/faults", func(w http.ResponseWriter, _ *http.Request) { reply(w, http.StatusOK, h.faults.list()) })
	mux.HandleFunc("POST /__test/faults", h.addFaults)
	mux.HandleFunc("DELETE /__test/faults", func(w http.ResponseWriter, r *http.Request) {
		h.faults.clear(r.URL.Query().Get("item"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /__test/reset", func(w http.ResponseWriter, _ *http.Request) {
		h.faults.clear("")
		h.items.reset()
		w.WriteHeader(http.StatusNoContent)
	})
	if static != "" {
		files := http.FileServer(http.Dir(static))
		// The admin fixture is one page; its flows keep their views under /admin/.
		mux.HandleFunc("/admin/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			http.ServeFile(w, r, path.Join(static, "admin.html"))
		})
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Built assets are content-hashed; the pages are not.
			if strings.HasPrefix(r.URL.Path, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-store")
			}
			files.ServeHTTP(w, r)
		}))
	}
	return mux
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, err error) {
	reply(w, status, map[string]string{"error": err.Error()})
}

func decode(r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// config tells the tests where everything is.
func (h *harness) config(w http.ResponseWriter, _ *http.Request) {
	reply(w, http.StatusOK, map[string]any{
		"origin": h.origin, "media": h.media, "namespace": tenant, "bucket": h.bucket,
		"api": apiPrefix, "on_upload_api": onUploadPrefix, "members_api": membersPrefix, "auth_api": authAPIPath + "/v1",
	})
}

// createUser: {role?: staff|moderator|editor} → a signed-in account (testUser).
func (h *harness) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Role string `json:"role"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	u, err := h.newUser(in.Role)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	reply(w, http.StatusCreated, u)
}

// createItem: {kind, owner?, access?, hidden?, title?} → the item, a
// content item made through the catalog as its authenticated owner (or host
// when none is specified), access "none" pay-per-view, published unless hidden.
func (h *harness) createItem(w http.ResponseWriter, r *http.Request) {
	var in struct {
		item
		Title string `json:"title"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	it := in.item
	if it.Access == "" {
		it.Access = "full"
	}
	if it.ID != "" || (it.Access != "full" && it.Access != "none") {
		fail(w, http.StatusBadRequest, fmt.Errorf("the catalog names items; access %q: full or none", it.Access))
		return
	}
	title := in.Title
	if title == "" {
		title = it.Kind + " " + uuid.NewString()[:8]
	}
	input := catalog.ContentInput{Kind: it.Kind, Text: map[string]catalog.Text{"en": {Title: title}}, Access: itemRule(it.Access)}
	if it.Kind != blogKind {
		input.Versions = []catalog.VersionInput{{Languages: []string{"en"}}}
	}
	ck := h.ck.Client()
	if it.Owner != "" {
		stored, ok := h.users.Load(it.Owner)
		if !ok {
			fail(w, http.StatusBadRequest, errors.New("owner is not a fixture account"))
			return
		}
		ck = contentkit.NewClient(h.origin+apiPrefix, &http.Client{Transport: fixtureCredential{stored.(testUser).AccessToken}})
	}
	c, err := ck.PostContents(r.Context(), input)
	if err == nil && !it.Hidden {
		c, err = ck.PostContentPublish(r.Context(), c.ID, catalog.PublishInput{})
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	it.ID = c.ID
	if len(c.Versions) > 0 {
		it.Version = c.Versions[0].ID
	}
	if in.Title != "" {
		it.Code, it.Path = c.Code, c.Path
	}
	h.items.put(itemKey(it.Kind, it.ID), it)
	reply(w, http.StatusCreated, it)
}

// itemRule is an item's access as a catalog rule.
func itemRule(open string) *access.Rule {
	if open == "none" {
		return &access.Rule{Level: access.PPV}
	}
	return &access.Rule{Level: access.Public}
}

// updateItem: {access?, hidden?} changes what viewers may do.
func (h *harness) updateItem(w http.ResponseWriter, r *http.Request) {
	key := itemKey(r.PathValue("kind"), r.PathValue("id"))
	it, ok := h.items.get(key)
	if !ok {
		fail(w, http.StatusNotFound, errors.New("no such item"))
		return
	}
	var in struct {
		Access *string `json:"access"`
		Hidden *bool   `json:"hidden"`
	}
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	ck := h.ck.Client()
	if in.Access != nil {
		if _, err := ck.PatchContent(r.Context(), it.ID, catalog.ContentPatch{Access: itemRule(*in.Access)}); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		it.Access = *in.Access
	}
	if in.Hidden != nil && *in.Hidden != it.Hidden {
		var err error
		if *in.Hidden {
			_, err = ck.PostContentUnpublish(r.Context(), it.ID)
		} else {
			_, err = ck.PostContentPublish(r.Context(), it.ID, catalog.PublishInput{})
		}
		if err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		it.Hidden = *in.Hidden
	}
	h.items.put(key, it)
	reply(w, http.StatusOK, it)
}

func (h *harness) addFaults(w http.ResponseWriter, r *http.Request) {
	var in []rule
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.faults.add(in)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	reply(w, http.StatusCreated, out)
}

// object: ?kind&id&path (an item's file by path or upload stem, or its
// staged upload until placed) or &public (a public name) → {size, sha256}
// of the stored bytes; 404 when absent.
func (h *harness) object(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ref, err := h.reg.VersionRef(q.Get("kind"), q.Get("id"), q.Get("version"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	it, err := h.reg.Item(ref)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	var key string
	if q.Has("public") {
		key, err = it.Public(q.Get("public"))
	} else {
		key, err = h.fileKey(r.Context(), it, q.Get("path"))
	}
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	rc, _, err := h.store.Get(r.Context(), key, media.GetOptions{})
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	defer rc.Close()
	sum := sha256.New()
	n, err := io.Copy(sum, rc)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"size": n, "sha256": hex.EncodeToString(sum.Sum(nil))})
}

// publication: ?kind&id&version → the version's publication jobs, its
// publications and its item's files, for a test that waits on them.
func (h *harness) publication(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ref, err := h.reg.VersionRef(q.Get("kind"), q.Get("id"), q.Get("version"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	out := map[string]any{}
	collect := func(name, sql string, args ...any) {
		rows, err := h.pool.Query(r.Context(), sql, args...)
		if err != nil {
			out[name] = err.Error()
			return
		}
		got, err := pgx.CollectRows(rows, pgx.RowToMap)
		if err != nil {
			out[name] = err.Error()
			return
		}
		out[name] = got
	}
	collect("jobs", `SELECT kind, queue, state, attempt, errors, scheduled_at FROM `+riverSchema+`.river_job
 WHERE args->>'version' = $1 OR args->>'Version' = $1 ORDER BY id`, q.Get("version"))
	collect("publications", `SELECT id, kind, state, created_at, ready_at FROM `+schema+`.content_version_publications
 WHERE version_id = $1 ORDER BY created_at`, q.Get("version"))
	if m, _, err := h.manifests.Get(r.Context(), ref); err != nil {
		out["files"] = err.Error()
	} else {
		out["files"] = m.Files
	}
	reply(w, http.StatusOK, out)
}

func (h *harness) fileKey(ctx context.Context, it media.Item, p string) (string, error) {
	m, _, err := h.manifests.Get(ctx, it.Ref())
	if err != nil {
		return "", err
	}
	for _, f := range m.Files {
		if f.Path == p || f.IsUpload() && f.Path[:len(f.Path)-len(path.Ext(f.Path))] == p {
			if f.Staged != "" {
				return it.Staged(f.Staged)
			}
			return it.Blob(f.Blob)
		}
	}
	return "", fmt.Errorf("no file %q", p)
}

// createNotification is host-only fixture publication, not a product API.
func (h *harness) createNotification(w http.ResponseWriter, r *http.Request) {
	var in notify.Input
	if err := decode(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if in.CreatedAt.IsZero() {
		in.CreatedAt = time.Now().UTC()
	}
	if in.Origin.System == "" {
		in.Origin = notify.Origin{System: "fixture-host", Scope: tenant, ID: uuid.NewString()}
	}
	published, err := h.ck.Client().PostNotificationsPublish(r.Context(), notification.Publication{Notifications: []notify.Input{in}})
	items := published.Items
	if err == nil && len(items) == 0 {
		err = errors.New("the recipient was erased")
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	reply(w, http.StatusCreated, items[0])
}

// fixtureCredential sends a real AuthKit credential through the public routes.
type fixtureCredential struct{ token string }

func (f fixtureCredential) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+f.token)
	return http.DefaultTransport.RoundTrip(r)
}
