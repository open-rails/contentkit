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
	"github.com/open-rails/contentkit/internal/hub"
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
	httpapi.Mount(mux, g.Guard, handler{s}, mineRoutes)
	httpapi.Mount(mux, g.Guard, handler{s}, searchRoutes)
	httpapi.Mount(mux, g.Guard, handler{s}, sitemapRoutes)
	httpapi.Mount(mux, g.Guard, handler{s}, statsRoutes)
	return mux, nil
}

func init() {
	httpapi.Register(httpapi.Discovery, routes)
	httpapi.Register(httpapi.Discovery, mineRoutes)
	httpapi.Register(httpapi.Discovery, searchRoutes)
	httpapi.Register(httpapi.Discovery, sitemapRoutes)
	httpapi.Register(httpapi.Discovery, statsRoutes)
}

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
		httpapi.List("creators", "user ids: only content one of them is credited on"),
		httpapi.List("access", "public (what anyone opens now), tier, group, ppv or group_ppv (what stays locked at that level): only content at one of them"),
		httpapi.List("tiers", "configured tiers: only content locked behind one of them"),
		httpapi.List("traits", "version traits every listed version has"),
		httpapi.Repeated("attribute", "name:value of one of the kind's filterable attributes; repeat for more"),
		httpapi.Text("released_on", "published on this day (YYYY-MM-DD) or month (YYYY-MM)"),
		httpapi.Text("published_within", "only currently live publications in 7d, 30d, 90d, 365d or all (whole UTC calendar days); independent of the sort's score window"),
		httpapi.Text("tz", "the IANA time zone of released_on and release dates; UTC by default"),
		httpapi.Text("view_status", "the signed-in reader's unseen, in_progress or completed contents"),
	}
	cardGroups     = httpapi.List("card_entities", "the group kinds cards name; every kind with a page by default")
	pageParams     = []httpapi.Param{httpapi.Text("cursor", "the previous page's next_cursor"), httpapi.Int("limit", "page size: 24 by default, at most 100")}
	sortParam      = httpapi.Text("sort", "newest, oldest, title, popular[:period], top_rated[:period], most_favorited, most_viewed (all time), position or random (periods: 7d, 30d, 90d, 365d, all); the kind's default, else newest")
	activityParams = []httpapi.Param{
		httpapi.Text("activity_from", "custom popularity activity window: inclusive UTC date YYYY-MM-DD; requires activity_until, kind and sort=popular; not a publication-date filter"),
		httpapi.Text("activity_until", "custom popularity activity window: exclusive UTC date YYYY-MM-DD; requires activity_from, kind and sort=popular"),
	}
	signals = []string{"not_configured"}
)

