package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/media"
)

// totalsPath is the totals route under a ContentKit mount.
var totalsPath = httpapi.Contents.Prefix() + "/catalog/totals"

func init() { httpapi.Register(httpapi.Contents, routes) }

const editorsDoc = " Creators, the owner group's editors (its owner, holders of {kind}:content:write in its AuthKit scope) and root:content:write holders only."

var languageParam = httpapi.Text("language", "the language to show text in; the site's first language, then any, when it has none")

var routes = []httpapi.Route[*Store]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/catalog/totals", Resource: "catalog", Auth: httpapi.Open,
		Doc: "Each kind's content, in all and per language: how many items an anonymous visitor's listing shows and opens, and how many each tier's holders open. The request's site scope applies. Cacheable for ten minutes.",
		Query: []httpapi.Param{httpapi.List("kinds", "content kinds to total; every kind by default"),
			httpapi.List("languages", "languages to break the totals down by; every counted language by default")},
		Responses: []httpapi.Reply{httpapi.OK(Totals{})}, Errors: []string{httpapi.CodeUnavailable}},
		Serve: httpapi.H((*Store).handleTotals)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/contents/{id}", Resource: "contents", Auth: httpapi.Public,
		Doc:   "A content item: its text in the viewer's language, live versions with their segments and access, groups and attributes. Editors see every version, every language and the moderation state.",
		Query: []httpapi.Param{languageParam}, Responses: []httpapi.Reply{httpapi.OK(Content{})}, Errors: []string{httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handleGet)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/contents/{id}/versions/{vid}", Resource: "contents", Auth: httpapi.Public,
		Doc:   "One version of a content item as the viewer sees it, with its segments and access decision.",
		Query: []httpapi.Param{languageParam}, Responses: []httpapi.Reply{httpapi.OK(Version{})}, Errors: []string{httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handleGetVersion)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/manage/contents", Resource: "contents", Auth: httpapi.Public,
		Doc: "Content items to manage, newest first: drafts, scheduled, held and live ones; deleted ones on their own. root:content:write holders see all; others what they created, or an owner group's they edit.",
		Query: append([]httpapi.Param{httpapi.Text("kind", "only this kind"),
			httpapi.Text("state", "draft, scheduled, published (live) or deleted; every state but deleted by default"),
			httpapi.Text("owner_group", "only this owner group's, which the caller edits"),
			httpapi.Text("q", "only items whose title, description or body contains this text, ignoring case"), languageParam}, httpapi.Page...),
		Responses: []httpapi.Reply{httpapi.OK([]Content{})}},
		Serve: httpapi.H((*Store).handleManage)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/contents", Resource: "contents", Auth: httpapi.Public,
		Doc:     "Creates a content item as a draft. root:content:write holders, or an owner group's editors (its owner, holders of {kind}:content:write in its AuthKit scope) in it.",
		Request: ContentInput{}, Responses: []httpapi.Reply{httpapi.Created(Content{})}, Errors: []string{httpapi.CodeForbidden}},
		Serve: httpapi.H((*Store).handleCreate)},
	{Spec: httpapi.Spec{Method: httpapi.PATCH, Path: "/contents/{id}", Resource: "contents", Auth: httpapi.Public,
		Doc:     "Edits a content item's listing, access, restrictions, attributes, cover or creators." + editorsDoc,
		Request: ContentPatch{}, Responses: []httpapi.Reply{httpapi.OK(Content{})},
		Errors: []string{httpapi.CodeConflict, httpapi.CodeForbidden, httpapi.CodeNotFound, "not_configured"}},
		Serve: httpapi.H((*Store).handleUpdate)},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/contents/{id}", Resource: "contents", Auth: httpapi.Public,
		Doc:       "Deletes a content item: holders of its key still read it; one never sold is erased after the retention." + editorsDoc,
		Responses: []httpapi.Reply{httpapi.NoContent}, Errors: []string{httpapi.CodeForbidden, httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handleDelete)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/contents/{id}/restore", Resource: "contents", Auth: httpapi.Public,
		Doc:       "Restores a deleted content item as it was." + editorsDoc,
		Responses: []httpapi.Reply{httpapi.OK(Content{})}, Errors: []string{httpapi.CodeForbidden, httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handleRestore)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/contents/{id}/publish", Resource: "contents", Auth: httpapi.Public,
		Doc:     "Publishes a content item now, or schedules it for publish_at; 202 when the moderator holds it for review." + editorsDoc,
		Request: PublishInput{}, Responses: []httpapi.Reply{httpapi.OK(Content{}), httpapi.Accepted(Content{})},
		Errors: []string{httpapi.CodeConflict, httpapi.CodeForbidden, httpapi.CodeNotFound, "moderation_rejected"}},
		Serve: httpapi.H((*Store).handlePublish)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/contents/{id}/unpublish", Resource: "contents", Auth: httpapi.Public,
		Doc:       "Returns a published or scheduled content item to a draft." + editorsDoc,
		Responses: []httpapi.Reply{httpapi.OK(Content{})}, Errors: []string{httpapi.CodeConflict, httpapi.CodeForbidden, httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handleUnpublish)},
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/contents/{id}/text/{language}", Resource: "contents", Auth: httpapi.Public,
		Doc:     "Sets a content item's text in one language; 202 when the moderator holds a live item's edit (its last approved text stays public)." + editorsDoc,
		Request: Text{}, Responses: []httpapi.Reply{httpapi.OK(Content{}), httpapi.Accepted(Content{})},
		Errors: []string{httpapi.CodeConflict, httpapi.CodeForbidden, httpapi.CodeNotFound, "moderation_rejected"}},
		Serve: httpapi.H((*Store).handlePutText)},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/contents/{id}/text/{language}", Resource: "contents", Auth: httpapi.Public,
		Doc:       "Removes a content item's text in one language." + editorsDoc,
		Responses: []httpapi.Reply{httpapi.OK(Content{})}, Errors: []string{httpapi.CodeConflict, httpapi.CodeForbidden, httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handleDeleteText)},
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/contents/{id}/creators", Resource: "contents", Auth: httpapi.Public,
		Doc:     "Replaces a content item's creators." + editorsDoc,
		Request: CreatorsInput{}, Responses: []httpapi.Reply{httpapi.OK(Content{})}, Errors: []string{httpapi.CodeForbidden, httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handlePutCreators)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/contents/{id}/images", Resource: "contents", Auth: httpapi.Public,
		Doc:     "The reference to store in a body for an inline image uploaded to the item's media folder, and a URL that shows it to the editor now." + editorsDoc,
		Request: content.ImageInput{}, Responses: []httpapi.Reply{httpapi.OK(InlineImage{})}, Errors: []string{httpapi.CodeForbidden, httpapi.CodeNotFound, "not_configured"}},
		Serve: httpapi.H((*Store).handleImage)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/contents/{id}/versions", Resource: "contents", Auth: httpapi.Public,
		Doc:     "Adds a version after the others." + editorsDoc,
		Request: VersionInput{}, Responses: []httpapi.Reply{httpapi.Created(Version{})}, Errors: []string{httpapi.CodeForbidden, httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handleCreateVersion)},
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/contents/{id}/versions/order", Resource: "contents", Auth: httpapi.Public,
		Doc:     "Orders a content item's versions: ids lists each version that is not deleted, first first." + editorsDoc,
		Request: OrderInput{}, Responses: []httpapi.Reply{httpapi.OK(Content{})}, Errors: []string{httpapi.CodeForbidden, httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handleOrderVersions)},
	{Spec: httpapi.Spec{Method: httpapi.PATCH, Path: "/contents/{id}/versions/{vid}", Resource: "contents", Auth: httpapi.Public,
		Doc:     "Edits a version: languages, traits, label, state and schedule, access, attributes, text and segments." + editorsDoc,
		Request: VersionInput{}, Responses: []httpapi.Reply{httpapi.OK(Version{})}, Errors: []string{httpapi.CodeForbidden, httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handleUpdateVersion)},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/contents/{id}/versions/{vid}", Resource: "contents", Auth: httpapi.Public,
		Doc:       "Deletes a version; a content item keeps at least one." + editorsDoc,
		Responses: []httpapi.Reply{httpapi.NoContent}, Errors: []string{httpapi.CodeConflict, httpapi.CodeForbidden, httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handleDeleteVersion)},
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/contents/{id}/versions/{vid}/segments", Resource: "contents", Auth: httpapi.Public,
		Doc:     "Replaces a version's segments (chapters): ordered ranges, each released at its time." + editorsDoc,
		Request: SegmentsInput{}, Responses: []httpapi.Reply{httpapi.OK(Version{})}, Errors: []string{httpapi.CodeForbidden, httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handlePutSegments)},
}

