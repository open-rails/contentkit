package discovery

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/edition"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/popularity"
	"github.com/open-rails/contentkit/signal"
)

// Reader is who reads, on which site, in which language.
type Reader struct {
	Actor access.Actor
	// Host is the request's host: it picks the site scope.
	Host string
	// Language lists only content with a live version serving it.
	Language string
	// DisplayLanguage is the language of titles and entity names and the
	// preferred language of each card's version; Language, else the site's
	// first.
	DisplayLanguage string
	// Mode is lock (the default) or hide.
	Mode access.Mode
}

// Filter selects contents: what lists, search and release dates share.
type Filter struct {
	Kind string
	// keepUnplayable keeps versions whose media does not play yet, which
	// browsing leaves out: a reader's own history, favorites, reactions and
	// known cards.
	keepUnplayable bool
	// named are entities a search names; the reader's content filters do not
	// hide them.
	named []string
	// IDs bounds a public hydration read; normal visibility/domain rules apply.
	IDs []string
	// Entities are all required (effective on the card's version); a position
	// sort orders by the first, which must be of an ordered entity kind. A
	// reader's content filter never hides an entity's content from a list
	// requiring that entity.
	Entities        []string
	ExcludeEntities []string
	// Traits are all required on the card's version.
	Traits []string
	// Attributes are "name:value" equalities on Kind's declared filterable
	// attributes, all required.
	Attributes []string
	// ReleasedOn is a day (YYYY-MM-DD) or month (YYYY-MM) of publication in
	// TimeZone.
	ReleasedOn string
	// PublishedWithin keeps currently live publications in this popularity
	// period (7d, 30d, 90d, 365d or all). It does not change the ranking window.
	PublishedWithin string
	publishedAsOf   time.Time
	// TimeZone is an IANA zone; UTC by default.
	TimeZone string
	// ViewStatus is unseen, in_progress or completed: the signed-in
	// reader's own.
	ViewStatus string
	// ChannelIDs keeps content one of these channels owns (the original feed);
	// owning is not membership and opens nothing.
	ChannelIDs []string
	// includeReposts is enabled only by the public channel feed.
	includeReposts bool
	// Creators keeps content one of these users is credited on.
	Creators []string
	// Access keeps content at one of these levels: public is what anyone
	// opens now (audience releases included), the others what stays locked at that
	// level.
	Access []string
	// Tiers keeps content locked behind one of these tiers.
	Tiers []string
}

// View statuses.
const (
	Unseen     = "unseen"
	InProgress = "in_progress"
	Completed  = "completed"
)

// historyDepth bounds the reader history a view_status filter reads.
const historyDepth = 10_000

// sel is one compiled request: the reader's predicate over content_facets
// row f, with its args.
type sel struct {
	where   []string
	args    pgx.NamedArgs
	reader  Reader
	viewer  *catalog.Viewer
	display string // the language of cards' text and paths
	loc     *time.Location
	asOf    *time.Time // publication-age window pinned by a paginated query
}

func (q *sel) and(pred string, args pgx.NamedArgs) {
	q.where = append(q.where, pred)
	for k, v := range args {
		q.args[k] = v
	}
}

func (q *sel) predicate() string { return strings.Join(q.where, " AND ") }

// with is a copy of q also requiring pred.
func (q *sel) with(pred string, args pgx.NamedArgs) *sel {
	c := *q
	c.where, c.args = slices.Clone(q.where), maps.Clone(q.args)
	c.and(pred, args)
	return &c
}

// compile resolves the reader once and renders the filter over f.
func (s *Service) compile(ctx context.Context, r Reader, f Filter) (*sel, error) {
	return s.compileWithViewer(ctx, r, f, nil)
}

// compileKnown preserves the normal visibility policy and also retains
// temporarily unplayable items in a bounded known-ID selection or history.
func (s *Service) compileKnown(ctx context.Context, r Reader, f Filter) (*sel, error) {
	v, err := s.o.Visibility.Viewer(ctx, catalog.ViewerOptions{Actor: r.Actor, Host: r.Host, Language: r.Language, Mode: r.Mode})
	if err != nil {
		if errors.Is(err, catalog.ErrInvalid) {
			return nil, fmt.Errorf("%w: %s", ErrInvalid, err)
		}
		return nil, err
	}
	f.keepUnplayable = true
	return s.compileWithViewer(ctx, r, f, v)
}