func params(groups ...[]httpapi.Param) []httpapi.Param {
	var out []httpapi.Param
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func dailyFilterParams() []httpapi.Param {
	out := make([]httpapi.Param, 0, len(filterParams))
	for _, p := range filterParams {
		if p.Name != "released_on" && p.Name != "tz" {
			out = append(out, p)
		}
	}
	return out
}

var routes = []httpapi.Route[handler]{
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/activity-contexts", Resource: "discovery", Auth: httpapi.Public,
		Doc:     "Captures signed, immutable event-time access and eligible audience context before displaying or consuming content. Batches up to 100 targets; clients reuse each receipt for all revisions of one view.",
		Request: ActivityContextsInput{}, Responses: []httpapi.Reply{httpapi.OK(ActivityContexts{})}, Errors: signals}, Serve: httpapi.H(handler.activityContexts)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/releases", Resource: "discovery", Auth: httpapi.Public,
		Doc:       "Every visible release in a calendar-day window ending on end_date (today by default), grouped newest first, with complete card metadata and empty days. Each day also counts unique contents omitted solely by optional reader entity filters and overlapping per-entity counts, including zero counts for current hidden public toggles. Hard domain, publication, language and access policies remain enforced at the current time; omitted content metadata and covers are not returned. No item limit or pagination; future, unlisted and deleted releases stay excluded.",
		Query:     params(dailyFilterParams(), readerParams, []httpapi.Param{cardGroups, httpapi.Int("days", "calendar days including end_date: 10 by default, from 1 through 31"), httpapi.Text("end_date", "inclusive YYYY-MM-DD in time_zone; today by default; future dates are rejected"), httpapi.Text("time_zone", "IANA zone for calendar-day boundaries; UTC by default")}),
		Responses: []httpapi.Reply{httpapi.OK(DailyReleases{})}}, Serve: httpapi.H(handler.releases)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/contents", Resource: "discovery", Auth: httpapi.Public,
		Doc:   "Lists contents as cards, one per content item with the version that fits the reader, under their visibility, site and content filters.",
		Query: params(filterParams, readerParams, []httpapi.Param{sortParam, cardGroups}, activityParams, pageParams), Responses: []httpapi.Reply{httpapi.OK(ContentPage{})},
		Errors: signals}, Serve: httpapi.H(handler.list)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/search", Resource: "discovery", Auth: httpapi.Public,
		Doc: "Keyword search over contents, grouped per content item, personalized for a signed-in reader unless sorted otherwise.",
		Query: params([]httpapi.Param{httpapi.Text("q", "the query")}, filterParams, readerParams,
			[]httpapi.Param{httpapi.Text("sort", "relevance (the default) or a list sort of the best matches"), cardGroups,
				httpapi.Bool("personalize", "false ranks a signed-in reader's results as anyone's"), sessionParam}, pageParams),
		Responses: []httpapi.Reply{httpapi.OK(ContentPage{})}, Errors: signals}, Serve: httpapi.H(handler.search)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/typeahead", Resource: "discovery", Auth: httpapi.Public,
		Doc: "Completes direct title and entity-name/alias prefixes while the reader types; excludes contextual keywords, interior words and typos.",
		Query: params([]httpapi.Param{httpapi.Text("q", "the partial query"), httpapi.Text("kind", "only contents of this kind"),
			httpapi.List("entity_kinds", "the group kinds suggested; every kind with a page by default")}, readerParams,
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
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/content-filters", Resource: "discovery", Auth: httpapi.Public,
		Doc:   "The current reader's configured content filters and settings. Anonymous requests may supply ContentKit-Reader-Preferences JSON with filters [{entity_id,visible}]; one choice applies across applies_to kinds. Account settings take precedence when signed in.",
		Query: []httpapi.Param{httpapi.Text("language", "the language of labels and group names")}, Responses: []httpapi.Reply{httpapi.OK(ContentFilters{})}},
		Serve: httpapi.H(handler.contentFilters)},
	{Spec: httpapi.Spec{Method: httpapi.PATCH, Path: "/me/content-filters", Resource: "discovery", Auth: httpapi.User,
		Doc:   "Atomically changes only submitted shared entity choices and optional include_deleted. Omitted choices remain unchanged. Each choice sets visible or default:true.",
		Query: []httpapi.Param{httpapi.Text("language", "the language of labels and group names")}, Request: ContentFiltersUpdate{},
		Responses: []httpapi.Reply{httpapi.OK(ContentFilters{})}}, Serve: httpapi.H(handler.setContentFilters)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/me/content-filters/initialize", Resource: "discovery", Auth: httpapi.User,
		Doc:   "Initializes the account_id's content filter settings from its guest choices only if no settings exist; account_id must match the authenticated account and retries preserve all saved settings.",
		Query: []httpapi.Param{httpapi.Text("language", "the language of labels and group names")}, Request: ContentFiltersInitialization{},
		Responses: []httpapi.Reply{httpapi.OK(ContentFilters{})}, Errors: []string{httpapi.CodeForbidden}}, Serve: httpapi.H(handler.initializeContentFilters)},
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
		Attributes: q["attribute"], ReleasedOn: q.Get("released_on"), PublishedWithin: q.Get("published_within"), TimeZone: q.Get("tz"), ViewStatus: q.Get("view_status"),
		OwnerGroups: list(q.Get("owner_group")), Creators: list(q.Get("creators")), Access: list(q.Get("access")), Tiers: list(q.Get("tiers"))}
}

