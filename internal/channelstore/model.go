package channelstore

import (
	channels "github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/entities"
)

func card(e entities.EntityCard) channels.Card {
	return channels.Card{ID: string(e.ID), Slug: e.Slug, Name: e.Name, NameLanguage: e.NameLanguage, Code: e.Code, Path: e.Path, Cover: e.Cover, Avatar: e.Avatar, Count: e.Count, Access: e.Access, CreatedAt: e.CreatedAt}
}
