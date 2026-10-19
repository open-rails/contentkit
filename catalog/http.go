package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
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
		Doc:   "A content item: its text in the viewer's language, live versions with their segments and access, groups and attributes; the body only to viewers who open a version, everyone else its description and the paywall decision. Editors see every version, every language and the moderation state.",
		Query: []httpapi.Param{languageParam}, Responses: []httpapi.Reply{httpapi.OK(Content{})}, Errors: []string{httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handleGet)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/contents/{id}/versions/{vid}", Resource: "contents", Auth: httpapi.Public,
		Doc:   "One version of a content item as the viewer sees it, with its segments and access decision; vid is its id or code.",
		Query: []httpapi.Param{languageParam}, Responses: []httpapi.Reply{httpapi.OK(Version{})}, Errors: []string{httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handleGetVersion)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/manage", Resource: "contents", Auth: httpapi.Public,
		Doc: "Content items to manage, newest first: drafts, scheduled, held and live ones; deleted ones on their own. root:content:write holders see all; others what they created, or an owner group's they edit.",
		Query: append([]httpapi.Param{httpapi.Text("state", "draft, scheduled, published (live) or deleted; every state but deleted by default"),
			httpapi.Text("owner_group", "only this owner group's, which the caller edits"),
			httpapi.Text("access", "only items whose own rule has this level: public, tier, group, ppv or group_ppv"), httpapi.Text("tier", "only items whose rule names this configured tier"),
			httpapi.Text("q", "only items whose title, description or body contains this text, ignoring case"), httpapi.Text("code", "only the item with this content code (any spelling), in every state unless state narrows it"),
			httpapi.List("ids", "only these content ids (at most 100), in every state unless state narrows it"), languageParam}, httpapi.Page...),
		Responses: []httpapi.Reply{httpapi.OK([]Content{})}},
		Serve: httpapi.H((*Store).handleManage)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/manage/{kind}", Resource: "contents", Auth: httpapi.Public,
		Doc: "Content items to manage for one configured kind. Uses the same permissions and filters as the cross-kind /manage listing. Unknown kinds return not_found.",
		Query: append([]httpapi.Param{httpapi.Text("state", "draft, scheduled, published or deleted"),
			httpapi.Text("owner_group", "only this owner group's, which the caller edits"),
			httpapi.Text("access", "only items whose own rule has this level: public, tier, group, ppv or group_ppv"), httpapi.Text("tier", "only items whose rule names this configured tier"),
			httpapi.Text("q", "only items whose title, description or body contains this text, ignoring case"), httpapi.Text("code", "only the item with this content code (any spelling), in every state unless state narrows it"),
			httpapi.List("ids", "only these content ids (at most 100), in every state unless state narrows it"), languageParam}, httpapi.Page...),
		Responses: []httpapi.Reply{httpapi.OK([]Content{})}, Errors: []string{httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handleManage)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/manage/{kind}/count", Resource: "contents", Auth: httpapi.Public,
		Doc: "How many items of one kind the caller manages under the /manage/{kind} filters: exact up to 10,000; beyond that, truncated marks a lower bound.",
		Query: []httpapi.Param{httpapi.Text("state", "draft, scheduled, published or deleted"), httpapi.Text("owner_group", "only this owner group's, which the caller edits"),
			httpapi.Text("access", "only items whose own rule has this level: public, tier, group, ppv or group_ppv"), httpapi.Text("tier", "only items whose rule names this configured tier"),
			httpapi.Text("q", "only items whose title, description or body contains this text, ignoring case")},
		Responses: []httpapi.Reply{httpapi.OK(ManageCount{})}, Errors: []string{httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handleManageCount)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/contents", Resource: "contents", Auth: httpapi.Public,
		Doc:     "Creates a content item as a draft, with one version serving its text's languages when it names none. root:content:write holders, or an owner group's editors (its owner, holders of {kind}:content:write in its AuthKit scope) in it.",
		Request: ContentInput{}, Responses: []httpapi.Reply{httpapi.Created(Content{})}, Errors: []string{httpapi.CodeForbidden}},
		Serve: httpapi.H((*Store).handleCreate)},
	{Spec: httpapi.Spec{Method: httpapi.PATCH, Path: "/contents/{id}", Resource: "contents", Auth: httpapi.Public,
		Doc:     "Edits a content item's listing, access, restrictions, attributes or creators." + editorsDoc,
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
		Doc:     "Publishes a content item now, or schedules it for publish_at; 202 when the moderator holds it for review. Refused (not_ready) while a published version's media is still processing or being published." + editorsDoc,
		Request: PublishInput{}, Responses: []httpapi.Reply{httpapi.OK(Content{}), httpapi.Accepted(Content{})},
		Errors: []string{httpapi.CodeConflict, httpapi.CodeForbidden, httpapi.CodeNotFound, "moderation_rejected", "not_ready"}},
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
		Doc: "Takes a body image uploaded to one version (its private files): copies it into every other version, since versions share no files, " +
			"and returns the reference to store in the body and a URL that shows it to the editor now." + editorsDoc,
		Request: BodyImageInput{}, Responses: []httpapi.Reply{httpapi.OK(InlineImage{})}, Errors: []string{httpapi.CodeForbidden, httpapi.CodeNotFound, "not_configured"}},
		Serve: httpapi.H((*Store).handleImage)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/contents/{id}/versions", Resource: "contents", Auth: httpapi.Public,
		Doc:     "Adds a version after the others, a draft unless state says otherwise." + editorsDoc,
		Request: VersionInput{}, Responses: []httpapi.Reply{httpapi.Created(Version{})}, Errors: []string{httpapi.CodeForbidden, httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handleCreateVersion)},
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/contents/{id}/versions/order", Resource: "contents", Auth: httpapi.Public,
		Doc:     "Orders a content item's versions: ids lists each version that is not deleted, first first." + editorsDoc,
		Request: OrderInput{}, Responses: []httpapi.Reply{httpapi.OK(Content{})}, Errors: []string{httpapi.CodeForbidden, httpapi.CodeNotFound}},
		Serve: httpapi.H((*Store).handleOrderVersions)},
	{Spec: httpapi.Spec{Method: httpapi.PATCH, Path: "/contents/{id}/versions/{vid}", Resource: "contents", Auth: httpapi.Public,
		Doc:     "Edits a version: languages, traits, label, state and schedule, access, attributes, text, segments and cover; a hidden version takes only its cover. Publishing it on a published item is refused (not_ready) while its media is still processing." + editorsDoc,
		Request: VersionInput{}, Responses: []httpapi.Reply{httpapi.OK(Version{})}, Errors: []string{httpapi.CodeForbidden, httpapi.CodeNotFound, "not_ready"}},
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
	Segments []SegmentInput `json:"segments"`
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
		if sw.cause != nil {
			attrs = append(attrs, "err", sw.cause.Error())
		}
		if sw.status >= http.StatusInternalServerError {
			s.log.Error("catalog request failed", attrs...)
			return
		}
		s.log.Debug("catalog request", attrs...)
	})
}

