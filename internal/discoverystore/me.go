package discoverystore

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/channels"
	discovery "github.com/open-rails/contentkit/discovery"
	entitlement "github.com/open-rails/contentkit/internal/entitlement"
	"github.com/open-rails/contentkit/internal/httpapi"
)

// Favorites pages what the signed-in reader favorited, most recent first: the
// content items their listing shows (one card per item, whichever version
// they favorited) or the channels.
func (s *Service) Favorites(ctx context.Context, o discovery.MineOptions) (discovery.InteractionPage, error) {
	return s.mine(ctx, o, s.t.favorites, "x.value = 1", "x.created_at")
}

func (s *Service) mine(ctx context.Context, o discovery.MineOptions, table, value, at string) (discovery.InteractionPage, error) {
	ctx = entitlement.WithMemo(ctx)
	out := discovery.InteractionPage{Items: []discovery.Interaction{}}
	if !o.Actor.Member() {
		return out, fmt.Errorf("%w: favorites belong to a signed-in reader", ErrInvalid)
	}
	var kinds []string
	switch o.Target {
	case "", discovery.TargetContents:
		o.Target = discovery.TargetContents
		for k := range s.o.Kinds.Kinds {
			kinds = append(kinds, k)
		}
		if _, ok := s.o.Kinds.Kinds[o.Kind]; o.Kind != "" && !ok {
			return out, fmt.Errorf("%w: kind %q is not configured", ErrInvalid, o.Kind)
		}
	case discovery.TargetChannels:
		if s.o.Kinds.Channels == nil || s.o.Channels == nil {
			return out, fmt.Errorf("%w: channels are not configured", ErrInvalid)
		}
		if o.Kind != "" && o.Kind != "channel" {
			return out, fmt.Errorf("%w: channel favorites have no metadata kind", ErrInvalid)
		}
		kinds = []string{"channel"}
	default:
		return out, fmt.Errorf("%w: target %q is contents or channels", ErrInvalid, o.Target)
	}
	if o.Kind != "" {
		kinds = []string{o.Kind}
	}
	afterAt, afterID, err := decodeMineCursor(o.Cursor)
	if err != nil {
		return out, err
	}
	limit := pageLimit(o.Limit, 24)
	args := pgx.NamedArgs{"d_tenant": s.o.Tenant, "d_user": o.Actor.AccountID(), "d_kinds": kinds, "d_limit": limit + 1, "d_after_at": afterAt, "d_after_id": afterID}
	var q *sel
	exists := fmt.Sprintf(`EXISTS (SELECT 1 FROM %s g WHERE g.tenant_id = x.tenant_id AND g.id = x.content_id AND g.kind = x.content_kind AND g.state = 'active' AND NOT g.internal)`, s.t.entities)
	if o.Target == discovery.TargetChannels {
		exists += ` AND EXISTS(SELECT 1 FROM ` + s.t.entities + ` channel WHERE channel.tenant_id=x.tenant_id AND channel.id=x.content_id AND channel.kind='channel' AND channel.auth_scope IS NOT NULL)`
	}
	if o.Target == discovery.TargetContents {
		if q, err = s.compile(ctx, o.Reader, Filter{Kind: o.Kind}); err != nil {
			return out, err
		}
		for k, v := range q.args {
			args[k] = v
		}
		exists = fmt.Sprintf(`EXISTS (SELECT 1 FROM %s f WHERE f.content_id = x.content_id AND %s)`, s.t.facets, q.predicate())
		if o.Summary && o.Cursor == "" {
			sort, sortErr := s.parseSort(ctx, discovery.SortNewest, Filter{})
			if sortErr != nil {
				return out, sortErr
			}
			selection := func(selected *sel) *sel {
				return selected.with(fmt.Sprintf(`EXISTS (SELECT 1 FROM %s x WHERE x.tenant_id = f.tenant_id
 AND x.user_id = @summary_user AND x.content_id = f.content_id AND x.content_kind = f.kind
 AND x.content_kind = ANY(@summary_kinds::text[]) AND %s)`, table, value),
					pgx.NamedArgs{"summary_user": o.Actor.AccountID(), "summary_kinds": kinds})
			}
			out.Summary, err = s.listingSummary(ctx, Filter{Kind: o.Kind}, q, sort, &summaryCandidates{selection: selection})
			if err != nil {
				return out, err
			}
		}
	}
	rows, err := s.o.Pool.Query(ctx, fmt.Sprintf(`SELECT id, value, at FROM (
 SELECT DISTINCT ON (x.content_id) x.content_id AS id, x.value, %[1]s AS at FROM %[2]s x
 WHERE x.tenant_id = @d_tenant AND x.user_id = @d_user AND x.content_kind = ANY(@d_kinds) AND %[3]s AND %[4]s
 ORDER BY x.content_id, %[1]s DESC) m
WHERE @d_after_id = '' OR (at, id) < (@d_after_at, @d_after_id) ORDER BY at DESC, id DESC LIMIT @d_limit`, at, table, value, exists), args)
	if err != nil {
		return out, err
	}
	type row struct {
		id    string
		value int16
		at    time.Time
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.value, &x.at)
		return x, err
	})
	if err != nil {
		return out, err
	}
	if len(got) > limit {
		got = got[:limit]
		last := got[limit-1]
		out.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(last.at.UTC().Format(time.RFC3339Nano) + "|" + last.id))
	}
	ids := make([]string, len(got))
	for i, x := range got {
		ids[i] = x.id
	}
	if o.Target == discovery.TargetChannels {
		cards, err := s.o.Channels.Cards(ctx, ids, cmpOr(o.DisplayLanguage, o.Language))
		if err != nil {
			return out, err
		}
		byID := map[string]channels.Card{}
		for _, c := range cards {
			byID[c.ID] = c
		}
		for _, x := range got {
			if c, ok := byID[x.id]; ok {
				out.Items = append(out.Items, discovery.Interaction{Channel: &c, Value: x.value, At: x.at})
			}
		}
		return out, nil
	}
	picks, err := s.pickAmong(ctx, q, ids)
	if err != nil {
		return out, err
	}
	cards, err := s.cards(ctx, q, picks, cardOptions{chips: o.CardEntities})
	if err != nil {
		return out, err
	}
	byID := make(map[string]discovery.Card, len(cards))
	for _, c := range cards {
		byID[c.ID] = c
	}
	for _, x := range got {
		if c, ok := byID[x.id]; ok {
			out.Items = append(out.Items, discovery.Interaction{Content: &c, Value: x.value, At: x.at})
		}
	}
	return out, nil
}