// PublishInput publishes now, or at PublishAt when it is in the future.
type PublishInput struct {
	PublishAt *time.Time `json:"publish_at,omitempty"`
}

// CreatorsInput replaces the creators.
type CreatorsInput struct {
	Creators []Creator `json:"creators"`
}

// OrderInput orders versions.
type OrderInput struct {
	IDs []string `json:"ids"`
}

// SegmentsInput replaces a version's segments.
type SegmentsInput struct {
	Segments []Segment `json:"segments"`
}

// InlineImage is an inline image of a body: Ref goes where its URL would
// ("contentkit:i-{uuid}"); URL shows it to the editor now (null until it has
// a file to show).
type InlineImage struct {
	Ref string  `json:"ref"`
	URL *string `json:"url"`
}

// Handler serves the catalog routes behind their gates over Options.Auth,
// each request in an access.WithMemo context (one billing read). Its paths
// are under the module's prefix (httpapi.Contents, /v1): mount it with that
// prefix stripped.
func (s *Store) Handler() http.Handler {
	mux := http.NewServeMux()
	httpapi.Mount(mux, s.guard.Guard, s, routes)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		mux.ServeHTTP(sw, r.WithContext(access.WithMemo(r.Context())))
		attrs := []any{"method", r.Method, "path", r.URL.Path, "status", sw.status, "duration", time.Since(start)}
		if sw.status >= http.StatusInternalServerError {
			if sw.cause != nil {
				attrs = append(attrs, "err", sw.cause.Error())
			}
			s.log.Error("catalog request failed", attrs...)
			return
		}
		s.log.Debug("catalog request", attrs...)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	cause  error
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writeErr(w http.ResponseWriter, err error) {
	status, code, msg := classify(err)
	if status >= http.StatusInternalServerError {
		if sw, ok := w.(*statusWriter); ok {
			sw.cause = err
		}
	}
	writeJSON(w, status, errorBody{Error: msg, Code: code})
}

func classify(err error) (int, string, string) {
	var ae *AuthError
	var rej content.RejectedError
	switch {
	case errors.As(err, &ae):
		if errors.Is(err, auth.ErrExpired) || errors.Is(err, auth.ErrRevoked) {
			return http.StatusUnauthorized, httpapi.CodeUnauthorized, "authentication required"
		}
		return http.StatusServiceUnavailable, httpapi.CodeUnavailable, "authentication is unavailable; retry"
	case errors.Is(err, errUnauthenticated):
		return http.StatusUnauthorized, httpapi.CodeUnauthorized, "authentication required"
	case errors.Is(err, ErrInvalid):
		return http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error()
	case errors.Is(err, ErrNotFound), errors.Is(err, content.ErrNotFound):
		return http.StatusNotFound, httpapi.CodeNotFound, "not found"
	case errors.Is(err, ErrForbidden), errors.Is(err, content.ErrSubjectErased):
		return http.StatusForbidden, httpapi.CodeForbidden, "forbidden"
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, httpapi.CodeConflict, err.Error()
	case errors.As(err, &rej):
		return http.StatusUnprocessableEntity, "moderation_rejected", rej.Reason
	case errors.Is(err, errMediaNotConfigured):
		return http.StatusNotImplemented, "not_configured", "images need ContentKit media"
	case errors.Is(err, access.ErrUnavailable):
		return http.StatusServiceUnavailable, httpapi.CodeUnavailable, "entitlements are unavailable; retry"
	}
	return http.StatusInternalServerError, httpapi.CodeInternal, "internal error"
}

var errUnauthenticated = errors.New("catalog: authentication required")

func decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return invalid("invalid JSON body: %v", err)
	}
	return nil
}