// compileWithViewer reuses a resolved request viewer for a private selection
// under the same domain, entitlements and captured clock. Nil resolves normally.
func (s *Service) compileWithViewer(ctx context.Context, r Reader, f Filter, v *catalog.Viewer) (*sel, error) {
	if r.Mode == "" {
		r.Mode = access.Lock
	}
	if r.Mode != access.Lock && r.Mode != access.Hide {
		return nil, fmt.Errorf("%w: mode %q is lock or hide", ErrInvalid, r.Mode)
	}
	if r.Language != "" {
		if err := s.checkLanguage(r.Language); err != nil {
			return nil, err
		}
	}
	q := &sel{args: pgx.NamedArgs{"d_tenant": s.o.Tenant}, reader: r, loc: time.UTC}
	var err error
	if q.display, err = s.displayLanguage(cmpOr(r.DisplayLanguage, r.Language)); err != nil {
		return nil, err
	}
	q.args["d_display"], q.args["d_langs"] = q.display, s.languages()
	if f.TimeZone != "" {
		if q.loc, err = time.LoadLocation(f.TimeZone); err != nil {
			return nil, fmt.Errorf("%w: time zone %q", ErrInvalid, f.TimeZone)
		}
	}
	owners, err := uniqueIDs("channel_id", f.ChannelIDs)
	if err != nil {
		return nil, err
	}
	required, err := uniqueIDs("groups", f.Entities)
	if err != nil {
		return nil, err
	}
	excluded, err := uniqueIDs("exclude_groups", f.ExcludeEntities)
	if err != nil {
		return nil, err
	}
	selected := append(append(append([]string{}, required...), excluded...), owners...)
	if len(selected) > 0 {
		var internal bool
		if err := s.o.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.t.entities+` WHERE tenant_id = $1 AND id = ANY($2::text[]) AND internal)`, s.o.Tenant, selected).Scan(&internal); err != nil {
			return nil, err
		}
		if internal {
			return nil, fmt.Errorf("%w: internal entities cannot be used in public filters", ErrInvalid)
		}
	}
	// A channel's feed shows its editors the channel's drafts too.
	if v == nil {
		v, err = s.o.Visibility.Viewer(ctx, catalog.ViewerOptions{Actor: r.Actor, Host: r.Host, Language: r.Language, Mode: r.Mode, EditorEntities: owners,
			Requested: append(required, f.named...)})
		if err != nil {
			if errors.Is(err, catalog.ErrInvalid) {
				return nil, fmt.Errorf("%w: %s", ErrInvalid, err)
			}
			return nil, err
		}
	}
	q.viewer = v
	listed, args := v.Listed("f")
	q.and("f.tenant_id = @d_tenant", nil)
	q.and("("+listed+")", args)
	// A video lists once it plays; its page still opens it.
	if !f.keepUnplayable {
		q.and(v.Playable("f"), nil)
	}
	if f.IDs != nil {
		if len(f.IDs) == 0 || len(f.IDs) > 100 {
			return nil, fmt.Errorf("%w: 1-100 content ids", ErrInvalid)
		}
		ids := make([]string, 0, len(f.IDs))
		for _, id := range f.IDs {
			if err := contentref.ValidateID(id); err != nil {
				return nil, fmt.Errorf("%w: content ids", ErrInvalid)
			}
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		q.and("f.content_id = ANY(@d_content_ids::text[])", pgx.NamedArgs{"d_content_ids": ids})
	}
	var kind kinds.Kind
	if f.Kind != "" {
		var ok bool
		if kind, ok = s.o.Kinds.Kinds[f.Kind]; !ok {
			return nil, fmt.Errorf("%w: kind %q is not configured", ErrInvalid, f.Kind)
		}
		q.and("f.kind = @d_kind", pgx.NamedArgs{"d_kind": f.Kind})
	}
	if len(required) > 0 {
		q.and("f.entity_ids @> @d_groups::text[]", pgx.NamedArgs{"d_groups": required})
	}
	if len(excluded) > 0 {
		q.and("NOT (f.entity_ids && @d_exclude::text[])", pgx.NamedArgs{"d_exclude": excluded})
	}
	if len(owners) > 0 {
		owned := "f.owner_entity_id = ANY(@d_owners::text[])"
		if f.includeReposts {
			owned = "(" + owned + " OR EXISTS (SELECT 1 FROM " + s.t.reposts + " rp WHERE rp.tenant_id=f.tenant_id AND rp.content_id=f.content_id AND rp.channel_id=ANY(@d_owners::text[])))"
			// A public feed never exposes unpublished or deleted originals, even to staff.
			q.and("NOT f.deleted AND f.live_from <= @cv_now::timestamptz AND (@cv_language = '' OR @cv_language = ANY(f.languages))", nil)
		}
		q.and(owned, pgx.NamedArgs{"d_owners": owners})
	}
	if cs, err := uniqueIDs("creators", f.Creators); err != nil {
		return nil, err
	} else if len(cs) > 0 {
		q.and(`EXISTS (SELECT 1 FROM `+s.t.creators+` cc WHERE cc.tenant_id = f.tenant_id AND cc.content_id = f.content_id
 AND cc.user_id = ANY(@d_creators::text[]))`, pgx.NamedArgs{"d_creators": cs})
	}
	if err := s.accessFilter(q, f.Access, f.Tiers); err != nil {
		return nil, err
	}
	if ts, err := uniqueIDs("traits", f.Traits); err != nil {
		return nil, err
	} else if len(ts) > 0 {
		for _, t := range ts {
			if f.Kind != "" && !slices.Contains(kind.Versions.Traits, t) {
				return nil, fmt.Errorf("%w: kind %s declares no trait %q", ErrInvalid, f.Kind, t)
			}
		}
		q.and("f.traits @> @d_traits::text[]", pgx.NamedArgs{"d_traits": ts})
	}
	if len(f.Attributes) > 0 {
		attrs, err := attributes(f.Kind, kind, f.Attributes)
		if err != nil {
			return nil, err
		}
		q.and("f.attributes @> @d_attrs::jsonb", pgx.NamedArgs{"d_attrs": attrs})
	}
	if f.ReleasedOn != "" {
		from, to, err := releasedOn(f.ReleasedOn, q.loc)
		if err != nil {
			return nil, err
		}
		q.and("f.published_at >= @d_from AND f.published_at < @d_to", pgx.NamedArgs{"d_from": from, "d_to": to})
	}
	if f.PublishedWithin != "" {
		now := s.now()
		if !f.publishedAsOf.IsZero() {
			if f.publishedAsOf.After(now) {
				return nil, fmt.Errorf("%w: cursor publication time is in the future", ErrInvalid)
			}
			now = f.publishedAsOf
		}
		q.asOf = &now
		window, err := popularity.WindowForPeriod(f.PublishedWithin, now)
		if err != nil {
			return nil, fmt.Errorf("%w: published_within %q is one of %v", ErrInvalid, f.PublishedWithin, popularity.Periods)
		}
		q.and("f.published_at IS NOT NULL AND f.published_at <= @d_published_now AND f.live_from <= @d_published_now AND NOT f.deleted", pgx.NamedArgs{"d_published_now": now})
		if !window.From.IsZero() {
			q.and("f.published_at >= @d_published_from", pgx.NamedArgs{"d_published_from": window.From})
		}
	}
	if f.ViewStatus != "" {
		if err := s.viewStatus(ctx, q, r.Actor, f.Kind, f.ViewStatus); err != nil {
			return nil, err
		}
	}
	return q, nil
}

