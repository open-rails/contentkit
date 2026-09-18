package content

import (
	"context"
	"net/http"
	"time"

	"github.com/open-rails/contentkit/contentref"
)

// favorites is the user-only bookmark (wishlist): an unsigned presence over a
// content key. A favorite requires the target be VISIBLE only, not accessible:
// premium content can be wishlisted before it is owned. No anonymous favorites.
type favorites struct {
	rt *Runtime
	s  *store
}

func newFavorites(rt *Runtime) *favorites {
	return &favorites{rt: rt, s: rt.store}
}

// FavoriteItem is one row of the caller's wishlist (newest-first on list).
type FavoriteItem struct {
	contentref.ContentRef
	CreatedAt time.Time `json:"created_at"`
}

// add gates on visibility only, then idempotently inserts under the canonical
// preference reference and records the snapshot in the same transaction.
// Re-favoriting is a no-op success that exports nothing.
func (f *favorites) add(ctx context.Context, actor Actor, kind, id string) (*PreferenceSnapshot, error) {
	ref, err := f.rt.gate(ctx, kind, id, actor, false)
	if err != nil {
		return nil, err
	}
	storage, key, exportable := f.rt.preferences.target(actor, ref, PreferenceAxisFavorite)
	tx, err := f.s.beginMutation(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := f.rt.guardPrivateSubject(ctx, tx, viewerID(actor)); err != nil {
		return nil, err
	}
	snap, err := f.rt.preferences.mutate(ctx, tx, key, exportable, 1, func() (bool, error) {
		tag, err := tx.Exec(ctx, `INSERT INTO `+f.s.t.favorites+` (`+keyCols+`, user_id) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, user_id, content_kind, content_id, content_version_id) DO NOTHING`,
			append(keyArgs(storage.Key()), actor.ID)...)
		if err != nil || tag.RowsAffected() != 1 {
			return false, err
		}
		return true, bumpCounts(ctx, tx, f.s, storage.Key(), 0, 0, 1, 0)
	})
	if err != nil {
		return nil, err
	}
	return snap, tx.Commit(ctx)
}

// remove deletes the caller's bookmark (idempotent) and keeps a zero-valued
// snapshot. No visibility gate: un-wishlisting content that later became
// hidden must still work.
func (f *favorites) remove(ctx context.Context, actor Actor, kind, id string) (*PreferenceSnapshot, error) {
	storage, key, exportable := f.rt.preferences.target(actor, f.rt.canonical(ctx, kind, id, actor), PreferenceAxisFavorite)
	tx, err := f.s.beginMutation(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := f.rt.guardPrivateSubject(ctx, tx, viewerID(actor)); err != nil {
		return nil, err
	}
	snap, err := f.rt.preferences.mutate(ctx, tx, key, exportable, 0, func() (bool, error) {
		tag, err := tx.Exec(ctx, `DELETE FROM `+f.s.t.favorites+` WHERE `+keyPred(1)+` AND user_id = $5`, append(keyArgs(storage.Key()), actor.ID)...)
		if err != nil || tag.RowsAffected() != 1 {
			return false, err
		}
		return true, bumpCounts(ctx, tx, f.s, storage.Key(), 0, 0, -1, 0)
	})
	if err != nil {
		return nil, err
	}
	return snap, tx.Commit(ctx)
}

// IsFavorited batch-reports which of refs the user has bookmarked, keyed by
// the caller's references; each is read under its canonical preference
// reference, the identity the write path stores under. Every requested key is
// present in the map (absent bookmarks => false).
func (f *favorites) IsFavorited(ctx context.Context, userID string, refs []contentref.ContentRef) (map[contentref.ContentKey]bool, error) {
	out := make(map[contentref.ContentKey]bool, len(refs))
	stored, err := f.rt.preferences.storedRefs(refs)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		out[r.Key()] = false
	}
	if userID == "" || len(refs) == 0 {
		return out, nil
	}
	kinds, ids, versions := refColumns(stored.refs)
	rows, err := f.s.pool.Query(ctx, `SELECT content_kind, content_id, content_version_id FROM `+f.s.t.favorites+`
		WHERE tenant_id = $1 AND user_id = $2 AND `+refsIn(3), f.s.tenant, userID, kinds, ids, versions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		k := contentref.ContentKey{TenantID: f.s.tenant}
		if err := rows.Scan(&k.ContentKind, &k.ContentID, &k.ContentVersionID); err != nil {
			return nil, err
		}
		for _, caller := range stored.callers[k] {
			out[caller] = true
		}
	}
	return out, rows.Err()
}

// list returns the caller's favorites, most recent first, paginated.
func (f *favorites) list(ctx context.Context, userID string, limit, offset int) ([]FavoriteItem, error) {
	rows, err := f.s.pool.Query(ctx, `SELECT content_kind, content_id, content_version_id, created_at
		FROM `+f.s.t.favorites+` WHERE tenant_id = $1 AND user_id = $2
		ORDER BY created_at DESC, content_kind, content_id, content_version_id
		LIMIT $3 OFFSET $4`, f.s.tenant, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]FavoriteItem, 0, min(limit, 128)) // limit can be MaxInt32 ("all")
	for rows.Next() {
		var it FavoriteItem
		var kind, id, version string
		if err := rows.Scan(&kind, &id, &version, &it.CreatedAt); err != nil {
			return nil, err
		}
		it.ContentRef = contentref.NewVersion(f.s.tenant, kind, id, version)
		items = append(items, it)
	}
	return items, rows.Err()
}

// --- HTTP ---

func (f *favorites) mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /favorites", f.handleList)
	mux.HandleFunc("POST /{kind}/{id}/favorite", f.handleAdd)
	mux.HandleFunc("DELETE /{kind}/{id}/favorite", f.handleRemove)
	mux.HandleFunc("GET /{kind}/{id}/favorite", f.handleStatus)
}

func (f *favorites) handleAdd(w http.ResponseWriter, req *http.Request) {
	actor, err := f.rt.requireActor(req.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, err := f.add(req.Context(), actor, req.PathValue("kind"), req.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"favorited": true})
}

func (f *favorites) handleRemove(w http.ResponseWriter, req *http.Request) {
	actor, err := f.rt.requireActor(req.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, err := f.remove(req.Context(), actor, req.PathValue("kind"), req.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"favorited": false})
}

func (f *favorites) handleStatus(w http.ResponseWriter, req *http.Request) {
	actor, err := f.rt.requireActor(req.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	ref := f.rt.canonical(req.Context(), req.PathValue("kind"), req.PathValue("id"), actor)
	m, err := f.IsFavorited(req.Context(), actor.ID, []contentref.ContentRef{ref}) // read under the stored reference
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"favorited": m[ref.Key()]})
}

func (f *favorites) handleList(w http.ResponseWriter, req *http.Request) {
	actor, err := f.rt.requireActor(req.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	limit, offset := parsePage(req)
	items, err := f.list(req.Context(), actor.ID, limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(items))
}