// writer is the request's actor, which must be signed in to write.
func writer(ctx context.Context) (access.Actor, error) {
	a := gate.Actor(ctx)
	if a.Anonymous || a.ID == "" {
		return a, errUnauthenticated
	}
	return a, nil
}

// viewer builds the request's Viewer: its host's site, its language.
func (s *Store) viewer(r *http.Request, o ViewerOptions) (*Viewer, error) {
	o.Actor = gate.Actor(r.Context())
	o.Host = r.Host
	if o.Language == "" {
		o.Language = s.requestLanguage(r)
	}
	return s.Viewer(r.Context(), o)
}

// requestLanguage is the language query parameter, else the first site
// language Accept-Language names; "" when neither.
func (s *Store) requestLanguage(r *http.Request) string {
	if l := r.URL.Query().Get("language"); l != "" {
		return l
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		tag = strings.ToLower(tag)
		for _, l := range s.cfg.Languages {
			if tag == l || strings.HasPrefix(tag, l+"-") {
				return l
			}
		}
	}
	return ""
}

// respond answers the item as the actor now sees it.
func (s *Store) respond(w http.ResponseWriter, r *http.Request, status int, id string) {
	v, err := s.viewer(r, ViewerOptions{})
	if err != nil {
		writeErr(w, err)
		return
	}
	got, err := s.Contents(r.Context(), v, []string{id})
	if err != nil {
		writeErr(w, err)
		return
	}
	if got[0] == nil {
		writeErr(w, ErrNotFound)
		return
	}
	if status == http.StatusOK && got[0].Moderation == content.ModerationHeld && r.Method != http.MethodGet {
		status = http.StatusAccepted
	}
	writeJSON(w, status, got[0])
}

