package discovery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/search"
)

// ManagementCatalog keeps editor authorization with the catalog. The standard
// *catalog.Store implements it; a custom Visibility may forward this capability.
type ManagementCatalog interface {
	ManagementSearch(context.Context, access.Actor, catalog.ManagementSearchQuery) (catalog.ManagementNames, error)
}

type ManagementSuggestionsOptions struct {
	Actor                            access.Actor
	Query, DisplayLanguage           string
	ContentKinds, EntityKinds        []string
	IncludeContents, IncludeEntities bool
	Limit                            int
}

// ManagementSuggestions are lean, private editor targets. Public domain,
// optional-reader, publication and billing rules never substitute for editor
// authorization; current draft/held names remain separate from public search.
func (s *Service) ManagementSuggestions(ctx context.Context, o ManagementSuggestionsOptions) (Typeahead, error) {
	out := Typeahead{Contents: []Card{}, Groups: []groups.GroupCard{}}
	if o.Actor.Anonymous || o.Actor.ID == "" {
		return out, auth.ErrUnauthenticated
	}
	manager, ok := s.o.Visibility.(ManagementCatalog)
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
		case errors.Is(err, catalog.ErrForbidden):
			return out, errors.Join(auth.ErrForbidden, err)
		case errors.Is(err, catalog.ErrInvalid):
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
		out.Contents = append(out.Contents, Card{ID: name.ID, Kind: name.Kind, Title: name.Title, Language: name.Language,
			Cover: []media.PublicRendition{}, Version: CardVersion{Languages: []string{}, Traits: []string{}}, Groups: map[string][]Chip{},
			Access: CardAccess{Allowed: true, Level: access.Public}, Attributes: map[string]any{}})
	}
	out.Groups = names.Entities
	return out, nil
}
