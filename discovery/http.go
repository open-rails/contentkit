package discovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/httpapi"
)

// Handler serves the discovery routes: lists, search, typeahead, similar and
// "for you" lists, the release date index, the reader's content filters and
// history, and the signals and exposures clients send. a says who each
// request is; each request runs in access.WithMemo, so a page costs one
// billing read.
func (s *Service) Handler(a auth.Authenticator) (http.Handler, error) {
	if err := gate.Check(a); err != nil {
		return nil, fmt.Errorf("discovery: Handler: %w", err)
	}
	mux := http.NewServeMux()
	g := &gate.Gate{Auth: a, Log: s.o.Logger, Then: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(access.WithMemo(r.Context())))
		})
	}}
	httpapi.Mount(mux, g.Guard, handler{s}, routes)
	return mux, nil
}

func init() { httpapi.Register(httpapi.Discovery, routes) }

type handler struct{ s *Service }

var (
	readerParams = []httpapi.Param{
		httpapi.Text("language", "only content with a live version in this language"),
		httpapi.Text("display_language", "the language of titles, group names and paths, and the preferred language of each card's version; language by default"),
		httpapi.Text("mode", "lock (the default: everything listed, locked cards marked) or hide (only what the reader opens)"),
	}
	filterParams = []httpapi.Param{
		httpapi.Text("kind", "only this content kind"),
		httpapi.List("groups", "group ids every listed version is in; a position sort orders by the first"),
		httpapi.List("exclude_groups", "group ids no listed version is in"),
		httpapi.List("owner_group", "owner group ids (channels): only content one of them owns"),
		httpapi.List("traits", "version traits every listed version has"),
		httpapi.Repeated("attribute", "name:value of one of the kind's filterable attributes; repeat for more"),
		httpapi.Text("released_on", "published on this day (YYYY-MM-DD) or month (YYYY-MM)"),
		httpapi.Text("tz", "the IANA time zone of released_on and release dates; UTC by default"),
		httpapi.Text("view_status", "the signed-in reader's unseen, in_progress or completed contents"),
	}
	cardGroups = httpapi.List("card_groups", "the group kinds cards name; every kind with a page by default")
	pageParams = []httpapi.Param{httpapi.Text("cursor", "the previous page's next_cursor"), httpapi.Int("limit", "page size: 24 by default, at most 100")}
	sortParam  = httpapi.Text("sort", "newest, oldest, title, popular[:period], top_rated[:period], most_favorited, position or random (periods: 7d, 30d, 90d, 365d, all); the kind's default, else newest")
	signals    = []string{"not_configured"}
)

