package discovery

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/httpapi"
)

// Interaction targets: content items or groups.
const (
	TargetContents = "contents"
	TargetGroups   = "entities"
)

// MineOptions select one page of the signed-in reader's favorites or
// reactions.
type MineOptions struct {
	Reader
	// Target is contents (the default) or entities.
	Target string
	// Kind is one content kind or group kind; every kind by default.
	Kind string
	// Value narrows reactions: like or dislike; both by default.
	Value      string
	CardGroups []string
	Cursor     string
	// Limit defaults to 24 and is capped at 100.
	Limit int
}

// Interaction is one content item or group the reader favorited or reacted
// to, as its card shows it now.
type Interaction struct {
	Content *Card             `json:"content,omitempty"`
	Group   *groups.GroupCard `json:"entity,omitempty"`
	// Value is the reaction: 1 like, -1 dislike; 1 for a favorite.
	Value int16 `json:"value"`
	// At is when the reader favorited or last reacted.
	At time.Time `json:"at"`
}

// InteractionPage is one page of the reader's favorites or reactions, most
// recent first.
type InteractionPage struct {
	Items []Interaction `json:"items"`
	// NextCursor pages on; empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// Favorites pages what the signed-in reader favorited, most recent first: the
// content items their listing shows (one card per item, whichever version
// they favorited) or the active groups.
func (s *Service) Favorites(ctx context.Context, o MineOptions) (InteractionPage, error) {
	return s.mine(ctx, o, s.t.favorites, "x.value = 1", "x.created_at")
}

// Reactions pages what the signed-in reader liked or disliked, most recently
// changed first, as Favorites does: tag affinities and preferences read it.
func (s *Service) Reactions(ctx context.Context, o MineOptions) (InteractionPage, error) {
	value := "x.value <> 0"
	switch o.Value {
	case "":
	case "like":
		value = "x.value = 1"
	case "dislike":
		value = "x.value = -1"
	default:
		return InteractionPage{}, fmt.Errorf("%w: value %q is like or dislike", ErrInvalid, o.Value)
	}
	return s.mine(ctx, o, s.t.reactions, value, "x.updated_at")
}

func (s *Service) mine(ctx context.Context, o MineOptions, table, value, at string) (InteractionPage, error) {
	out := InteractionPage{Items: []Interaction{}}
	if !o.Actor.Member() {
		return out, fmt.Errorf("%w: favorites and reactions belong to a signed-in reader", ErrInvalid)
	}
	var kinds []string
	switch o.Target {
	case "", TargetContents:
		o.Target = TargetContents
		for k := range s.o.Kinds.Kinds {
			kinds = append(kinds, k)
		}
		if _, ok := s.o.Kinds.Kinds[o.Kind]; o.Kind != "" && !ok {
			return out, fmt.Errorf("%w: kind %q is not configured", ErrInvalid, o.Kind)
		}
	case TargetGroups:
		for k := range s.o.Kinds.GroupKinds {
			kinds = append(kinds, k)
		}
		if _, ok := s.o.Kinds.GroupKinds[o.Kind]; o.Kind != "" && !ok {
			return out, fmt.Errorf("%w: group kind %q is not configured", ErrInvalid, o.Kind)
		}
	default:
		return out, fmt.Errorf("%w: target %q is contents or entities", ErrInvalid, o.Target)
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
	exists := fmt.Sprintf(`EXISTS (SELECT 1 FROM %s g WHERE g.tenant_id = x.tenant_id AND g.id = x.content_id AND g.kind = x.content_kind AND g.state = 'active' AND NOT g.internal)`, s.t.groups)
	if o.Target == TargetContents {
		if q, err = s.compile(ctx, o.Reader, Filter{Kind: o.Kind}); err != nil {
			return out, err
		}
		for k, v := range q.args {
			args[k] = v
		}
		exists = fmt.Sprintf(`EXISTS (SELECT 1 FROM %s f WHERE f.content_id = x.content_id AND %s)`, s.t.facets, q.predicate())
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
		return x, r.Scan(&x.id, &x.value, &x.at)
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
	if o.Target == TargetGroups {
		gids := make([]groups.GroupID, len(ids))
		for i, id := range ids {
			gids[i] = groups.GroupID(id)
		}
		cards, err := s.o.Groups.Cards(ctx, gids, groups.CardOptions{Language: cmpOr(o.DisplayLanguage, o.Language)})
		if err != nil {
			return out, err
		}
		byID := make(map[string]groups.GroupCard, len(cards))
		for _, c := range cards {
			byID[string(c.ID)] = c
		}
		for _, x := range got {
			if c, ok := byID[x.id]; ok {
				out.Items = append(out.Items, Interaction{Group: &c, Value: x.value, At: x.at})
			}
		}
		return out, nil
	}
	picks, err := s.pickAmong(ctx, q, ids)
	if err != nil {
		return out, err
	}
	cards, err := s.cards(ctx, q, picks, cardOptions{chips: o.CardGroups})
	if err != nil {
		return out, err
	}
	byID := make(map[string]Card, len(cards))
	for _, c := range cards {
		byID[c.ID] = c
	}
	for _, x := range got {
		if c, ok := byID[x.id]; ok {
			out.Items = append(out.Items, Interaction{Content: &c, Value: x.value, At: x.at})
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
	httpapi.Text("target", "contents (the default) or entities"),
	httpapi.Text("kind", "only this content kind or group kind"),
	cardGroups,
	httpapi.Text("cursor", "the previous page's next_cursor"),
	httpapi.Int("limit", "page size: 24 by default, at most 100"),
}

var mineRoutes = []httpapi.Route[handler]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/me/favorites", Resource: "discovery", Auth: httpapi.User,
		Doc:   "The content items (as their listing shows them) or groups the reader favorited, most recent first.",
		Query: params(mineParams, readerParams), Responses: []httpapi.Reply{httpapi.OK(InteractionPage{})}},
		Serve: httpapi.H(handler.favorites)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/me/reactions", Resource: "discovery", Auth: httpapi.User,
		Doc:       "The content items or groups the reader liked or disliked, most recent first: tag affinities and preferences.",
		Query:     params([]httpapi.Param{httpapi.Text("value", "like or dislike; both by default")}, mineParams, readerParams),
		Responses: []httpapi.Reply{httpapi.OK(InteractionPage{})}},
		Serve: httpapi.H(handler.reactions)},
}

func (h handler) mineOptions(r *http.Request) (MineOptions, error) {
	limit, err := intParam(r, "limit")
	q := r.URL.Query()
	return MineOptions{Reader: readerOf(r), Target: q.Get("target"), Kind: q.Get("kind"), Value: q.Get("value"),
		CardGroups: list(q.Get("card_entities")), Cursor: q.Get("cursor"), Limit: limit}, err
}

func (h handler) favorites(w http.ResponseWriter, r *http.Request) {
	o, err := h.mineOptions(r)
	if err == nil && o.Value != "" {
		err = fmt.Errorf("%w: favorites take no value", ErrInvalid)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	page, err := h.s.Favorites(r.Context(), o)
	h.reply(w, r, page, err)
}

func (h handler) reactions(w http.ResponseWriter, r *http.Request) {
	o, err := h.mineOptions(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	page, err := h.s.Reactions(r.Context(), o)
	h.reply(w, r, page, err)
}
