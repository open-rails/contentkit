package catalog

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contenturl"
)

// Routes are the URL routes of the kinds config: each content kind's and
// group kind's, for contenturl.RouterOptions.Routes.
func (s *Store) Routes() contenturl.Routes {
	out := contenturl.Routes{}
	for name, k := range s.cfg.Kinds {
		out[name] = k.Route
	}
	for name, g := range s.cfg.GroupKinds {
		if g.Route != "" {
			out[name] = g.Route
		}
	}
	return out
}

// Visibility is contenturl.RouterOptions.Visibility for content kinds: a
// page shows while its item is live for an anonymous visitor of the
// request's site, is Gone once a live item was deleted (or erased), and is
// Hidden otherwise. Other kinds are Visible.
func (s *Store) Visibility(r *http.Request, l contenturl.Link) (contenturl.Visibility, error) {
	if _, ok := s.cfg.Kinds[l.ContentKind]; !ok || l.TenantID != s.tenant {
		return contenturl.Visible, nil
	}
	var state string
	var wasLive bool
	err := s.pool.QueryRow(r.Context(), `SELECT state, published_at IS NOT NULL FROM `+s.t.items+` WHERE tenant_id = $1 AND id = $2 AND kind = $3`,
		s.tenant, l.ContentID, l.ContentKind).Scan(&state, &wasLive)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return contenturl.Gone, nil
	case err != nil:
		return contenturl.Hidden, err
	case state == StateDeleted && wasLive:
		return contenturl.Gone, nil
	}
	v, err := s.Viewer(r.Context(), ViewerOptions{Actor: access.Actor{Anonymous: true}, Host: r.Host})
	if err != nil {
		return contenturl.Hidden, err
	}
	got, err := s.Contents(r.Context(), v, []string{l.ContentID})
	if err != nil || got[0] == nil || !got[0].Live {
		return contenturl.Hidden, err
	}
	return contenturl.Visible, nil
}