// accessFilter keeps content anyone opens now (public), content locked at
// the other levels named, and content locked behind the tiers named.
func (s *Service) accessFilter(q *sel, levels, tiers []string) error {
	levels, err := uniqueIDs("access", levels)
	if err != nil {
		return err
	}
	if tiers, err = uniqueIDs("tiers", tiers); err != nil {
		return err
	}
	for _, l := range levels {
		if !slices.Contains([]access.Level{access.Public, access.TierLevel, access.ChannelLevel, access.PPV, access.ChannelPPV}, access.Level(l)) {
			return fmt.Errorf("%w: access %q is public, tier, group, ppv or group_ppv", ErrInvalid, l)
		}
	}
	for _, t := range tiers {
		if !slices.Contains(s.o.Kinds.Tiers, t) {
			return fmt.Errorf("%w: tier %q is not configured", ErrInvalid, t)
		}
	}
	free := access.PublicSQL("f")
	if len(levels) > 0 {
		q.and("(("+free+" AND 'public' = ANY(@d_levels::text[])) OR (NOT "+free+" AND (f.access_level = ANY(@d_levels::text[]) OR (cardinality("+access.ReleasedTiersSQL("f")+")>0 AND 'tier'=ANY(@d_levels::text[])))))",
			pgx.NamedArgs{"d_levels": levels})
	}
	if len(tiers) > 0 {
		q.and("(NOT "+free+" AND ((f.access_level = 'tier' AND f.access_tiers && @d_tiers::text[]) OR "+access.ReleasedTiersSQL("f")+" && @d_tiers::text[]))", pgx.NamedArgs{"d_tiers": tiers})
	}
	return nil
}

