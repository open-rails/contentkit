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

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/media"
)

// routes is the app origin: AuthKit, ContentKit at apiPrefix, the bucket's
// presigned uploads, the test-control surface and the built demo apps.
func (h *harness) routes(static string) http.Handler {
	mux := http.NewServeMux()
	if err := h.auth.Mount(mux); err != nil {
		panic(err)
	}
	mux.Handle(apiPrefix+"/", h.faults.api(h.identify(http.StripPrefix(apiPrefix, h.rt.Handler()))))
	mux.Handle(membersPrefix+"/", h.faults.api(h.identify(http.StripPrefix(membersPrefix, h.members.Handler()))))
	upload := media.UploadHandler(h.onUpload, media.UploadHandlerOptions{Actor: func(r *http.Request) (a access.Actor, ok bool) {
		a, ok = identity{}.Actor(r.Context())
		return a, ok && !a.Anonymous
	}})
	mux.Handle(onUploadPrefix+"/media/upload/", h.faults.api(h.identify(http.StripPrefix(onUploadPrefix+"/media/upload", upload))))
	// The bucket, for presigned uploads and the media worker (its S3 endpoint).
	bucket := h.faults.bucket(h.s3Endpoint)
	mux.Handle("/"+h.bucket, bucket)
	mux.Handle("/"+h.bucket+"/", bucket)

	mux.HandleFunc("GET /__test/config", h.config)
	mux.HandleFunc("POST /__test/users", h.createUser)
	mux.HandleFunc("POST /__test/items", h.createItem)
	mux.HandleFunc("PATCH /__test/items/{kind}/{id}", h.updateItem)
	mux.HandleFunc("GET /__test/object", h.object)
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

// createItem: {kind, id?, owner?, access?, hidden?, title?} → the item, with
// a content code when title is given.
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
	if it.ID == "" {
		it.ID = contentref.NewID()
	}
	if it.Access == "" {
		it.Access = "full"
	}
	if it.Access != "full" && it.Access != "none" {
		fail(w, http.StatusBadRequest, fmt.Errorf("access %q: full or none", it.Access))
		return
	}
	ref, err := h.ref(it.Kind, it.ID)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if in.Title != "" {
		links, err := h.urls.Put(r.Context(), contenturl.Entry{ContentRef: contentref.New(tenant, it.Kind, it.ID), Title: in.Title})
		if err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		it.Code = string(links[0].Code)
		if p, ok := h.router.Path(links[0], ""); ok {
			it.Path = p
		}
	}
	h.items.put(ref.Key(), it)
	reply(w, http.StatusCreated, it)
}

// updateItem: {access?, hidden?} changes what viewers may do.
func (h *harness) updateItem(w http.ResponseWriter, r *http.Request) {
	ref, err := h.ref(r.PathValue("kind"), r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	it, ok := h.items.get(ref.Key())
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
	if in.Access != nil {
		it.Access = *in.Access
	}
	if in.Hidden != nil {
		it.Hidden = *in.Hidden
	}
	h.items.put(ref.Key(), it)
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
	ref, err := h.ref(q.Get("kind"), q.Get("id"))
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
