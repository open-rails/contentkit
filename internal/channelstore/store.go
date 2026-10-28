package channelstore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	channels "github.com/open-rails/contentkit/channels"
	"github.com/open-rails/contentkit/entities"
	entitystore "github.com/open-rails/contentkit/internal/entitystore"
	"github.com/open-rails/contentkit/kinds"
)

// Store is a catalog's channels. Existing persisted identity, media and scope
// bytes are retained; callers only see this typed channel interface.
type Store struct {
	trusted  bool
	actor    access.Actor
	storage  *entitystore.Store
	metadata *entitystore.Store
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
	e, err := entitystore.New(entitystore.Options{Pool: opts.Pool, Schema: opts.Schema, Tenant: opts.Tenant, Kinds: opts.Model, Scopes: opts.Scopes})
	if err != nil {
		return nil, err
	}
	return FromStore(e), nil
}

// FromStore binds channel storage to the engine's existing catalog dependencies.
func FromStore(storage *entitystore.Store) *Store {
	return &Store{storage: storage.ChannelStorage(), metadata: storage, trusted: true}
}

func (s *Store) As(a auth.Authenticator, actor access.Actor) *Store {
	return &Store{storage: s.storage.As(a, actor), metadata: s.metadata.As(a, actor), actor: actor}
}

type ListOptions struct {
	Language string
	Slug     string
	Query    string
	Cursor   string
	Limit    int
	Sort     entities.Sort
	State    string
}

func options(in ListOptions) entitystore.ListOptions {
	return entitystore.ListOptions{Kind: "channel", Language: in.Language, Slug: in.Slug, Query: in.Query, Cursor: in.Cursor, Limit: in.Limit, Sort: in.Sort}
}

func (s *Store) ManagementList(ctx context.Context, in ListOptions) (channels.Page, error) {
	o := options(in)
	o.IncludeInternal = true
	switch in.State {
	case "", "active":
	case "deleted":
		o.Deleted = true
	default:
		return channels.Page{}, fmt.Errorf("%w: channel state must be active or deleted", entities.ErrInvalid)
	}
	return s.list(ctx, o)
}

func (s *Store) List(ctx context.Context, in ListOptions) (channels.Page, error) {
	return s.list(ctx, options(in))
}

func (s *Store) list(ctx context.Context, in entitystore.ListOptions) (channels.Page, error) {
	p, err := s.storage.List(ctx, in)
	out := channels.Page{Channels: []channels.Card{}, NextCursor: p.NextCursor, Total: p.Total}
	for _, e := range p.Entities {
		out.Channels = append(out.Channels, card(e))
	}
	return out, err
}

func (s *Store) Mine(ctx context.Context, in ListOptions) (channels.MyPage, error) {
	p, err := s.storage.Mine(ctx, options(in))
	out := channels.MyPage{Channels: []channels.Mine{}, NextCursor: p.NextCursor}
	for _, e := range p.Entities {
		out.Channels = append(out.Channels, channels.Mine{Card: card(e.EntityCard), Owner: e.Owner, Editor: e.Editor, CanPost: e.CanPost, CanEditOwn: e.CanEditOwn, Writer: e.Writer, Moderator: e.Moderator, Member: e.Member})
	}
	return out, err
}

func (s *Store) Cards(ctx context.Context, ids []string, language string) ([]channels.Card, error) {
	raw := make([]entities.EntityID, len(ids))
	for i, id := range ids {
		raw[i] = entities.EntityID(id)
	}
	cs, err := s.storage.Cards(ctx, raw, entitystore.CardOptions{Language: language, Covers: true})
	out := []channels.Card{}
	for _, e := range cs {
		out = append(out, card(e))
	}
	return out, err
}

func (s *Store) Get(ctx context.Context, id, language string) (channels.Channel, error) {
	return s.get(ctx, id, language, false)
}

func (s *Store) ManagementGet(ctx context.Context, id, language string) (channels.Channel, error) {
	return s.get(ctx, id, language, true)
}

func (s *Store) get(ctx context.Context, id, language string, management bool) (channels.Channel, error) {
	var e entities.EntityDetail
	var err error
	if management {
		e, err = s.storage.ManagementDetail(ctx, entities.EntityID(id), language)
	} else {
		e, err = s.storage.Detail(ctx, entities.EntityID(id), language)
	}
	if err != nil {
		return channels.Channel{}, err
	}
	out := channels.Channel{Card: channels.Card{ID: string(e.ID), Slug: e.Slug, Name: e.Name, NameLanguage: e.NameLanguage, Code: e.Code, Path: e.Path, Cover: e.Cover, Avatar: e.Avatar, Access: e.Access, CreatedAt: e.CreatedAt}, State: e.State, OwnerUserID: e.OwnerUserID, AuthScope: e.AuthScope, Revision: e.Revision, UpdatedAt: e.UpdatedAt, Names: e.Names, Text: e.Text, Counts: e.Counts, Entities: []entities.EntityCard{}}
	for _, count := range e.Counts {
		if count.Language == language {
			out.Count += count.Count
		}
	}
	out.Entities, err = s.Metadata(ctx, id, language)
	return out, err
}

