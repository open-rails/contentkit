package discoverystore

import (
	"fmt"
	"net/http"
	"strconv"

	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/gate"
)

func (h handler) library(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := discovery.LibraryQuery{Language: q.Get("language"), Cursor: q.Get("cursor")}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 {
			h.fail(w, r, fmt.Errorf("%w: limit", ErrInvalid))
			return
		}
		opts.Limit = n
	}
	out, err := h.s.Library(r.Context(), gate.Actor(r.Context()), opts)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.reply(w, r, out, nil)
}