// uniqueIDs trims and dedupes a filter's ids, at most 50.
func uniqueIDs(name string, in []string) ([]string, error) {
	out := []string{}
	for _, id := range in {
		if id = strings.TrimSpace(id); id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	if len(out) > 50 {
		return nil, fmt.Errorf("%w: at most 50 %s", ErrInvalid, name)
	}
	return out, nil
}

// attributes renders declared filterable attributes as one JSON containment.
func attributes(name string, kind kinds.Kind, raw []string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: attribute filters need a kind", ErrInvalid)
	}
	declared := map[string]kinds.Attribute{}
	values := map[string]any{}
	for _, a := range raw {
		n, v, ok := strings.Cut(a, ":")
		attr, declaredOK := kind.Attributes[n]
		if !ok || !declaredOK || !attr.Filterable {
			return "", fmt.Errorf("%w: attribute %q is not a filterable attribute of %s", ErrInvalid, n, name)
		}
		declared[n] = attr
		var typed any = v
		switch attr.Type {
		case "bool":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return "", fmt.Errorf("%w: attribute %s is a bool", ErrInvalid, n)
			}
			typed = b
		case "int":
			i, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return "", fmt.Errorf("%w: attribute %s is an int", ErrInvalid, n)
			}
			typed = i
		}
		values[n] = typed
	}
	norm, err := kinds.ValidateAttributes(declared, values)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	b, err := json.Marshal(norm)
	return string(b), err
}

// releasedOn is a day or month in loc as a time range.
func releasedOn(v string, loc *time.Location) (time.Time, time.Time, error) {
	if d, err := time.ParseInLocation(time.DateOnly, v, loc); err == nil {
		return d, d.AddDate(0, 0, 1), nil
	}
	if m, err := time.ParseInLocation("2006-01", v, loc); err == nil {
		return m, m.AddDate(0, 1, 0), nil
	}
	return time.Time{}, time.Time{}, fmt.Errorf("%w: released_on %q is YYYY-MM-DD or YYYY-MM", ErrInvalid, v)
}

