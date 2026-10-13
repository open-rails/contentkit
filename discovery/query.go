package discovery

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
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
	// DisplayLanguage is the language of titles and group names and the
	// preferred language of each card's version; Language, else the site's
	// first.
	DisplayLanguage string
	// Mode is lock (the default) or hide.
	Mode access.Mode
}

// Filter selects contents: what lists, search and release dates share.
type Filter struct {
	Kind string
	// Groups are all required (effective on the card's version); a position
	// sort orders by the first.
	Groups        []string
	ExcludeGroups []string
	// Traits are all required on the card's version.
	Traits []string
	// Attributes are "name:value" equalities on Kind's declared filterable
	// attributes, all required.
	Attributes []string
	// ReleasedOn is a day (YYYY-MM-DD) or month (YYYY-MM) of publication in
	// TimeZone.
	ReleasedOn string
	// TimeZone is an IANA zone; UTC by default.
	TimeZone string
	// ViewStatus is unseen, in_progress or completed: the signed-in
	// reader's own.
	ViewStatus string
	// OwnerGroups keeps content one of these groups owns (a channel's feed);
	// owning is not membership and opens nothing.
	OwnerGroups []string
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
	display string
	path    string // language of card paths; "" unprefixed
	loc     *time.Location
}

func (q *sel) and(pred string, args pgx.NamedArgs) {
	q.where = append(q.where, pred)
	for k, v := range args {
		q.args[k] = v
	}
}

func (q *sel) predicate() string { return strings.Join(q.where, " AND ") }

// compile resolves the reader once and renders the filter over f.
func (s *Service) compile(ctx context.Context, r Reader, f Filter) (*sel, error) {
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
	q.path = cmpOr(r.DisplayLanguage, r.Language)
	q.args["d_display"], q.args["d_langs"] = q.display, s.languages()
	if f.TimeZone != "" {
		if q.loc, err = time.LoadLocation(f.TimeZone); err != nil {
			return nil, fmt.Errorf("%w: time zone %q", ErrInvalid, f.TimeZone)
		}
	}
	owners, err := uniqueIDs("owner_group", f.OwnerGroups)
	if err != nil {
		return nil, err
	}
	// A channel's feed shows its editors the channel's drafts too.
	v, err := s.o.Visibility.Viewer(ctx, catalog.ViewerOptions{Actor: r.Actor, Host: r.Host, Language: r.Language, Mode: r.Mode, EditorGroups: owners})
	if err != nil {
		return nil, err
	}
	q.viewer = v
	listed, args := v.Listed("f")
	q.and("f.tenant_id = @d_tenant", nil)
	q.and("("+listed+")", args)
	var kind kinds.Kind
	if f.Kind != "" {
		var ok bool
		if kind, ok = s.o.Kinds.Kinds[f.Kind]; !ok {
			return nil, fmt.Errorf("%w: kind %q is not configured", ErrInvalid, f.Kind)
		}
		q.and("f.kind = @d_kind", pgx.NamedArgs{"d_kind": f.Kind})
	}
	if gs, err := uniqueIDs("groups", f.Groups); err != nil {
		return nil, err
	} else if len(gs) > 0 {
		q.and("f.group_ids @> @d_groups::text[]", pgx.NamedArgs{"d_groups": gs})
	}
	if gs, err := uniqueIDs("exclude_groups", f.ExcludeGroups); err != nil {
		return nil, err
	} else if len(gs) > 0 {
		q.and("NOT (f.group_ids && @d_exclude::text[])", pgx.NamedArgs{"d_exclude": gs})
	}
	if len(owners) > 0 {
		q.and("f.owner_group_id = ANY(@d_owners::text[])", pgx.NamedArgs{"d_owners": owners})
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
	if f.ViewStatus != "" {
		if err := s.viewStatus(ctx, q, r.Actor, f.Kind, f.ViewStatus); err != nil {
			return nil, err
		}
	}
	return q, nil
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
	if !actor.Person() {
		return fmt.Errorf("%w: view_status needs a signed-in reader", ErrInvalid)
	}
	if s.recs == nil {
		return ErrNoSignals
	}
	subject := signal.Subject{UserID: actor.ID}
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
	SortMostViewed    = "most_viewed"
	SortPosition      = "position"
	SortRandom        = "random"
)

// order is one parsed sort: an expression over f constant across a content's
// versions, its direction and the type its cursor value casts to.
type order struct {
	raw  string
	expr string
	desc bool
	cast string
	args pgx.NamedArgs
}

// titleKey is a content's title sort key in the display language's text.
const titleKey = `coalesce((SELECT lower(coalesce(nullif(x.sort_key, ''), x.title)) FROM %s x WHERE x.tenant_id = f.tenant_id AND x.content_id = f.content_id
 ORDER BY x.language = @d_display DESC, array_position(@d_langs::text[], x.language), x.language LIMIT 1), '')`

func (s *Service) parseSort(raw string, f Filter) (order, error) {
	if raw == "" {
		raw = s.o.Kinds.Kinds[f.Kind].Sort
	}
	if raw == "" {
		raw = SortNewest
	}
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
		o.expr, o.args["d_period"] = "coalesce((f.popularity->'score'->>@d_period::text)::float8, 0)", cmpOr(period, popularity.DefaultPeriod)
	case SortTopRated:
		if period == "" || period == "all" {
			o.expr = "f.rating"
		} else {
			o.expr, o.args["d_period"] = "coalesce((f.popularity->'rating'->>@d_period::text)::float8, 0)", period
		}
	case SortMostFavorited:
		o.expr, o.cast = "f.favorites::bigint", "bigint"
	case SortMostViewed:
		o.expr, o.cast = "coalesce((f.popularity->>'views')::bigint, 0) + f.view_baseline", "bigint"
	case SortPosition:
		if len(f.Groups) == 0 {
			return o, fmt.Errorf("%w: sort position needs groups: it orders by the first", ErrInvalid)
		}
		o.expr = fmt.Sprintf(`coalesce((SELECT min(m.position) FROM %s m WHERE m.tenant_id = f.tenant_id AND m.group_id = @d_pgroup
 AND m.content_id = f.content_id AND m.state = 'active'), 2147483647)::bigint`, s.t.memberships)
		o.cast, o.desc, o.args["d_pgroup"] = "bigint", false, strings.TrimSpace(f.Groups[0])
	case SortRandom:
		o.expr, o.cast, o.desc = "md5(f.content_id || @d_seed::text)", "text", false
	default:
		return o, fmt.Errorf("%w: unknown sort %q", ErrInvalid, raw)
	}
	return o, nil
}