// statusWriter records the status and the cause writeErr kept off the wire.
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
	if sw, ok := w.(*statusWriter); ok {
		sw.cause = err
	}
	writeJSON(w, status, errorBody{Error: msg, Code: code})
}

func classify(err error) (int, string, string) {
	var ae *AuthError
	var rej content.RejectedError
	var notReady *NotReadyError
	switch {
	case errors.As(err, &ae):
		if errors.Is(err, auth.ErrExpired) || errors.Is(err, auth.ErrRevoked) {
			return http.StatusUnauthorized, httpapi.CodeUnauthorized, "authentication required"
		}
		return http.StatusServiceUnavailable, httpapi.CodeUnavailable, "authentication is unavailable; retry"
	case errors.Is(err, errUnauthenticated):
		return http.StatusUnauthorized, httpapi.CodeUnauthorized, "authentication required"
	case errors.Is(err, ErrInvalid):
		return http.StatusBadRequest, httpapi.CodeInvalidRequest, httpapi.Rule(err, httpapi.CodeInvalidRequest)
	case errors.Is(err, ErrNotFound), errors.Is(err, content.ErrNotFound):
		return http.StatusNotFound, httpapi.CodeNotFound, "not found"
	case errors.Is(err, ErrForbidden), errors.Is(err, content.ErrSubjectErased):
		return http.StatusForbidden, httpapi.CodeForbidden, "forbidden"
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, httpapi.CodeConflict, httpapi.Message(httpapi.CodeConflict)
	case errors.As(err, &rej):
		return http.StatusUnprocessableEntity, "moderation_rejected", rej.Reason
	case errors.As(err, &notReady):
		return http.StatusConflict, "not_ready", notReady.Error()
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
		return invalid("%s", httpapi.BodyError(err))
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
	return contenturl.AcceptLanguage(s.cfg.Languages, r.Header.Get("Accept-Language"))
}

// respond answers the item as the actor now sees it.
func (s *Store) respond(w http.ResponseWriter, r *http.Request, status int, id string) {
	v, err := s.viewer(r, ViewerOptions{})
	if err != nil {
		writeErr(w, err)
		return
	}
	v.management = r.Method != http.MethodGet && r.Method != http.MethodHead
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
	if r.Method == http.MethodGet {
		if err := s.fillStanding(r.Context(), v.actor, got); err != nil {
			writeErr(w, err)
			return
		}
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
	site, _ := s.cfg.DomainFor(strings.ToLower(host))
	q.Domain = site.Name
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
	vid := r.PathValue("vid")
	got, err := s.contents(r.Context(), v, []string{r.PathValue("id")}, vid)
	if err != nil {
		writeErr(w, err)
		return
	}
	if c := got[0]; c != nil {
		code, _ := contenturl.ParseVersionCode(vid)
		i := slices.IndexFunc(c.Versions, func(v Version) bool { return v.ID == vid })
		if i < 0 && code != "" {
			i = slices.IndexFunc(c.Versions, func(v Version) bool { return v.Code == string(code) })
		}
		if i >= 0 {
			c.Versions = c.Versions[i : i+1]
			if err := s.fillStanding(r.Context(), v.actor, got); err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, c.Versions[0])
			return
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
	var in BodyImageInput
	if err == nil {
		err = decode(r, &in)
	}
	var out InlineImage
	if err == nil {
		out, err = s.BodyImage(r.Context(), actor, r.PathValue("id"), in)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
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
	v.management = true
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
	kind := r.PathValue("kind")
	if err == nil && q.Has("kind") {
		err = invalid("select the content kind in the path")
	}
	if kind != "" && err == nil {
		if _, ok := s.cfg.Kinds[kind]; !ok {
			err = ErrNotFound
		}
	}
	limit, offset := page(r)
	var ids []string
	if err == nil {
		var only []string
		for _, id := range strings.Split(strings.Join(q["ids"], ","), ",") {
			if id = strings.TrimSpace(id); id != "" {
				only = append(only, id)
			}
		}
		ids, err = s.Manage(r.Context(), actor, ManageQuery{Kind: kind, State: q.Get("state"), OwnerGroup: q.Get("owner_group"),
			Text: q.Get("q"), Code: q.Get("code"), Access: q.Get("access"), Tier: q.Get("tier"), IDs: only, Limit: limit, Offset: offset})
	}
	var v *Viewer
	if err == nil {
		v, err = s.viewer(r, ViewerOptions{})
		if v != nil {
			v.management = true
		}
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
	Text string
	// Code selects the item with this content code; with State "" it matches
	// deleted items too, so editor links keep resolving.
	Code string
	// IDs selects these items (at most MaxManageIDs); with State "" it
	// matches deleted items too, so member lists resolve every member.
	IDs []string
	// Access keeps items whose own rule has this level; Tier those whose rule
	// names this configured tier (implying the tier level).
	Access, Tier  string
	Limit, Offset int
}

// MaxManageIDs bounds ManageQuery.IDs.
const MaxManageIDs = 100

// Manage lists the ids of items actor manages, newest first: every item for
// root:content:write holders; else what they created, or with OwnerGroup the
// group's items when they edit it.
func (s *Store) Manage(ctx context.Context, actor access.Actor, q ManageQuery) ([]string, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	query, args, err := s.manageSelection(ctx, actor, q)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, query+" ORDER BY c.created_at DESC, c.id DESC LIMIT $8 OFFSET $9", args...)
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

// manageSelection keeps listing and counting on the same authorization and filters.
func (s *Store) manageSelection(ctx context.Context, actor access.Actor, q ManageQuery) (string, []any, error) {
	rights, err := s.staffRights(ctx, actor)
	if err != nil {
		return "", nil, err
	}
	group := false
	if q.OwnerGroup != "" && !rights.write {
		editors, err := s.groupEditor(ctx, s.pool, actor, []string{q.OwnerGroup})
		if err != nil {
			return "", nil, err
		}
		if !editors[q.OwnerGroup] {
			return "", nil, ErrForbidden
		}
		group = true
	}
	var code string
	if q.Code != "" {
		c, err := contenturl.ParseCode(q.Code)
		if err != nil {
			return "", nil, invalid("code %q is not a content code", q.Code)
		}
		code = string(c)
	}
	if len(q.IDs) > MaxManageIDs {
		return "", nil, invalid("ids lists at most %d content ids", MaxManageIDs)
	}
	for _, id := range q.IDs {
		if err := contentref.ValidateID(id); err != nil {
			return "", nil, invalid("ids: %q is not a content id", id)
		}
	}
	var state string
	switch q.State {
	case "":
		state = "c.state <> 'deleted'"
		if code != "" || len(q.IDs) > 0 {
			state = "true"
		}
	case StateDraft, StateDeleted:
		state = "c.state = '" + q.State + "'"
	case "scheduled":
		state = "c.state = 'published' AND c.published_at IS NULL"
	case StatePublished:
		state = "c.state = 'published' AND c.published_at IS NOT NULL"
	default:
		return "", nil, invalid("state %q must be draft, scheduled, published or deleted", q.State)
	}
	cond, err := s.accessCond(q.Access, q.Tier)
	if err != nil {
		return "", nil, err
	}
	state += cond
	return `SELECT c.id FROM ` + s.t.items + ` c WHERE c.tenant_id = $1 AND ` + state + `
 AND ($2 = '' OR c.kind = $2) AND ($3 = '' OR c.owner_group_id = $3)
 AND ($4 OR $5 OR EXISTS (SELECT 1 FROM ` + s.t.creators + ` cc WHERE cc.tenant_id = c.tenant_id AND cc.content_id = c.id AND cc.user_id = $6))
 AND ($7 = '' OR EXISTS (SELECT 1 FROM ` + s.t.text + ` t WHERE t.tenant_id = c.tenant_id AND t.content_id = c.id
  AND (strpos(lower(t.title), lower($7)) > 0 OR strpos(lower(t.description), lower($7)) > 0 OR strpos(lower(t.body), lower($7)) > 0)))
 AND ($10 = '' OR EXISTS (SELECT 1 FROM ` + s.qs + `.content_codes k WHERE k.tenant_id = c.tenant_id AND k.content_kind = c.kind AND k.content_id = c.id AND k.code = $10))
 AND (cardinality($11::text[]) = 0 OR c.id = ANY($11::text[]))`,
		[]any{s.tenant, q.Kind, q.OwnerGroup, rights.write, group, viewerID(actor), strings.TrimSpace(q.Text), q.Limit, q.Offset, code, orEmpty(q.IDs)}, nil
}

// ManageCount is how many items a manage listing selects.
type ManageCount struct {
	Total int `json:"total"`
	// Truncated makes Total a lower bound.
	Truncated bool `json:"truncated,omitempty"`
}

// manageCountBound caps a count: no numbered list needs an exact total past it.
const manageCountBound = 10000

func (s *Store) handleManageCount(w http.ResponseWriter, r *http.Request) {
	actor, err := writer(r.Context())
	kind := r.PathValue("kind")
	if _, ok := s.cfg.Kinds[kind]; err == nil && !ok {
		err = ErrNotFound
	}
	var out ManageCount
	if err == nil {
		q := r.URL.Query()
		out, err = s.CountManaged(r.Context(), actor, ManageQuery{Kind: kind, State: q.Get("state"), OwnerGroup: q.Get("owner_group"),
			Text: q.Get("q"), Access: q.Get("access"), Tier: q.Get("tier")})
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// CountManaged counts what Manage selects for q, exactly up to manageCountBound.
// It counts a bounded selection without sorting or transferring its ids.
func (s *Store) CountManaged(ctx context.Context, actor access.Actor, q ManageQuery) (ManageCount, error) {
	q.Limit, q.Offset = manageCountBound+1, 0
	query, args, err := s.manageSelection(ctx, actor, q)
	if err != nil {
		return ManageCount{}, err
	}
	var total int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM ("+query+" LIMIT $8 OFFSET $9) counted", args...).Scan(&total); err != nil {
		return ManageCount{}, err
	}
	if total > manageCountBound {
		return ManageCount{Total: manageCountBound, Truncated: true}, nil
	}
	return ManageCount{Total: total}, nil
}

// accessCond is the SQL condition for a manage access filter. Levels and
// tiers are checked against the model before they reach the statement.
func (s *Store) accessCond(level, tier string) (string, error) {
	if tier != "" {
		if !slices.Contains(s.cfg.Tiers, tier) {
			return "", invalid("tier %q is not configured", tier)
		}
		if level == "" {
			level = string(access.TierLevel)
		}
		if level != string(access.TierLevel) {
			return "", invalid("tier filters need access tier")
		}
	}
	switch access.Level(level) {
	case "":
		return "", nil
	case access.Public, access.TierLevel, access.GroupLevel, access.PPV, access.GroupPPV:
	default:
		return "", invalid("access %q must be public, tier, group, ppv or group_ppv", level)
	}
	cond := " AND c.access->>'level' = '" + level + "'"
	if tier != "" {
		b, _ := json.Marshal([]string{tier})
		cond += " AND c.access->'tiers' @> '" + strings.ReplaceAll(string(b), "'", "''") + "'::jsonb"
	}
	return cond, nil
}