// viewStatus keeps the reader's in-progress or completed contents, or drops
// those they have seen, read from the signal plane.
func (s *Service) viewStatus(ctx context.Context, q *sel, actor access.Actor, kind, status string) error {
	if !actor.Member() {
		return fmt.Errorf("%w: view_status needs a signed-in reader", ErrInvalid)
	}
	if err := s.signals(); err != nil {
		return err
	}
	subject := signal.Subject{UserID: actor.AccountID()}
	var ids []string
	switch status {
	case InProgress, Completed:
		hs := signal.HistoryInProgress
		if status == Completed {
			hs = signal.HistoryCompleted
		}
		rows, err := s.o.Hub.History(ctx, subject, signal.HistoryOptions{ContentKind: kind, Status: hs, Limit: historyDepth})
		if err != nil {
			return err
		}
		for _, r := range rows {
			ids = append(ids, r.ContentID)
		}
		q.and("f.content_id = ANY(@d_viewed::text[])", pgx.NamedArgs{"d_viewed": nonNil(ids)})
	case Unseen:
		ks := []string{kind}
		if kind == "" {
			ks = s.kindNames()
		}
		for _, k := range ks {
			seen, err := s.o.Hub.SeenIDs(ctx, subject, k)
			if err != nil {
				return err
			}
			for id := range seen {
				ids = append(ids, id)
			}
		}
		q.and("f.content_id <> ALL(@d_seen::text[])", pgx.NamedArgs{"d_seen": nonNil(ids)})
	default:
		return fmt.Errorf("%w: view_status %q is unseen, in_progress or completed", ErrInvalid, status)
	}
	return nil
}