// cursor continues a sorted list after the item it names.
type cursor struct {
	Sort  string `json:"s"`
	Value string `json:"v"`
	ID    string `json:"id"`
	Seed  string `json:"r,omitempty"`
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

// preference orders a content's matching versions: the display language's
// first, then position.
const preference = `(@d_display::text = ANY(f.languages)) DESC, f.position, f.version_id`

// page lists one sorted page of contents and the cursor after it.
func (s *Service) page(ctx context.Context, q *sel, o order, after string, limit int) ([]pick, string, error) {
	cur, err := decodeCursor(after, o.raw)
	if err != nil {
		return nil, "", err
	}
	args := pgx.NamedArgs{"d_limit": limit + 1}
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
	after = "true"
	if cur.ID != "" {
		after = fmt.Sprintf("(c.sk, c.content_id) %s (@d_cv::%s, @d_cid::text)", cmp, o.cast)
		args["d_cv"], args["d_cid"] = cur.Value, cur.ID
	}
	sql := fmt.Sprintf(`SELECT c.content_id, c.version_id, c.sk::text FROM (
 SELECT DISTINCT ON (f.content_id) f.content_id, f.version_id, (%s) AS sk FROM %s f WHERE %s ORDER BY f.content_id, %s) c
WHERE %s ORDER BY c.sk %s, c.content_id %s LIMIT @d_limit`, o.expr, s.t.facets, q.predicate(), preference, after, dir, dir)
	rows, err := s.o.Pool.Query(ctx, sql, args)
	if err != nil {
		return nil, "", err
	}
	type row struct {
		pick
		sk string
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.content, &x.version, &x.sk)
	})
	if err != nil {
		return nil, "", err
	}
	out := make([]pick, 0, min(len(got), limit))
	next := ""
	for i, x := range got {
		if i == limit {
			last := got[limit-1]
			next = cursor{Sort: o.raw, Value: last.sk, ID: last.content, Seed: cur.Seed}.encode()
			break
		}
		out = append(out, x.pick)
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
		return p, r.Scan(&p.content, &p.version)
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
