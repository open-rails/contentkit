package discovery

import (
	"context"
	"errors"
	"net/http"

	"github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/entities"
)

// ChannelFeed returns originals and repost references through the usual catalog
// reader policy. Reposts preserve the original IDs, versions and interactions.
func (s *Service) ChannelFeed(ctx context.Context, id string, r Reader, cursor string, limit int) (ContentPage, error) {
	empty := ContentPage{Contents: []Card{}}
	if s.o.Channels == nil || contentref.ValidateID(id) != nil {
		return empty, ErrNotFound
	}
	channel, err := s.o.Channels.Raw(ctx, id)
	if errors.Is(err, entities.ErrNotFound) {
		return empty, ErrNotFound
	}
	if err != nil {
		return empty, err
	}
	if channel.State != entities.StateActive || channel.AuthScope == "" {
		return empty, ErrNotFound
	}
	return s.List(ctx, ListOptions{Reader: r, Filter: Filter{ChannelIDs: []string{id}, includeReposts: true}, Sort: SortNewest, Cursor: cursor, Limit: limit})
}

func (h handler) channelFeed(w http.ResponseWriter, r *http.Request) {
	limit, err := intParam(r, "limit")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.s.ChannelFeed(r.Context(), r.PathValue("id"), readerOf(r), r.URL.Query().Get("cursor"), limit)
	h.reply(w, r, out, err)
}

func (s *Service) repostContext(ctx context.Context, q *sel, channel string, cards []Card) error {
	ids := make([]string, 0, len(cards))
	for _, card := range cards {
		if card.Channel == nil || card.Channel.ID != channel {
			ids = append(ids, card.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.o.Pool.Query(ctx, "SELECT content_id,coalesce(actor_id,''),created_at FROM "+s.t.reposts+" WHERE tenant_id=$1 AND channel_id=$2 AND content_id=ANY($3)", s.o.Tenant, channel, ids)
	if err != nil {
		return err
	}
	refs := map[string]channels.Repost{}
	for rows.Next() {
		var id string
		rp := channels.Repost{ChannelID: channel}
		if err := rows.Scan(&id, &rp.ActorID, &rp.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		refs[id] = rp
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	writer := q.viewer.Editors(ctx, []string{channel})[channel]
	canPost, err := q.viewer.CanPostToChannel(ctx, channel)
	if err != nil {
		return err
	}
	for i := range cards {
		if rp, ok := refs[cards[i].ID]; ok {
			rp.CanRemove = writer || canPost && rp.ActorID != "" && rp.ActorID == q.reader.Actor.AccountID()
			cards[i].Repost = &rp
		}
	}
	return nil
}