func (s *Service) kindNames() []string {
	out := make([]string, 0, len(s.o.Kinds.Kinds))
	for k := range s.o.Kinds.Kinds {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Sorts.
const (
	SortNewest        = "newest"
	SortOldest        = "oldest"
	SortTitle         = "title"
	SortPopular       = "popular"
	SortTopRated      = "top_rated"
	SortMostFavorited = "most_favorited"
	SortPosition      = "position"
	SortRandom        = "random"
)

// order is one parsed sort: an expression over f constant across a content's
// versions, its direction and the type its cursor value casts to.
type order struct {
	raw  string
	expr string
	join string
	desc bool
	cast string
	args pgx.NamedArgs
	// edition keys can differ between a content's versions (version
	// memberships): a content sorts by its shown edition's.
	edition bool
}

// titleKey is a content's title sort key in the display language's text.
const titleKey = `coalesce((SELECT lower(coalesce(nullif(x.sort_key, ''), x.title)) FROM %s x WHERE x.tenant_id = f.tenant_id AND x.content_id = f.content_id
 ORDER BY x.language = @d_display DESC, array_position(@d_langs::text[], x.language), x.language LIMIT 1), '')`

func (s *Service) parseSort(ctx context.Context, raw string, f Filter) (order, error) {
	raw = s.sortName(raw, f.Kind)
	name, period, hasPeriod := strings.Cut(raw, ":")
	o := order{raw: raw, desc: true, cast: "float8", args: pgx.NamedArgs{}}
	if hasPeriod && name != SortPopular && name != SortTopRated {
		return o, fmt.Errorf("%w: sort %q takes no period", ErrInvalid, raw)
	}
	if hasPeriod && !slices.Contains(popularity.Periods, period) {
		return o, fmt.Errorf("%w: sort period %q is one of %v", ErrInvalid, period, popularity.Periods)
	}
	switch name {
	case SortNewest, SortOldest:
		o.expr, o.cast, o.desc = "coalesce(f.published_at, 'epoch'::timestamptz)", "timestamptz", name == SortNewest
	case SortTitle:
		o.expr, o.cast, o.desc = fmt.Sprintf(titleKey, s.t.text), "text", false
	case SortPopular:
		window, err := popularity.WindowForPeriod(cmpOr(period, popularity.DefaultPeriod), s.now())
		if err != nil {
			return o, err
		}
		return s.standingOrder(raw, window, true), nil
	case SortTopRated:
		if period == "" || period == "all" {
			o.expr = content.RatingSQL("coalesce(counts.likes, 0)", "coalesce(counts.dislikes, 0)")
			o.join = s.countsJoin()
			break
		}
		window, err := popularity.WindowForPeriod(period, s.now())
		if err != nil {
			return o, err
		}
		return s.standingOrder(raw, window, false), nil
	case SortMostFavorited:
		o.expr, o.cast, o.join = "coalesce(counts.favorites, 0)::bigint", "bigint", s.countsJoin()

	case SortPosition:
		if len(f.Entities) == 0 {
			return o, fmt.Errorf("%w: sort position needs groups: it orders by the first", ErrInvalid)
		}
		entity := strings.TrimSpace(f.Entities[0])
		var kind string
		err := s.o.Pool.QueryRow(ctx, `SELECT kind FROM `+s.t.entities+` WHERE tenant_id = $1 AND id = $2`, s.o.Tenant, entity).Scan(&kind)
		switch {
		case errors.Is(err, pgx.ErrNoRows): // lists nothing
		case err != nil:
			return o, err
		case !s.o.Kinds.EntityKinds[kind].Ordered:
			return o, fmt.Errorf("%w: sort position needs an ordered group first; %s groups have no order", ErrInvalid, kind)
		}
		o.expr = fmt.Sprintf(`coalesce((SELECT min(m.position) FROM %s m WHERE m.tenant_id = f.tenant_id AND m.entity_id = @d_pgroup
 AND m.content_id = f.content_id AND (m.version_id IS NULL OR m.version_id = f.version_id) AND m.state = 'active'), 2147483647)::bigint`, s.t.memberships)
		o.cast, o.desc, o.edition, o.args["d_pgroup"] = "bigint", false, true, entity
	case SortRandom:
		o.expr, o.cast, o.desc = "md5(f.content_id || @d_seed::text)", "text", false
	default:
		return o, fmt.Errorf("%w: unknown sort %q", ErrInvalid, raw)
	}
	return o, nil
}

// countsJoin reads the current content-level aggregate once per content,
// irrespective of how many versions match. Version reactions remain separate.
func (s *Service) countsJoin() string {
	return "LEFT JOIN " + s.t.counts + " counts ON counts.tenant_id = f.tenant_id AND counts.content_kind = f.kind AND counts.content_id = f.content_id AND counts.content_version_id IS NULL"
}

func (s *Service) sortName(raw, kind string) string {
	if raw == "" {
		raw = s.o.Kinds.Kinds[kind].Sort
	}
	if raw == "" {
		raw = SortNewest
	}
	return raw
}

func publicationCursor(f Filter, raw, sort string) (Filter, error) {
	if f.PublishedWithin == "" || raw == "" {
		return f, nil
	}
	cur, err := decodeCursor(raw, sort)
	if err != nil {
		return f, err
	}
	if cur.AsOf == nil || cur.AsOf.IsZero() {
		return f, fmt.Errorf("%w: cursor publication time is required", ErrInvalid)
	}
	f.publishedAsOf = *cur.AsOf
	return f, nil
}

// cursor continues a sorted list after the item it names.
type cursor struct {
	Sort  string     `json:"s"`
	Value string     `json:"v"`
	ID    string     `json:"id"`
	Seed  string     `json:"r,omitempty"`
	AsOf  *time.Time `json:"at,omitempty"`
}

func (c cursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(raw, sort string) (cursor, error) {
	var c cursor
	if raw == "" {
		return c, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil || c.ID == "" || c.Sort != sort {
		return c, fmt.Errorf("%w: cursor", ErrInvalid)
	}
	return c, nil
}

func newSeed() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pick is a content item and the version its card shows.
type pick struct {
	content, version string
}

// preference orders a content's matching versions by the default-version
// rule in the display language (internal/edition).
var preference = edition.Order("f", "@d_display::text")

// page lists one sorted page of contents and the cursor after it.
func (s *Service) page(ctx context.Context, q *sel, o order, after string, limit, offset int) ([]pick, string, error) {
	cur, err := decodeCursor(after, o.raw)
	if err != nil {
		return nil, "", err
	}
	args := pgx.NamedArgs{"d_limit": limit + 1, "d_offset": offset}
	for k, v := range q.args {
		args[k] = v
	}
	for k, v := range o.args {
		args[k] = v
	}
	if o.raw == SortRandom && cur.Seed == "" {
		cur.Seed = newSeed()
	}
	args["d_seed"] = cur.Seed
	dir, cmp := "ASC", ">"
	if o.desc {
		dir, cmp = "DESC", "<"
	}
	// Order the contents first, then pick each one's version. A key every
	// version shares entities in the sort's order, so an index can end the scan
	// at the page; an edition's key is read from the edition the card shows.
	if cur.ID != "" {
		args["d_cv"], args["d_cid"] = cur.Value, cur.ID
	}
	var sql string
	switch {
	case o.join != "":
		// A joined score is per content: take each content once, then score it.
		where := "true"
		if name, _, _ := strings.Cut(o.raw, ":"); name == SortPopular {
			where = "(" + o.expr + ") > 0"
		}
		if cur.ID != "" {
			where += fmt.Sprintf(" AND ((%s), f.content_id) %s (@d_cv::%s, @d_cid::text)", o.expr, cmp, o.cast)
		}
		sql = fmt.Sprintf(`SELECT f.content_id, (%[1]s)::text FROM (SELECT DISTINCT f.tenant_id, f.kind, f.content_id FROM %[2]s f WHERE %[3]s) f %[4]s
 WHERE %[5]s ORDER BY (%[1]s) %[6]s, f.content_id %[6]s LIMIT @d_limit OFFSET @d_offset`, o.expr, s.t.facets, q.predicate(), o.join, where, dir)
	case o.edition:
		after := "true"
		if cur.ID != "" {
			after = fmt.Sprintf("(c.sk, c.content_id) %s (@d_cv::%s, @d_cid::text)", cmp, o.cast)
		}
		sql = fmt.Sprintf(`SELECT c.content_id, c.sk::text FROM (
 SELECT DISTINCT ON (f.content_id) f.content_id, (%s) AS sk FROM %s f WHERE %s ORDER BY f.content_id, %s) c
WHERE %s ORDER BY c.sk %s, c.content_id %s LIMIT @d_limit OFFSET @d_offset`, o.expr, s.t.facets, q.predicate(), preference, after, dir, dir)
	default:
		ordered := q
		if cur.ID != "" {
			ordered = q.with(fmt.Sprintf("((%s), f.content_id) %s (@d_cv::%s, @d_cid::text)", o.expr, cmp, o.cast), nil)
		}
		sql = fmt.Sprintf(`SELECT f.content_id, (%[1]s)::text FROM %[2]s f WHERE %[3]s
 GROUP BY (%[1]s), f.content_id ORDER BY (%[1]s) %[4]s, f.content_id %[4]s LIMIT @d_limit OFFSET @d_offset`, o.expr, s.t.facets, ordered.predicate(), dir)
	}
	rows, err := s.o.Pool.Query(ctx, sql, args)
	if err != nil {
		return nil, "", err
	}
	type row struct {
		content, sk string
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.content, &x.sk)
		return x, err
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(got) > limit {
		last := got[limit-1]
		next = cursor{Sort: o.raw, Value: last.sk, ID: last.content, Seed: cur.Seed, AsOf: q.asOf}.encode()
		got = got[:limit]
	}
	ids := make([]string, len(got))
	for i, x := range got {
		ids[i] = x.content
	}
	out, err := s.pickAmong(ctx, q, ids)
	if err != nil {
		return nil, "", err
	}
	return out, next, nil
}

// pickAmong returns the matching contents among ids with their versions, in
// ids order.
func (s *Service) pickAmong(ctx context.Context, q *sel, ids []string) ([]pick, error) {
	if len(ids) == 0 {
		return []pick{}, nil
	}
	args := pgx.NamedArgs{"d_ids": ids}
	for k, v := range q.args {
		args[k] = v
	}
	rows, err := s.o.Pool.Query(ctx, fmt.Sprintf(`SELECT DISTINCT ON (f.content_id) f.content_id, f.version_id FROM %s f
 WHERE %s AND f.content_id = ANY(@d_ids::text[]) ORDER BY f.content_id, %s`, s.t.facets, q.predicate(), preference), args)
	if err != nil {
		return nil, err
	}
	byID, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pick, error) {
		var p pick
		err := r.Scan(&p.content, &p.version)
		return p, err
	})
	if err != nil {
		return nil, err
	}
	found := make(map[string]pick, len(byID))
	for _, p := range byID {
		found[p.content] = p
	}
	out := make([]pick, 0, len(found))
	for _, id := range ids {
		if p, ok := found[id]; ok {
			out = append(out, p)
			delete(found, id)
		}
	}
	return out, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
