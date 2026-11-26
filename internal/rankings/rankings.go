// Package rankings computes popularity from ClickHouse's content_daily and
// caches the ranked lists in Redis (in process without it).
//
// A gallery's popularity over a window is the sum of q over its person-days
// on the site, the days it was open only to tiers scaled to the site's active
// people; trending (the week) divides it by the days the gallery was live,
// today counting as the part elapsed. An entity's is the sum of its
// galleries'.
//
// Featured windows (Featured) are precomputed after each hourly rebuild of
// content_daily, every site and list at once, and served from the cache:
// pages never query ClickHouse for them, and a failed precompute leaves the
// previous ranking served. A closed range (one whose last day has had its
// final rebuild: a past month, a past year) never changes for one
// outcome_values version, so it is computed once, on first request, and kept
// without expiry. Any other range is computed per request, uncached.
package rankings

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/signal"
)

// ErrInvalid is a request the rankings refuse.
var ErrInvalid = errors.New("rankings: invalid request")

// Site is where activity happened: a domain and a language.
type Site struct{ Domain, Language string }

// List names a ranked list: a content kind's, its audience's (readers in
// each content's audience only, for a niche's own list) or an entity kind's.
type List struct {
	Kind     string
	Audience bool
	Entities bool
}

func (l List) key() string {
	switch {
	case l.Entities:
		return "e:" + l.Kind
	case l.Audience:
		return "a:" + l.Kind
	}
	return "c:" + l.Kind
}

// Source sums content_daily: the engine's hub.
type Source interface {
	Popularity(ctx context.Context, q signal.PopularityQuery) ([]signal.ContentPopularity, error)
}

// Options configure an Engine.
type Options struct {
	Pool   *pgxpool.Pool
	Schema string
	Tenant string
	Kinds  kinds.Config
	// Source is ClickHouse.
	Source Source
	// Redis shares rankings and similar lists across replicas; nil keeps
	// them per process.
	Redis  redis.UniversalClient
	Logger *slog.Logger
	Now    func() time.Time
}

// Engine ranks one tenant's contents and entities.
type Engine struct {
	o     Options
	store store
	local localCache

	catalogMu sync.Mutex
	catalog   *catalog

	memo  memo
	sizes sync.Map

	flights flights
	reports chan struct{}
}

// reportSlots bounds concurrent uncached ranges per process: each is a
// ClickHouse aggregation.
const reportSlots = 2

