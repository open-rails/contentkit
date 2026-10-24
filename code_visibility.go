package contentkit

import (
	"net/http"

	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/gate"
)

// codeVisibility composes the catalog's content policy with the entity
// store's ordinary public reads. Management-only entities never acquire a
// public identity or slug through the shared code resolver.
func (e *Engine) codeVisibility(req *http.Request, links []contenturl.Link) ([]contenturl.Visibility, error) {
	visible, err := e.catalog.Visibility(req, links)
	if err != nil {
		return nil, err
	}
	indices := map[entities.EntityID][]int{}
	var ids []entities.EntityID
	for i, link := range links {
		if _, content := e.cfg.Content.Kinds[link.ContentKind]; content {
			continue
		}
		if _, entity := e.cfg.Content.EntityKinds[link.ContentKind]; !entity {
			continue
		}
		visible[i] = contenturl.Hidden
		if link.TenantID != e.cfg.Tenant {
			continue
		}
		id := entities.EntityID(link.ContentID)
		if _, seen := indices[id]; !seen {
			ids = append(ids, id)
		}
		indices[id] = append(indices[id], i)
	}
	if len(ids) == 0 {
		return visible, nil
	}
	cards, err := e.entities.As(e.auth, gate.Actor(req.Context())).Cards(req.Context(), ids, entities.CardOptions{})
	if err != nil {
		return nil, err
	}
	for _, card := range cards {
		for _, i := range indices[card.ID] {
			if links[i].ContentKind == card.Kind {
				visible[i] = contenturl.Visible
			}
		}
	}
	return visible, nil
}