func (s *Store) Create(ctx context.Context, in channels.Input) (channels.Channel, error) {
	out, err := s.storage.CreateEntities(ctx, []entities.EntityInput{{Kind: "channel", Slug: in.Slug, Names: in.Names, Text: in.Text, Cover: in.Cover, Avatar: in.Avatar}})
	if errors.Is(err, entitystore.ErrConflict) || errors.Is(err, entitystore.ErrLimit) {
		page, lookupErr := s.storage.List(ctx, entitystore.ListOptions{Kind: "channel", Slug: strings.TrimSpace(in.Slug), Limit: 2})
		if lookupErr != nil {
			return channels.Channel{}, lookupErr
		}
		if len(page.Entities) == 1 {
			current, lookupErr := s.storage.Detail(ctx, page.Entities[0].ID, "")
			if lookupErr != nil {
				return channels.Channel{}, lookupErr
			}
			staff, _ := s.storage.PrepareStaffWrite(ctx)
			if (s.trusted || staff || current.OwnerUserID != "" && current.OwnerUserID == s.actor.AccountID()) && sameInput(current, in) {
				if err := s.Finish(ctx, []string{string(current.ID)}); err != nil {
					return channels.Channel{}, err
				}
				return s.Get(ctx, string(current.ID), "")
			}
		}
	}
	if err != nil {
		return channels.Channel{}, err
	}
	return s.Get(ctx, string(out[0].ID), "")
}

func (s *Store) Update(ctx context.Context, id string, in channels.Update) (channels.Channel, error) {
	_, err := s.storage.UpdateEntities(ctx, []entities.EntityUpdate{{ID: entities.EntityID(id), Slug: in.Slug, Names: in.Names, Text: in.Text, Cover: in.Cover, Avatar: in.Avatar, State: in.State, OwnerUserID: in.OwnerUserID}})
	if err != nil {
		return channels.Channel{}, err
	}
	if in.State != nil && *in.State == entities.StateDeleted {
		return channels.Channel{Card: channels.Card{ID: id}, State: entities.StateDeleted}, nil
	}
	return s.Get(ctx, id, "")
}

// Delete withdraws the channel and completes its group/content lifecycle
// synchronously. Retained media remains owned by the existing purge sweep.
func (s *Store) Delete(ctx context.Context, id string) error {
	current, err := s.Raw(ctx, id)
	if err != nil {
		return err
	}
	// The committed tombstone already records the authorized deletion. Its
	// original account may finish a lost acknowledgement without acquiring the
	// retired group's powers or being allowed to restore/edit the channel.
	if current.State == entities.StateDeleted && s.actor.Member() && current.OwnerUserID == s.actor.AccountID() {
		if err := s.Finish(ctx, []string{id}); err != nil {
			return err
		}
		now, err := s.Raw(ctx, id)
		if err != nil {
			return err
		}
		if now.State != entities.StateDeleted {
			return entitystore.ErrConflict
		}
		return nil
	}
	_, err = s.storage.DeleteEntities(ctx, []entities.EntityID{entities.EntityID(id)})
	return err
}

func (s *Store) Metadata(ctx context.Context, id, language string) ([]entities.EntityCard, error) {
	ids, err := s.storage.ChannelMetadata(ctx, entities.EntityID(id))
	if err != nil {
		return nil, err
	}
	return s.metadata.Cards(ctx, ids, entitystore.CardOptions{Language: language, Covers: true})
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
		return entities.Entity{}, fmt.Errorf("%w: channel", entitystore.ErrNotFound)
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

// Finish completes synchronous cross-store lifecycle operations after a
// caller-owned import transaction commits. Retrying the same IDs is safe.
func (s *Store) Finish(ctx context.Context, ids []string) error {
	raw := make([]entities.EntityID, len(ids))
	for i, id := range ids {
		raw[i] = entities.EntityID(id)
	}
	return s.storage.FinishChannels(ctx, raw)
}

func sameInput(current entities.EntityDetail, in channels.Input) bool {
	if !current.MatchesIdentity(in.Names, in.Text) {
		return false
	}
	if in.Cover != nil && (current.Cover == nil || in.Cover.Image != current.Cover.Image || in.Cover.ContentID != current.Cover.ContentID || in.Cover.Auto != current.Cover.Auto) {
		return false
	}
	if in.Avatar != nil && (current.Avatar == nil || in.Avatar.Image != current.Avatar.Image) {
		return false
	}
	return true
}
