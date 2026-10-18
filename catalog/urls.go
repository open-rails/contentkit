package catalog

import (
	"net/http"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/gate"
)

// Visibility is contenturl.RouterOptions.Visibility for content kinds: a
// page shows while its item is live for an anonymous visitor of the
// request's site, is Gone once a live item was deleted (or erased), is a
// Preview for a signed-in editor of an item not live yet (staff, creators),
// and is Hidden otherwise. Other kinds are Visible.
func (s *Store) Visibility(r *http.Request, links []contenturl.Link) ([]contenturl.Visibility, error) {
	out := make([]contenturl.Visibility, len(links))
	at := map[string][]int{}
	var ids, kinds []string
	for i, l := range links {
		out[i] = contenturl.Visible
		if _, ok := s.cfg.Kinds[l.ContentKind]; !ok || l.TenantID != s.tenant {
			continue
		}
		out[i] = contenturl.Gone
		if _, seen := at[l.ContentID]; !seen {
			ids, kinds = append(ids, l.ContentID), append(kinds, l.ContentKind)
		}
		at[l.ContentID] = append(at[l.ContentID], i)
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(r.Context(), `SELECT c.id, c.state = 'deleted' AND c.published_at IS NOT NULL FROM unnest($2::text[], $3::text[]) AS l(id, kind)
 JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = l.id AND c.kind = l.kind`, s.tenant, ids, kinds)
	if err != nil {
		return nil, err
	}
	var candidates []string
	for rows.Next() {
		var id string
		var removed bool
		if err := rows.Scan(&id, &removed); err != nil {
			rows.Close()
			return nil, err
		}
		if !removed {
			candidates = append(candidates, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	set := func(id string, v contenturl.Visibility) {
		for _, i := range at[id] {
			out[i] = v
		}
	}
	for _, id := range candidates {
		set(id, contenturl.Hidden)
	}
	if len(candidates) == 0 {
		return out, nil
	}
	v, err := s.Viewer(r.Context(), ViewerOptions{Actor: access.Actor{Anonymous: true}, Host: r.Host})
	if err != nil {
		return nil, err
	}
	// Only liveness and editorship matter here, so the rule's verdict is read
	// directly rather than through the full Contents graph.
	views, err := v.decide(r.Context(), candidates, true)
	if err != nil {
		return nil, err
	}
	var hidden []string
	for _, id := range candidates {
		if iv := views[id]; iv != nil && v.itemLive(iv) {
			set(id, contenturl.Visible)
		} else {
			hidden = append(hidden, id)
		}
	}
	actor := gate.Actor(r.Context())
	if len(hidden) == 0 || actor.Anonymous {
		return out, nil
	}
	ev, err := s.Viewer(r.Context(), ViewerOptions{Actor: actor, Host: r.Host})
	if err != nil {
		return nil, err
	}
	mine, err := ev.decide(r.Context(), hidden, true)
	if err != nil {
		return nil, err
	}
	for _, id := range hidden {
		if iv := mine[id]; iv != nil && iv.visible && iv.editor {
			set(id, contenturl.Preview)
		}
	}
	return out, nil
}