func decodeMineCursor(raw string) (time.Time, string, error) {
	if raw == "" {
		return time.Time{}, "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	at, id, ok := strings.Cut(string(b), "|")
	if err != nil || !ok || id == "" {
		return time.Time{}, "", fmt.Errorf("%w: cursor", ErrInvalid)
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: cursor", ErrInvalid)
	}
	return t, id, nil
}

var mineParams = []httpapi.Param{
	httpapi.Text("target", "contents (the default) or channels"),
	httpapi.Text("kind", "only this content kind"),
	cardEntities,
	httpapi.Text("cursor", "the previous page's next_cursor"),
	httpapi.Int("limit", "page size: 24 by default, at most 100"),
	httpapi.Bool("summary", "matching content counts for this caller, on the first page only"),
}

var mineRoutes = []httpapi.Route[handler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/me/favorites", Resource: "discovery", Auth: httpapi.User,
		Doc:   "The content items (as their listing shows them) or channels the reader favorited, most recent first.",
		Query: params(mineParams, readerParams), Responses: []httpapi.Reply{httpapi.OK(discovery.InteractionPage{})}},
		Serve: httpapi.H(handler.favorites)},
}

func (h handler) mineOptions(r *http.Request) (discovery.MineOptions, error) {
	limit, err := intParam(r, "limit")
	summary, flagErr := optionalFlag(r, "summary")
	if err == nil {
		err = flagErr
	}
	q := r.URL.Query()
	return discovery.MineOptions{Reader: readerOf(r), Target: q.Get("target"), Kind: q.Get("kind"),
		CardEntities: list(q.Get("card_entities")), Cursor: q.Get("cursor"), Limit: limit, Summary: summary}, err
}

func (h handler) favorites(w http.ResponseWriter, r *http.Request) {
	o, err := h.mineOptions(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	page, err := h.s.Favorites(r.Context(), o)
	h.reply(w, r, page, err)
}