func (h handler) releases(w http.ResponseWriter, r *http.Request) {
	days, err := intParam(r, "days")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	out, err := h.s.Releases(r.Context(), DailyReleasesOptions{Reader: readerOf(r), Filter: filterOf(r), Days: days, EndDate: q.Get("end_date"), TimeZone: q.Get("time_zone"), CardGroups: list(q.Get("card_entities"))})
	h.reply(w, r, out, err)
}

func (h handler) list(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	for _, name := range []string{"activity_from", "activity_until"} {
		if len(q[name]) > 1 || len(q[name]) == 1 && q.Get(name) == "" {
			h.fail(w, r, fmt.Errorf("%w: %s must occur once with a UTC date", ErrInvalid, name))
			return
		}
	}
	page, err := h.s.List(r.Context(), ListOptions{Reader: readerOf(r), Filter: filterOf(r), Sort: q.Get("sort"), CardGroups: list(q.Get("card_entities")),
		ActivityFrom: q.Get("activity_from"), ActivityUntil: q.Get("activity_until"),
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
	o := SearchOptions{Reader: readerOf(r), Filter: filterOf(r), Query: q.Get("q"), Sort: q.Get("sort"), CardGroups: list(q.Get("card_entities")),
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
	if err == nil {
		h.recordSearch(r, o, page)
	}
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
		GroupKinds: list(q.Get("entity_kinds")), Limit: limit})
	h.reply(w, r, out, err)
}

func (h handler) recOptions(r *http.Request) (RecommendationOptions, error) {
	limit, err := intParam(r, "limit")
	return RecommendationOptions{Reader: readerOf(r), Filter: filterOf(r), CardGroups: list(r.URL.Query().Get("card_entities")), Limit: limit}, err
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

func (h handler) initializeContentFilters(w http.ResponseWriter, r *http.Request) {
	var u ContentFiltersInitialization
	if err := decode(r, &u); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.s.InitializeContentFilters(r.Context(), gate.Actor(r.Context()), u); err != nil {
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
		CardGroups: list(q.Get("card_entities")), Cursor: q.Get("cursor"), Limit: limit})
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

func (h handler) activityContexts(w http.ResponseWriter, r *http.Request) {
	var in ActivityContextsInput
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.s.ActivityContexts(r.Context(), gate.Actor(r.Context()), in)
	h.reply(w, r, out, err)
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
		return fmt.Errorf("%w: %s", ErrInvalid, httpapi.BodyError(err))
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("%w: the body holds more than one JSON value", ErrInvalid)
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
	code, msg, failed := httpapi.CodeInternal, "", true
	switch {
	case errors.Is(err, auth.ErrForbidden):
		code, failed = httpapi.CodeForbidden, false
	case errors.Is(err, ErrInvalid):
		code, msg, failed = httpapi.CodeInvalidRequest, httpapi.Rule(err, httpapi.CodeInvalidRequest), false
	case errors.Is(err, ErrNotFound):
		code, failed = httpapi.CodeNotFound, false
	case errors.Is(err, ErrSignalsDown), errors.Is(err, hub.ErrSignalPlaneDown):
		// The engine warns once while the plane is down, not per refusal.
		code, msg, failed = httpapi.CodeUnavailable, "Signals are unavailable right now; try again later.", false
	case errors.Is(err, ErrNoSignals), errors.Is(err, hub.ErrSignalPlaneDisabled):
		code, msg, failed = "not_configured", "This site records no signals.", false
	case errors.Is(err, access.ErrUnavailable), errors.Is(err, auth.ErrUnavailable):
		code = httpapi.CodeUnavailable
	}
	if msg == "" {
		msg = httpapi.Message(code)
	}
	if failed {
		h.s.o.Logger.ErrorContext(r.Context(), "discovery: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	} else {
		h.s.o.Logger.DebugContext(r.Context(), "discovery: request refused", "method", r.Method, "path", r.URL.Path, "code", code, "err", err)
	}
	c, _ := httpapi.LookupErrorCode(code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(c.Status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}
