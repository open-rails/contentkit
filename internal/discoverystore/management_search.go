package discoverystore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/entities"
	catalogstore "github.com/open-rails/contentkit/internal/catalogstore"
	"github.com/open-rails/contentkit/internal/search"
	"github.com/open-rails/contentkit/media"
)

// ManagementSuggestions are lean, private editor targets. Public domain,
// optional-reader, publication and billing rules never substitute for editor
// authorization; current draft/held names remain separate from public search.
func (s *Service) ManagementSuggestions(ctx context.Context, o discovery.ManagementSuggestionsOptions) (discovery.Typeahead, error) {
	out := discovery.Typeahead{Contents: []discovery.Card{}, Entities: []entities.EntityCard{}}
	if o.Actor.Anonymous || o.Actor.ID == "" {
		return out, auth.ErrUnauthenticated
	}
	manager, ok := s.o.Visibility.(discovery.ManagementCatalog)
	if !ok {
		return out, ErrNotFound
	}
	language, err := s.displayLanguage(o.DisplayLanguage)
	if err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	names, err := manager.ManagementSearch(ctx, o.Actor, catalog.ManagementSearchQuery{Query: o.Query, Language: language,
		ContentKinds: o.ContentKinds, EntityKinds: o.EntityKinds, IncludeContents: o.IncludeContents, IncludeEntities: o.IncludeEntities, Limit: o.Limit})
	if err != nil {
		var authErr *catalog.AuthError
		switch {
		case errors.Is(err, catalogstore.ErrForbidden):
			return out, errors.Join(auth.ErrForbidden, err)
		case errors.Is(err, catalogstore.ErrInvalid):
			return out, fmt.Errorf("%w: %v", ErrInvalid, err)
		case errors.Is(err, search.ErrInvalidQuery):
			return out, fmt.Errorf("%w: %v", ErrInvalid, err)
		case errors.As(err, &authErr):
			return out, errors.Join(auth.ErrUnavailable, err)
		default:
			return out, err
		}
	}
	for _, name := range names.Contents {
		out.Contents = append(out.Contents, discovery.Card{ID: name.ID, Kind: name.Kind, Title: name.Title, Language: name.Language,
			Cover: []media.PublicRendition{}, Version: discovery.CardVersion{Languages: []string{}, Traits: []string{}}, Entities: map[string][]discovery.Chip{},
			Access: discovery.CardAccess{Allowed: true, Level: access.Public}, Attributes: map[string]any{}})
	}
	out.Entities = names.Entities
	return out, nil
}
