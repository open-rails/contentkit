package versions

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/media/layout"
)

func init() { httpapi.Register(httpapi.Versions, routes) }

// ForkBody forks a version from a sibling (Fork).
type ForkBody struct {
	From     string `json:"from"`
	Playback bool   `json:"playback,omitempty"`
	Segments bool   `json:"segments,omitempty"`
}

// Pending answers a version whose media is still being prepared: retry.
type Pending struct {
	Status string `json:"status"` // "pending"
}

type errorReply struct {
	Error      string `json:"error"`
	Code       string `json:"code"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

var routes = []httpapi.Route[handler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/contents/{id}/versions/{version}/reader", Resource: "reader", Auth: httpapi.Public,
		Doc:       "A version's reader plan: its segments with their release state, the published item of each the viewer opens, the previews of a locked version, its playback item and reader metadata. 202 while it has no current publication.",
		Responses: []httpapi.Reply{httpapi.OK(ReaderPlan{}), httpapi.Accepted(Pending{})}},
		Serve: httpapi.H(handler.plan)},
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/contents/{id}/versions/{version}/reader", Resource: "reader", Auth: httpapi.User,
		Doc:       "Sets a version's reader metadata (editors): anchors and audio cues on its pages and audio uploads.",
		Request:   ReaderMeta{},
		Responses: []httpapi.Reply{httpapi.NoContent}},
		Serve: httpapi.H(handler.setMeta)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/contents/{id}/versions/{version}/fork", Resource: "reader", Auth: httpapi.User,
		Doc:       "Fills a version without media from a sibling (editors): its uploads with their outputs, reader metadata and optionally its audio and segments, in a durable job.",
		Request:   ForkBody{},
		Responses: []httpapi.Reply{httpapi.Accepted(Pending{})}},
		Serve: httpapi.H(handler.fork)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/contents/{id}/versions/{version}/download", Resource: "reader", Auth: httpapi.Public,
		Doc:       "Grants a version's download, what its kind's download serves (the original, a rendition, its pages as a zip), charged to the viewer's download quota: a FileDownload whose short-lived URL serves it as an attachment. The URL is the media host's, or, for a zip the kind's watermark marks for this viewer, the archive route's (relative to this one). Every segment released and the version open. 202 while it is prepared; 409 no_download when the version has nothing to serve.",
		Responses: []httpapi.Reply{httpapi.OK(FileDownload{}), httpapi.Accepted(Pending{})},
		Errors:    []string{httpapi.CodeForbidden, httpapi.CodeRateLimited, CodeNoDownload}},
		Serve: httpapi.H(handler.download)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/contents/{id}/versions/{version}/archive", Resource: "reader", Auth: httpapi.Public,
		Doc:       "Streams the marked zip a download granted: the version's pages, those its kind's watermark names marked for the viewer the ticket is for. 404 for an expired or foreign ticket.",
		Query:     []httpapi.Param{httpapi.Text("for", "the viewer the ticket is for"), httpapi.Text("ticket", "the download's ticket")},
		Responses: []httpapi.Reply{httpapi.OK(httpapi.Stream{ContentType: "application/zip"})}},
		Serve: httpapi.H(handler.archive)},
}

// CodeNoDownload is a version without the file its kind's download serves.
const CodeNoDownload = "no_download"

// Handler serves the versions module's routes, each behind its tier's gate.
func (s *Service) Handler(a auth.Authenticator, log *slog.Logger) (http.Handler, error) {
	if err := gate.Check(a); err != nil {
		return nil, err
	}
	if log == nil {
		log = s.o.Logger
	}
	mux := http.NewServeMux()
	httpapi.Mount(mux, (&gate.Gate{Auth: a, Log: log}).Guard, handler{s, log}, routes)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(access.WithMemo(r.Context())))
	}), nil
}

type handler struct {
	s   *Service
	log *slog.Logger
}

func target(r *http.Request) Target {
	return Target{Content: r.PathValue("id"), Version: r.PathValue("version")}
}

func (h handler) plan(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	plan, err := h.s.Plan(r.Context(), gate.Actor(r.Context()), target(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (h handler) setMeta(w http.ResponseWriter, r *http.Request) {
	var rm ReaderMeta
	if err := decode(r, &rm); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.s.SetReaderMeta(r.Context(), gate.Actor(r.Context()), target(r), rm); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h handler) fork(w http.ResponseWriter, r *http.Request) {
	var b ForkBody
	if err := decode(r, &b); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.s.RequestFork(r.Context(), gate.Actor(r.Context()), Fork{Version: target(r), From: b.From, Playback: b.Playback,
		Segments: b.Segments}); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, Pending{Status: "pending"})
}

func (h handler) download(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	d, err := h.s.Download(r.Context(), gate.Actor(r.Context()), target(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h handler) archive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	q := r.URL.Query()
	a, err := h.s.Archive(r.Context(), target(r), q.Get("for"), q.Get("ticket"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", layout.Attachment(a.Name))
	w.WriteHeader(http.StatusOK)
	if _, err := a.WriteTo(w); err != nil && r.Context().Err() == nil {
		h.log.ErrorContext(r.Context(), "versions: archive stream failed", "version", target(r).Version, "error", err)
	}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, httpapi.BodyError(err))
	}
	return nil
}

func (h handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set("Cache-Control", "no-store")
	var quota *QuotaError
	switch {
	case errors.Is(err, ErrNotReady):
		writeJSON(w, http.StatusAccepted, Pending{Status: "pending"})
	case errors.As(err, &quota):
		retry := max(1, int(math.Ceil(quota.RetryAfter.Seconds())))
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		writeJSON(w, http.StatusTooManyRequests, errorReply{Error: "download quota reached", Code: httpapi.CodeRateLimited, RetryAfter: retry})
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, errorReply{Error: "not found", Code: httpapi.CodeNotFound})
	case errors.Is(err, ErrNoDownload):
		writeJSON(w, http.StatusConflict, errorReply{Error: httpapi.Message(CodeNoDownload), Code: CodeNoDownload})
	case errors.Is(err, ErrForbidden):
		writeJSON(w, http.StatusForbidden, errorReply{Error: "not allowed", Code: httpapi.CodeForbidden})
	case errors.Is(err, ErrInvalid):
		writeJSON(w, http.StatusBadRequest, errorReply{Error: httpapi.Rule(err, httpapi.CodeInvalidRequest), Code: httpapi.CodeInvalidRequest})
	default:
		h.log.ErrorContext(r.Context(), "versions: request failed", "path", r.URL.Path, "error", err)
		writeJSON(w, http.StatusInternalServerError, errorReply{Error: "internal error", Code: httpapi.CodeInternal})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