func (s *Store) handleTotals(w http.ResponseWriter, r *http.Request) {
	var q TotalsQuery
	for _, k := range strings.Split(strings.Join(r.URL.Query()["kinds"], ","), ",") {
		if k = strings.TrimSpace(k); k != "" {
			q.Kinds = append(q.Kinds, k)
		}
	}
	for _, l := range strings.Split(strings.Join(r.URL.Query()["languages"], ","), ",") {
		if l = strings.TrimSpace(l); l != "" {
			q.Languages = append(q.Languages, l)
		}
	}
	host := r.Host
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	site, _ := s.cfg.SiteFor(strings.ToLower(host))
	q.Site = site.Name
	t, err := s.Totals(r.Context(), q)
	switch {
	case errors.Is(err, ErrInvalid):
		writeErr(w, err)
	case err != nil:
		if sw, ok := w.(*statusWriter); ok {
			sw.cause = err
		}
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "catalog totals are unavailable", Code: httpapi.CodeUnavailable})
	default:
		w.Header().Set("Cache-Control", "public, max-age=600")
		writeJSON(w, http.StatusOK, t)
	}
}

func (s *Store) handleGet(w http.ResponseWriter, r *http.Request) {
	s.respond(w, r, http.StatusOK, r.PathValue("id"))
}

func (s *Store) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	v, err := s.viewer(r, ViewerOptions{})
	if err != nil {
		writeErr(w, err)
		return
	}
	got, err := s.Contents(r.Context(), v, []string{r.PathValue("id")})
	if err != nil {
		writeErr(w, err)
		return
	}
	if got[0] != nil {
		for _, ver := range got[0].Versions {
			if ver.ID == r.PathValue("vid") {
				writeJSON(w, http.StatusOK, ver)
				return
			}
		}
	}
	writeErr(w, ErrNotFound)
}