// New validates o.
func New(o Options) (*Engine, error) {
	if o.Pool == nil || strings.TrimSpace(o.Schema) == "" || strings.TrimSpace(o.Tenant) == "" || o.Source == nil {
		return nil, errors.New("rankings: Pool, Schema, Tenant and Source are required")
	}
	if len(o.Kinds.Languages) == 0 {
		return nil, errors.New("rankings: Kinds needs a language")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	e := &Engine{o: o, reports: make(chan struct{}, reportSlots)}
	if o.Redis != nil {
		e.store = &redisStore{rdb: o.Redis, prefix: "contentkit:" + o.Tenant + ":"}
	} else {
		e.store = newMemoryStore(o.Now)
	}
	return e, nil
}

// Site is a request's site: the domain its host selects and language, else
// the first configured language.
func (e *Engine) Site(host, language string) Site {
	domain, _ := e.o.Kinds.DomainFor(host)
	if language = strings.TrimSpace(language); language == "" {
		language = e.o.Kinds.Languages[0]
	}
	return Site{Domain: domain.Name, Language: language}
}

// sites are every site precomputed: each domain in each language.
func (e *Engine) sites() []Site {
	domains := []string{""}
	if ds := e.o.Kinds.ContentFilters.Domains; len(ds) > 0 {
		domains = domains[:0]
		for _, d := range ds {
			domains = append(domains, d.Name)
		}
	}
	var out []Site
	for _, d := range domains {
		for _, l := range e.o.Kinds.Languages {
			out = append(out, Site{Domain: d, Language: l})
		}
	}
	return out
}

// Ranking is site's list over w. A cursor's version pins the ranking it
// paged while that ranking is cached. A featured window not yet precomputed
// is an empty ranking that is not Ready.
func (e *Engine) Ranking(ctx context.Context, site Site, w Window, list List, version string) (*Ranking, error) {
	if err := e.checkList(list); err != nil {
		return nil, err
	}
	if w.Featured {
		return e.featured(ctx, site, w, list, version)
	}
	final, err := e.final(ctx)
	if err != nil {
		return nil, err
	}
	if w.closedBy(final) {
		return e.closed(ctx, site, w, list)
	}
	return e.report(ctx, site, w, list)
}

func (e *Engine) checkList(l List) error {
	if l.Entities {
		if _, ok := e.o.Kinds.EntityKinds[l.Kind]; !ok {
			return fmt.Errorf("%w: entity kind %q is not configured", ErrInvalid, l.Kind)
		}
		return nil
	}
	if _, ok := e.o.Kinds.Kinds[l.Kind]; !ok {
		return fmt.Errorf("%w: kind %q is not configured", ErrInvalid, l.Kind)
	}
	return nil
}

// Keys: a featured list's pointer names its current version; a version's
// data is immutable; a closed range's data has no expiry.
func featuredKey(site Site, window string, l List) string {
	return "rank:" + site.Domain + ":" + site.Language + ":" + window + ":" + l.key()
}

func pointerKey(site Site, window string, l List) string {
	return "ptr:" + featuredKey(site, window, l)
}

func dataKey(site Site, window string, l List, version string) string {
	return featuredKey(site, window, l) + ":" + version
}

// supersededTTL keeps a replaced featured ranking for the cursors paging it.
const supersededTTL = 2 * time.Hour

// featuredTTL keeps the current featured ranking through a long outage of
// the hourly precompute; past it, lists rank without activity.
const featuredTTL = 8 * 24 * time.Hour

// staleAfter marks a featured ranking whose precompute has not run since.
const staleAfter = 3 * time.Hour

// featured serves the cached ranking; while the cache cannot be read, lists
// keep their order without activity.
func (e *Engine) featured(ctx context.Context, site Site, w Window, l List, version string) (*Ranking, error) {
	r, err := e.cachedFeatured(ctx, site, w, l, version)
	if err != nil {
		e.o.Logger.WarnContext(ctx, "rankings: cache unreadable; listing without activity", "window", w.Name, "list", l.key(), "err", err)
	}
	if r == nil {
		return &Ranking{From: w.From, Until: w.Until}, nil
	}
	return e.dated(r), nil
}

func (e *Engine) cachedFeatured(ctx context.Context, site Site, w Window, l List, version string) (*Ranking, error) {
	if version != "" {
		if r, err := e.load(ctx, dataKey(site, w.Name, l, version), version); err != nil || r != nil {
			return r, err
		}
	}
	current, err := e.memo.pointer(ctx, e.store, pointerKey(site, w.Name, l))
	if err != nil || current == "" {
		return nil, err
	}
	return e.load(ctx, dataKey(site, w.Name, l, current), current)
}

func (e *Engine) dated(r *Ranking) *Ranking {
	if r != nil && r.Computed.Add(staleAfter).Before(e.now()) {
		c := *r
		c.Stale = true
		return &c
	}
	return r
}

// load reads and decodes key, in process first; nil when absent.
func (e *Engine) load(ctx context.Context, key, version string) (*Ranking, error) {
	if r := e.local.get(key); r != nil {
		return r, nil
	}
	data, ok, err := e.store.get(ctx, key)
	if err != nil || !ok {
		return nil, err
	}
	r, err := decodeRanking(data, version)
	if err != nil {
		return nil, err
	}
	e.local.put(key, r)
	return r, nil
}

// closedKey names a closed range's list for one outcome_values version.
func closedKey(site Site, w Window, l List, values string) string {
	return "closed:" + site.Domain + ":" + site.Language + ":" + w.From.Format(time.DateOnly) + ":" + w.Until.Format(time.DateOnly) + ":" + l.key() + ":" + values
}

func (e *Engine) closed(ctx context.Context, site Site, w Window, l List) (*Ranking, error) {
	values, err := e.memo.values(ctx, e)
	if err != nil {
		return nil, err
	}
	key := closedKey(site, w, l, values.version)
	if r, err := e.load(ctx, key, values.version); err != nil || r != nil {
		return r, err
	}
	fill := "closed:" + site.Domain + ":" + site.Language + ":" + w.Name + ":" + values.version
	err = e.flights.do(ctx, fill, func() error { return e.fillClosed(ctx, site, w, values, fill) })
	if err != nil {
		return nil, err
	}
	r, err := e.load(ctx, key, values.version)
	if r == nil && err == nil {
		err = fmt.Errorf("rankings: closed range %s was not stored", w.Name)
	}
	return r, err
}

// fillClosed computes a closed range's lists once across replicas: another
// replica's fill is awaited before computing it here.
func (e *Engine) fillClosed(ctx context.Context, site Site, w Window, values outcomeValues, lock string) error {
	locked, err := e.store.lock(ctx, lock, fillLock)
	if err != nil {
		return err
	}
	if !locked {
		probe := closedKey(site, w, List{Kind: e.firstKind()}, values.version)
		for deadline := time.Now().Add(fillLock); time.Now().Before(deadline); {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
			if _, ok, err := e.store.get(ctx, probe); err != nil || ok {
				return err
			}
		}
	} else {
		defer e.store.unlock(context.WithoutCancel(ctx), lock)
	}
	lists, err := e.compute(ctx, site, []Window{w}, values, e.now())
	if err != nil {
		return err
	}
	for l, r := range lists[0] {
		if err := e.store.set(ctx, closedKey(site, w, l, values.version), encodeRanking(r), 0); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) firstKind() string {
	for _, k := range slices.Sorted(maps.Keys(e.o.Kinds.Kinds)) {
		return k
	}
	return ""
}

// report computes an open range for one request.
func (e *Engine) report(ctx context.Context, site Site, w Window, l List) (*Ranking, error) {
	values, err := e.memo.values(ctx, e)
	if err != nil {
		return nil, err
	}
	select {
	case e.reports <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-e.reports }()
	lists, err := e.compute(ctx, site, []Window{w}, values, e.now())
	if err != nil {
		return nil, err
	}
	return lists[0][l], nil
}

// Rebuild is one rebuild of content_daily the precompute follows.
type Rebuild struct {
	// Generation is the rebuild's content_daily generation.
	Generation time.Time
	// Final is the day whose last rebuild this was (yesterday, after the
	// post-midnight rebuild); zero for an hourly rebuild of today. Ranges
	// through it are closed from then on.
	Final time.Time
}

// Precompute fills every featured window's lists for every site after a
// rebuild: each site's windows in one ClickHouse query. A site whose query
// fails keeps serving its previous rankings. After a month's or a year's
// last day is final, its calendar range is filled too.
func (e *Engine) Precompute(ctx context.Context, r Rebuild) error {
	if !r.Final.IsZero() {
		if err := e.markFinal(ctx, day(r.Final)); err != nil {
			return err
		}
	}
	values, err := e.loadValues(ctx)
	if err != nil {
		return err
	}
	at := r.Generation.UTC()
	if at.IsZero() {
		at = e.now()
	}
	// Unique per run: a cursor pins the version it paged, and two runs of one
	// generation must not overwrite each other's lists.
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	version := fmt.Sprintf("%d-%s-%x", at.UnixMilli(), values.version, nonce)
	windows := make([]Window, len(Featured))
	for i, name := range Featured {
		windows[i], _ = ParseWindow(name, at)
	}
	var failures []error
	for _, site := range e.sites() {
		lists, err := e.compute(ctx, site, windows, values, at)
		if err != nil {
			failures = append(failures, fmt.Errorf("rankings: precompute %s/%s: %w", site.Domain, site.Language, err))
			continue
		}
		for i, w := range windows {
			for l, r := range lists[i] {
				if err := e.publish(ctx, site, w.Name, l, version, r); err != nil {
					failures = append(failures, err)
				}
			}
		}
	}
	if !r.Final.IsZero() {
		final := day(r.Final)
		var closed []string
		if next := final.AddDate(0, 0, 1); next.Day() == 1 {
			closed = append(closed, "month:"+final.Format("2006-01"))
			if next.Month() == time.January {
				closed = append(closed, "year:"+final.Format("2006"))
			}
		}
		for _, name := range closed {
			w, err := ParseWindow(name, e.now())
			if err != nil {
				failures = append(failures, err)
				continue
			}
			for _, site := range e.sites() {
				if _, err := e.closed(ctx, site, w, List{Kind: e.firstKind()}); err != nil {
					failures = append(failures, fmt.Errorf("rankings: prewarm %s: %w", name, err))
				}
			}
		}
	}
	return errors.Join(failures...)
}

// publish stores a featured list's new version, points at it, and keeps the
// replaced one briefly for its cursors.
func (e *Engine) publish(ctx context.Context, site Site, window string, l List, version string, r *Ranking) error {
	if err := e.store.set(ctx, dataKey(site, window, l, version), encodeRanking(r), featuredTTL); err != nil {
		return err
	}
	ptr := pointerKey(site, window, l)
	previous, _, err := e.store.get(ctx, ptr)
	if err != nil {
		return err
	}
	if err := e.store.set(ctx, ptr, []byte(version), featuredTTL); err != nil {
		return err
	}
	e.memo.forget(ptr)
	if len(previous) > 0 && string(previous) != version {
		return e.store.expire(ctx, dataKey(site, window, l, string(previous)), supersededTTL)
	}
	return nil
}

const finalKey = "rank:final"

func (e *Engine) markFinal(ctx context.Context, final time.Time) error {
	current, err := e.final(ctx)
	if err != nil || !final.After(current) {
		return err
	}
	e.memo.forget(finalKey)
	return e.store.set(ctx, finalKey, []byte(final.Format(time.DateOnly)), 0)
}

// final is the newest day whose last rebuild has run; zero before one has.
func (e *Engine) final(ctx context.Context) (time.Time, error) {
	raw, err := e.memo.pointer(ctx, e.store, finalKey)
	if err != nil || raw == "" {
		return time.Time{}, err
	}
	return time.Parse(time.DateOnly, raw)
}

func (e *Engine) table(name string) string { return pgx.Identifier{e.o.Schema, name}.Sanitize() }

func (e *Engine) now() time.Time { return e.o.Now().UTC() }

// Now is the engine's clock, which windows are read at.
func (e *Engine) Now() time.Time { return e.now() }

// memo keeps small, frequently read values in process briefly: a featured
// list's pointer and the outcome_values version.
type memo struct {
	mu      sync.Mutex
	entries map[string]memoEntry
	vals    *outcomeValues
	valsAt  time.Time
}

type memoEntry struct {
	value string
	at    time.Time
}

// memoTTL is how long a replica may serve a replaced pointer.
const memoTTL = 15 * time.Second

func (m *memo) pointer(ctx context.Context, s store, key string) (string, error) {
	m.mu.Lock()
	if e, ok := m.entries[key]; ok && time.Since(e.at) < memoTTL {
		m.mu.Unlock()
		return e.value, nil
	}
	m.mu.Unlock()
	raw, _, err := s.get(ctx, key)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	if m.entries == nil {
		m.entries = map[string]memoEntry{}
	}
	m.entries[key] = memoEntry{value: string(raw), at: time.Now()}
	m.mu.Unlock()
	return string(raw), nil
}

func (m *memo) forget(key string) {
	m.mu.Lock()
	delete(m.entries, key)
	m.mu.Unlock()
}

// valuesTTL is how long a replica ranks closed ranges by a replaced
// outcome_values version.
const valuesTTL = time.Minute

func (m *memo) values(ctx context.Context, e *Engine) (outcomeValues, error) {
	m.mu.Lock()
	if m.vals != nil && time.Since(m.valsAt) < valuesTTL {
		v := *m.vals
		m.mu.Unlock()
		return v, nil
	}
	m.mu.Unlock()
	v, err := e.loadValues(ctx)
	if err != nil {
		return v, err
	}
	m.mu.Lock()
	m.vals, m.valsAt = &v, time.Now()
	m.mu.Unlock()
	return v, nil
}