func params(groups ...[]httpapi.Param) []httpapi.Param {
	var out []httpapi.Param
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

var routes = []httpapi.Route[handler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/contents", Resource: "discovery", Auth: httpapi.Public,
		Doc:   "Lists contents as cards, one per content item with the version that fits the reader, under their visibility, site and content filters.",
		Query: params(filterParams, readerParams, []httpapi.Param{sortParam, cardGroups}, pageParams), Responses: []httpapi.Reply{httpapi.OK(ContentPage{})},
		Errors: signals}, Serve: httpapi.H(handler.list)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/search", Resource: "discovery", Auth: httpapi.Public,
		Doc: "Keyword search over contents, grouped per content item, personalized for a signed-in reader unless sorted otherwise.",
		Query: params([]httpapi.Param{httpapi.Text("q", "the query")}, filterParams, readerParams,
			[]httpapi.Param{httpapi.Text("sort", "relevance (the default) or a list sort of the best matches"), cardGroups,
				httpapi.Bool("personalize", "false ranks a signed-in reader's results as anyone's")}, pageParams),
		Responses: []httpapi.Reply{httpapi.OK(ContentPage{})}, Errors: signals}, Serve: httpapi.H(handler.search)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/typeahead", Resource: "discovery", Auth: httpapi.Public,
		Doc: "Suggests contents and groups while the reader types, tolerating typos and partial words.",
		Query: params([]httpapi.Param{httpapi.Text("q", "the partial query"), httpapi.Text("kind", "only contents of this kind"),
			httpapi.List("group_kinds", "the group kinds suggested; every kind with a page by default")}, readerParams,
			[]httpapi.Param{httpapi.Int("limit", "per section: 8 by default, at most 20")}),
		Responses: []httpapi.Reply{httpapi.OK(Typeahead{})}}, Serve: httpapi.H(handler.typeahead)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/contents/{id}/similar", Resource: "discovery", Auth: httpapi.Public,
		Doc:       "Contents like this one (co-engagement, or the configured source), of its kind unless kind names another.",
		Query:     params(filterParams, readerParams, []httpapi.Param{cardGroups, httpapi.Int("limit", "12 by default, at most 50")}),
		Responses: []httpapi.Reply{httpapi.OK(ContentPage{})}, Errors: append([]string{httpapi.CodeNotFound}, signals...)}, Serve: httpapi.H(handler.similar)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/recommendations", Resource: "discovery", Auth: httpapi.User,
		Doc:       "Contents for the signed-in reader from their history, filled from what is popular; seen and disliked content left out.",
		Query:     params(filterParams, readerParams, []httpapi.Param{cardGroups, httpapi.Int("limit", "12 by default, at most 50")}),
		Responses: []httpapi.Reply{httpapi.OK(ContentPage{})}, Errors: signals}, Serve: httpapi.H(handler.recommendations)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/release-dates", Resource: "discovery", Auth: httpapi.Public,
		Doc: "The release date index: per day or month, newest first, the contents the reader's listing shows released then and how many are free.",
		Query: params([]httpapi.Param{httpapi.Text("by", "month (the default) or day")}, filterParams, readerParams,
			[]httpapi.Param{httpapi.Text("cursor", "the previous page's next_cursor"), httpapi.Int("limit", "60 by default, at most 1000")}),
		Responses: []httpapi.Reply{httpapi.OK(ReleaseDates{})}, Errors: signals}, Serve: httpapi.H(handler.releaseDates)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/me/content-filters", Resource: "discovery", Auth: httpapi.User,
		Doc:   "The reader's content filters: each configured filter's group, default and setting; every read applies them.",
		Query: []httpapi.Param{httpapi.Text("language", "the language of labels and group names")}, Responses: []httpapi.Reply{httpapi.OK(ContentFilters{})}},
		Serve: httpapi.H(handler.contentFilters)},
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/me/content-filters", Resource: "discovery", Auth: httpapi.User,
		Doc:   "Replaces the reader's content filter settings and whether lists include deleted content they hold.",
		Query: []httpapi.Param{httpapi.Text("language", "the language of labels and group names")}, Request: ContentFiltersUpdate{},
		Responses: []httpapi.Reply{httpapi.OK(ContentFilters{})}}, Serve: httpapi.H(handler.setContentFilters)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/me/history", Resource: "discovery", Auth: httpapi.User,
		Doc: "The contents the reader engaged with, most recent first, with their progress.",
		Query: params([]httpapi.Param{httpapi.Text("kind", "only this content kind"), httpapi.Text("status", "seen, in_progress or completed; any by default"),
			cardGroups}, readerParams, []httpapi.Param{httpapi.Text("cursor", "the previous page's next_cursor"), httpapi.Int("limit", "page size: 24 by default, at most 100")}),
		Responses: []httpapi.Reply{httpapi.OK(ContentPage{})}, Errors: signals}, Serve: httpapi.H(handler.history)},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/me/history", Resource: "discovery", Auth: httpapi.User,
		Doc:       "Erases the reader's history: of one content item, one kind or everything.",
		Query:     []httpapi.Param{httpapi.Text("kind", "only this content kind"), httpapi.Text("content_id", "only this content item")},
		Responses: []httpapi.Reply{httpapi.NoContent}, Errors: append([]string{httpapi.CodeNotFound}, signals...)}, Serve: httpapi.H(handler.forget)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/signals", Resource: "discovery", Auth: httpapi.Public,
		Doc:     "Records views and clicks of the signed-in reader or, signed out, of the session named; a view's progress is scored against its version.",
		Request: Signals{}, Responses: []httpapi.Reply{httpapi.NoContent}, Errors: append([]string{httpapi.CodeNotFound}, signals...)},
		Serve: httpapi.H(handler.signals)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/exposures", Resource: "discovery", Auth: httpapi.Public,
		Doc:     "Records result lists a client served, rendered or showed, for click attribution.",
		Request: Exposures{}, Responses: []httpapi.Reply{httpapi.NoContent}, Errors: append([]string{httpapi.CodeNotFound}, signals...)},
		Serve: httpapi.H(handler.exposures)},
}

func list(v string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

func intParam(r *http.Request, name string) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%w: %s", ErrInvalid, name)
	}
	return n, nil
}

func readerOf(r *http.Request) Reader {
	q := r.URL.Query()
	return Reader{Actor: gate.Actor(r.Context()), Host: r.Host, Language: q.Get("language"), DisplayLanguage: q.Get("display_language"),
		Mode: access.Mode(q.Get("mode"))}
}

func filterOf(r *http.Request) Filter {
	q := r.URL.Query()
	return Filter{Kind: q.Get("kind"), Groups: list(q.Get("groups")), ExcludeGroups: list(q.Get("exclude_groups")), Traits: list(q.Get("traits")),
		Attributes: q["attribute"], ReleasedOn: q.Get("released_on"), TimeZone: q.Get("tz"), ViewStatus: q.Get("view_status"),
		OwnerGroups: list(q.Get("owner_group"))}
}

