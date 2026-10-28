package channelstore

import (
	"context"

	"github.com/jackc/pgx/v5"
	channels "github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/entities"
)

// ImportTx stores identities in the import transaction. Call Finish after
// commit to apply the corresponding AuthKit operation synchronously.
func (s *Store) ImportTx(ctx context.Context, tx pgx.Tx, in []channels.Import, provision bool) error {
	rows := make([]entities.ImportEntity, len(in))
	for i, v := range in {
		rows[i] = entities.ImportEntity{ID: entities.EntityID(v.ID), Kind: "channel", Slug: v.Slug, State: v.State, Cover: v.Cover, Avatar: v.Avatar, OwnerUserID: v.OwnerUserID, AuthScope: v.AuthScope, Names: v.Names, Text: v.Text, Code: v.Code, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt}
	}
	return s.storage.ImportEntities(ctx, tx, rows, provision)
}

func (s *Store) ImportDeletesTx(ctx context.Context, tx pgx.Tx, ids []string) error {
	raw := make([]entities.EntityID, len(ids))
	for i, id := range ids {
		raw[i] = entities.EntityID(id)
	}
	return s.storage.ImportDeletes(ctx, tx, raw)
}