func (s *Store) handleCreate(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	var in ContentInput
	if err == nil {
		err = decode(r, &in)
	}
	var ids []string
	if err == nil {
		ids, err = s.Create(r.Context(), actor, []ContentInput{in})
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	s.respond(w, r, http.StatusCreated, ids[0])
}

func (s *Store) handleUpdate(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	var in ContentPatch
	if err == nil {
		err = decode(r, &in)
	}
	if err == nil {
		err = s.Update(r.Context(), actor, []Update{{ID: r.PathValue("id"), ContentPatch: in}})
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	s.respond(w, r, http.StatusOK, r.PathValue("id"))
}

func (s *Store) handleDelete(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	if err == nil {
		err = s.Delete(r.Context(), actor, []string{r.PathValue("id")})
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *Store) handleRestore(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	if err == nil {
		err = s.Restore(r.Context(), actor, []string{r.PathValue("id")})
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	s.respond(w, r, http.StatusOK, r.PathValue("id"))
}

func (s *Store) handlePublish(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	var in PublishInput
	if err == nil && r.ContentLength != 0 {
		err = decode(r, &in)
	}
	if err == nil {
		err = s.Publish(r.Context(), actor, []Publish{{ContentID: r.PathValue("id"), At: in.PublishAt}})
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	s.respond(w, r, http.StatusOK, r.PathValue("id"))
}

func (s *Store) handleUnpublish(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	if err == nil {
		err = s.Unpublish(r.Context(), actor, []string{r.PathValue("id")})
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	s.respond(w, r, http.StatusOK, r.PathValue("id"))
}

func (s *Store) handlePutText(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	var in Text
	if err == nil {
		err = decode(r, &in)
	}
	if err == nil {
		err = s.PutText(r.Context(), actor, []TextPut{{ContentID: r.PathValue("id"), Language: r.PathValue("language"), Text: &in}})
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	s.respond(w, r, http.StatusOK, r.PathValue("id"))
}

func (s *Store) handleDeleteText(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	if err == nil {
		err = s.PutText(r.Context(), actor, []TextPut{{ContentID: r.PathValue("id"), Language: r.PathValue("language")}})
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	s.respond(w, r, http.StatusOK, r.PathValue("id"))
}

func (s *Store) handlePutCreators(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	var in CreatorsInput
	if err == nil {
		err = decode(r, &in)
	}
	if err == nil {
		cs := orEmpty(in.Creators)
		err = s.Update(r.Context(), actor, []Update{{ID: r.PathValue("id"), ContentPatch: ContentPatch{Creators: &cs}}})
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	s.respond(w, r, http.StatusOK, r.PathValue("id"))
}

func (s *Store) handleImage(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	var in content.ImageInput
	if err == nil {
		err = decode(r, &in)
	}
	if err == nil && (in.Image == nil || !media.ValidNamed(*in.Image)) {
		err = invalid("image must be an inline image name (i-{uuid})")
	}
	if err == nil && s.media == nil {
		err = errMediaNotConfigured
	}
	var got map[string]owned
	if err == nil {
		got, err = s.lookup(r.Context(), actor, r.PathValue("id"))
	}
	var url string
	if err == nil {
		url, err = s.previewURL(r.Context(), actor, got[r.PathValue("id")].kind, r.PathValue("id"), *in.Image)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	out := InlineImage{Ref: ImageRef(*in.Image)}
	if url != "" {
		out.URL = &url
	}
	writeJSON(w, http.StatusOK, out)
}

// lookup authorizes actor to edit one live item without writing.
func (s *Store) lookup(ctx context.Context, actor access.Actor, id string) (map[string]owned, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	got, err := s.authorize(ctx, tx, actor, []string{id})
	if err == nil && got[id].state == StateDeleted {
		err = ErrNotFound
	}
	return got, err
}

func (s *Store) handleCreateVersion(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	var in VersionInput
	if err == nil {
		err = decode(r, &in)
	}
	var ids []string
	if err == nil {
		ids, err = s.PutVersions(r.Context(), actor, []VersionPut{{ContentID: r.PathValue("id"), VersionInput: in}})
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	s.respondVersion(w, r, http.StatusCreated, ids[0])
}

func (s *Store) handleUpdateVersion(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	var in VersionInput
	if err == nil {
		err = decode(r, &in)
	}
	if err == nil {
		_, err = s.PutVersions(r.Context(), actor, []VersionPut{{ContentID: r.PathValue("id"), VersionID: r.PathValue("vid"), VersionInput: in}})
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	s.respondVersion(w, r, http.StatusOK, r.PathValue("vid"))
}

func (s *Store) handlePutSegments(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	var in SegmentsInput
	if err == nil {
		err = decode(r, &in)
	}
	if err == nil {
		segs := orEmpty(in.Segments)
		_, err = s.PutVersions(r.Context(), actor, []VersionPut{{ContentID: r.PathValue("id"), VersionID: r.PathValue("vid"), VersionInput: VersionInput{Segments: &segs}}})
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	s.respondVersion(w, r, http.StatusOK, r.PathValue("vid"))
}

func (s *Store) respondVersion(w http.ResponseWriter, r *http.Request, status int, vid string) {
	v, err := s.viewer(r, ViewerOptions{})
	if err != nil {
		writeErr(w, err)
		return
	}
	got, err := s.Contents(r.Context(), v, []string{r.PathValue("id")})
	if err != nil {
		writeErr(w, err)
		return
	}
	if got[0] != nil {
		for _, ver := range got[0].Versions {
			if ver.ID == vid {
				writeJSON(w, status, ver)
				return
			}
		}
	}
	writeErr(w, ErrNotFound)
}

func (s *Store) handleDeleteVersion(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	if err == nil {
		err = s.DeleteVersions(r.Context(), actor, []VersionKey{{ContentID: r.PathValue("id"), VersionID: r.PathValue("vid")}})
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (s *Store) handleOrderVersions(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	var in OrderInput
	if err == nil {
		err = decode(r, &in)
	}
	if err == nil {
		err = s.OrderVersions(r.Context(), actor, r.PathValue("id"), in.IDs)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	s.respond(w, r, http.StatusOK, r.PathValue("id"))
}

func (s *Store) handleManage(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	q := r.URL.Query()
	limit, offset := page(r)
	var ids []string
	if err == nil {
		ids, err = s.Manage(r.Context(), actor, ManageQuery{Kind: q.Get("kind"), State: q.Get("state"), OwnerGroup: q.Get("owner_group"),
			Text: q.Get("q"), Limit: limit, Offset: offset})
	}
	var v *Viewer
	if err == nil {
		v, err = s.viewer(r, ViewerOptions{})
	}
	var got []*Content
	if err == nil {
		got, err = s.Contents(r.Context(), v, ids)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	out := []Content{}
	for _, c := range got {
		if c != nil {
			out = append(out, *c)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func page(r *http.Request) (limit, offset int) {
	limit = 20
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 100)
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}
	return limit, offset
}

// ManageQuery selects content items to manage.
type ManageQuery struct {
	Kind string
	// State is draft, scheduled, published (live) or deleted; "" is every
	// state but deleted.
	State      string
	OwnerGroup string
	// Text matches title, description or body, ignoring case.
	Text          string
	Limit, Offset int
}

// Manage lists the ids of items actor manages, newest first: every item for
// root:content:write holders; else what they created, or with OwnerGroup the
// group's items when they edit it.
func (s *Store) Manage(ctx context.Context, actor access.Actor, q ManageQuery) ([]string, error) {
	rights, err := s.staffRights(ctx, actor)
	if err != nil {
		return nil, err
	}
	group := false
	if q.OwnerGroup != "" && !rights.write {
		editors, err := s.groupEditor(ctx, s.pool, actor, []string{q.OwnerGroup})
		if err != nil {
			return nil, err
		}
		if !editors[q.OwnerGroup] {
			return nil, ErrForbidden
		}
		group = true
	}
	var state string
	switch q.State {
	case "":
		state = "c.state <> 'deleted'"
	case StateDraft, StateDeleted:
		state = "c.state = '" + q.State + "'"
	case "scheduled":
		state = "c.state = 'published' AND c.published_at IS NULL"
	case StatePublished:
		state = "c.state = 'published' AND c.published_at IS NOT NULL"
	default:
		return nil, invalid("state %q must be draft, scheduled, published or deleted", q.State)
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id FROM `+s.t.items+` c WHERE c.tenant_id = $1 AND `+state+`
 AND ($2 = '' OR c.kind = $2) AND ($3 = '' OR c.owner_group_id = $3)
 AND ($4 OR $5 OR EXISTS (SELECT 1 FROM `+s.t.creators+` cc WHERE cc.tenant_id = c.tenant_id AND cc.content_id = c.id AND cc.user_id = $6))
 AND ($7 = '' OR EXISTS (SELECT 1 FROM `+s.t.text+` t WHERE t.tenant_id = c.tenant_id AND t.content_id = c.id
  AND (strpos(lower(t.title), lower($7)) > 0 OR strpos(lower(t.description), lower($7)) > 0 OR strpos(lower(t.body), lower($7)) > 0)))
 ORDER BY c.created_at DESC, c.id DESC LIMIT $8 OFFSET $9`,
		s.tenant, q.Kind, q.OwnerGroup, rights.write, group, viewerID(actor), strings.TrimSpace(q.Text), q.Limit, q.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
