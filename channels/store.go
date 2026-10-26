package channels

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
)

// Store is a catalog's channels. Existing persisted identity, media and scope
// bytes are retained; callers only see this typed channel interface.
type Store struct {
	storage  *entities.Store
	metadata *entities.Store
}

type Options struct {
	Pool   *pgxpool.Pool
	Schema string
	Tenant string
	Model  kinds.Config
	Scopes entities.Scopes
}

// New opens a standalone channel store, for example for a staff role CLI.
// It does not migrate, seed channels or construct another authentication server.
func New(opts Options) (*Store, error) {
	e, err := entities.New(entities.Options{Pool: opts.Pool, Schema: opts.Schema, Tenant: opts.Tenant, Kinds: opts.Model, Scopes: opts.Scopes})
	if err != nil {
		return nil, err
	}
	return FromStore(e), nil
}

// FromStore binds channel storage to the engine's existing catalog dependencies.
func FromStore(storage *entities.Store) *Store {
	return &Store{storage: storage.ChannelStorage(), metadata: storage}
}

func (s *Store) As(a auth.Authenticator, actor access.Actor) *Store {
	return &Store{storage: s.storage.As(a, actor), metadata: s.metadata.As(a, actor)}
}

type ListOptions struct {
	Language string
	Slug     string
	Query    string
	Cursor   string
	Limit    int
	Sort     entities.Sort
}

func options(in ListOptions) entities.ListOptions {
	return entities.ListOptions{Kind: "channel", Language: in.Language, Slug: in.Slug, Query: in.Query, Cursor: in.Cursor, Limit: in.Limit, Sort: in.Sort}
}

func (s *Store) List(ctx context.Context, in ListOptions) (Page, error) {
	p, err := s.storage.List(ctx, options(in))
	out := Page{Channels: []Card{}, NextCursor: p.NextCursor, Total: p.Total}
	for _, e := range p.Entities {
		out.Channels = append(out.Channels, card(e))
	}
	return out, err
}

func (s *Store) Mine(ctx context.Context, in ListOptions) (MyPage, error) {
	p, err := s.storage.Mine(ctx, options(in))
	out := MyPage{Channels: []Mine{}, NextCursor: p.NextCursor}
	for _, e := range p.Entities {
		out.Channels = append(out.Channels, Mine{Card: card(e.EntityCard), Owner: e.Owner, Editor: e.Editor, CanPost: e.CanPost, CanEditOwn: e.CanEditOwn, Writer: e.Writer, Moderator: e.Moderator, Member: e.Member})
	}
	return out, err
}

func (s *Store) Cards(ctx context.Context, ids []string, language string) ([]Card, error) {
	raw := make([]entities.EntityID, len(ids))
	for i, id := range ids {
		raw[i] = entities.EntityID(id)
	}
	cs, err := s.storage.Cards(ctx, raw, entities.CardOptions{Language: language, Covers: true})
	out := []Card{}
	for _, e := range cs {
		out = append(out, card(e))
	}
	return out, err
}

func (s *Store) Get(ctx context.Context, id, language string) (Channel, error) {
	return s.get(ctx, id, language, false)
}
func (s *Store) ManagementGet(ctx context.Context, id, language string) (Channel, error) {
	return s.get(ctx, id, language, true)
}
func (s *Store) get(ctx context.Context, id, language string, management bool) (Channel, error) {
	var e entities.EntityDetail
	var err error
	if management {
		e, err = s.storage.ManagementDetail(ctx, entities.EntityID(id), language)
	} else {
		e, err = s.storage.Detail(ctx, entities.EntityID(id), language)
	}
	if err != nil {
		return Channel{}, err
	}
	out := Channel{Card: Card{ID: string(e.ID), Slug: e.Slug, Name: e.Name, NameLanguage: e.NameLanguage, Code: e.Code, Path: e.Path, Cover: e.Cover, Avatar: e.Avatar, Access: e.Access, CreatedAt: e.CreatedAt}, State: e.State, OwnerUserID: e.OwnerUserID, AuthScope: e.AuthScope, Revision: e.Revision, UpdatedAt: e.UpdatedAt, Names: e.Names, Text: e.Text, Counts: e.Counts, Entities: []entities.EntityCard{}}
	for _, count := range e.Counts {
		out.Count += count.Count
	}
	out.Entities, err = s.Metadata(ctx, id, language)
	return out, err
}

func (s *Store) Create(ctx context.Context, in Input) (Channel, error) {
	out, err := s.storage.CreateEntities(ctx, []entities.EntityInput{{Kind: "channel", Slug: in.Slug, Names: in.Names, Text: in.Text, Cover: in.Cover, Avatar: in.Avatar}})
	if err != nil {
		return Channel{}, err
	}
	return s.Get(ctx, string(out[0].ID), "")
}

func (s *Store) Update(ctx context.Context, id string, in Update) (Channel, error) {
	_, err := s.storage.UpdateEntities(ctx, []entities.EntityUpdate{{ID: entities.EntityID(id), Slug: in.Slug, Names: in.Names, Text: in.Text, Cover: in.Cover, Avatar: in.Avatar, State: in.State, OwnerUserID: in.OwnerUserID}})
	if err != nil {
		return Channel{}, err
	}
	if in.State != nil && *in.State == entities.StateDeleted {
		return Channel{Card: Card{ID: id}, State: entities.StateDeleted}, nil
	}
	return s.Get(ctx, id, "")
}

// Delete only withdraws the channel. Existing durable lifecycle processing
// applies its content-retention policy; no new cleanup worker is introduced.
func (s *Store) Delete(ctx context.Context, id string) error {
	_, err := s.storage.DeleteEntities(ctx, []entities.EntityID{entities.EntityID(id)})
	return err
}

func (s *Store) Metadata(ctx context.Context, id, language string) ([]entities.EntityCard, error) {
	ids, err := s.storage.ChannelMetadata(ctx, entities.EntityID(id))
	if err != nil {
		return nil, err
	}
	return s.metadata.Cards(ctx, ids, entities.CardOptions{Language: language, Covers: true})
}

func (s *Store) SetMetadata(ctx context.Context, id string, ids []string) error {
	raw := make([]entities.EntityID, len(ids))
	for i, id := range ids {
		raw[i] = entities.EntityID(id)
	}
	return s.storage.SetChannelMetadata(ctx, entities.EntityID(id), raw)
}

// Raw returns channel identity for the engine's permission/media adapters.
// It is not exposed by the HTTP contract.
func (s *Store) Raw(ctx context.Context, id string) (entities.Entity, error) {
	es, err := s.storage.Entities(ctx, []entities.EntityID{entities.EntityID(id)})
	if err != nil {
		return entities.Entity{}, err
	}
	if len(es) != 1 {
		return entities.Entity{}, fmt.Errorf("%w: channel", entities.ErrNotFound)
	}
	return es[0], nil
}

func (s *Store) RawMany(ctx context.Context, ids []string) ([]entities.Entity, error) {
	raw := make([]entities.EntityID, len(ids))
	for i, id := range ids {
		raw[i] = entities.EntityID(id)
	}
	return s.storage.Entities(ctx, raw)
}