func (h handler) list(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	page, err := h.s.List(r.Context(), ListOptions{Reader: readerOf(r), Filter: filterOf(r), Sort: q.Get("sort"), CardGroups: list(q.Get("card_groups")),
		Cursor: q.Get("cursor"), Limit: limit})
	h.reply(w, r, page, err)
}

func (h handler) search(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	o := SearchOptions{Reader: readerOf(r), Filter: filterOf(r), Query: q.Get("q"), Sort: q.Get("sort"), CardGroups: list(q.Get("card_groups")),
		Cursor: q.Get("cursor"), Limit: limit}
	if v := q.Get("personalize"); v != "" {
		p, err := strconv.ParseBool(v)
		if err != nil {
			h.fail(w, r, fmt.Errorf("%w: personalize", ErrInvalid))
			return
		}
		o.Unpersonalized = !p
	}
	page, err := h.s.Search(r.Context(), o)
	h.reply(w, r, page, err)
}

func (h handler) typeahead(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	out, err := h.s.Typeahead(r.Context(), TypeaheadOptions{Reader: readerOf(r), Query: q.Get("q"), Kind: q.Get("kind"),
		GroupKinds: list(q.Get("group_kinds")), Limit: limit})
	h.reply(w, r, out, err)
}

func (h handler) recOptions(r *http.Request) (RecommendationOptions, error) {
	limit, err := intParam(r, "limit")
	return RecommendationOptions{Reader: readerOf(r), Filter: filterOf(r), CardGroups: list(r.URL.Query().Get("card_groups")), Limit: limit}, err
}

func (h handler) similar(w http.ResponseWriter, r *http.Request) {
	o, err := h.recOptions(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	page, err := h.s.Similar(r.Context(), r.PathValue("id"), o)
	h.reply(w, r, page, err)
}

func (h handler) recommendations(w http.ResponseWriter, r *http.Request) {
	o, err := h.recOptions(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	page, err := h.s.Recommendations(r.Context(), o)
	h.reply(w, r, page, err)
}

func (h handler) releaseDates(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	out, err := h.s.ReleaseDates(r.Context(), ReleaseDatesOptions{Reader: readerOf(r), Filter: filterOf(r), By: q.Get("by"), Cursor: q.Get("cursor"), Limit: limit})
	h.reply(w, r, out, err)
}

func (h handler) contentFilters(w http.ResponseWriter, r *http.Request) {
	out, err := h.s.ContentFilters(r.Context(), gate.Actor(r.Context()), r.URL.Query().Get("language"))
	h.reply(w, r, out, err)
}

func (h handler) setContentFilters(w http.ResponseWriter, r *http.Request) {
	var u ContentFiltersUpdate
	if err := decode(r, &u); err != nil {
		h.fail(w, r, err)
		return
	}
	actor := gate.Actor(r.Context())
	if err := h.s.SetContentFilters(r.Context(), actor, u); err != nil {
		h.fail(w, r, err)
		return
	}
	h.contentFilters(w, r)
}

func (h handler) history(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	page, err := h.s.History(r.Context(), HistoryOptions{Reader: readerOf(r), Kind: q.Get("kind"), Status: q.Get("status"),
		CardGroups: list(q.Get("card_groups")), Cursor: q.Get("cursor"), Limit: limit})
	h.reply(w, r, page, err)
}

func (h handler) forget(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	h.done(w, r, h.s.ForgetHistory(r.Context(), gate.Actor(r.Context()), q.Get("kind"), q.Get("content_id")))
}

func (h handler) signals(w http.ResponseWriter, r *http.Request) {
	var in Signals
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	h.done(w, r, h.s.RecordSignals(r.Context(), gate.Actor(r.Context()), in))
}

func (h handler) exposures(w http.ResponseWriter, r *http.Request) {
	var in Exposures
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	h.done(w, r, h.s.RecordExposures(r.Context(), gate.Actor(r.Context()), in))
}

const maxBody = 1 << 20

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: body: %v", ErrInvalid, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("%w: body: one JSON value", ErrInvalid)
	}
	return nil
}

func (h handler) reply(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func (h handler) done(w http.ResponseWriter, r *http.Request, err error) {
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	code, msg := httpapi.CodeInternal, "internal error"
	switch {
	case errors.Is(err, ErrInvalid):
		code, msg = httpapi.CodeInvalidRequest, err.Error()
	case errors.Is(err, ErrNotFound):
		code, msg = httpapi.CodeNotFound, "not found"
	case errors.Is(err, ErrNoSignals):
		code, msg = "not_configured", "this read needs the signal plane, which this site runs without"
	case errors.Is(err, access.ErrUnavailable), errors.Is(err, auth.ErrUnavailable):
		code, msg = httpapi.CodeUnavailable, "billing or auth unavailable; retry"
	}
	if code == httpapi.CodeInternal || code == httpapi.CodeUnavailable {
		h.s.o.Logger.ErrorContext(r.Context(), "discovery: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	c, _ := httpapi.LookupErrorCode(code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(c.Status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}
