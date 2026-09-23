package content

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
)

// favorites is the user-only bookmark (wishlist) over a content key: value 1
// favorited, 0 unfavorited (the row is kept so the change exports). A favorite requires the target be VISIBLE only, not accessible:
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

// add gates on visibility only, then favorites under the canonical preference
// reference. Re-favoriting is a no-op success.
func (f *favorites) add(ctx context.Context, actor Actor, kind, id string) error {
	ref, err := f.rt.gate(ctx, kind, id, actor, false)
	if err != nil {
		return err
	}
	return f.set(ctx, actor, ref, 1)
}

// remove unfavorites (idempotent), keeping the row at value 0. No visibility
// gate: un-wishlisting content that later became hidden must still work.
func (f *favorites) remove(ctx context.Context, actor Actor, kind, id string) error {
	return f.set(ctx, actor, f.rt.canonical(ctx, kind, id, actor), 0)
}

func (f *favorites) set(ctx context.Context, actor Actor, ref contentref.ContentRef, value int16) error {
	storage, _ := f.rt.preferences.work(ref)
	tx, err := f.s.beginMutation(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := f.rt.guardErasedSubject(ctx, tx, viewerID(actor)); err != nil {
		return err
	}
	changed, err := f.setTx(ctx, tx, actor.ID, storage.Key(), value, true)
	if err != nil {
		return err
	}
	if changed {
		if err := bumpCounts(ctx, tx, f.s, storage.Key(), 0, 0, int(2*value-1), 0); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// setTx moves the caller's row to value under its row lock, taking a fresh
// revision on change; a lost insert race re-locks the winner's row once.
func (f *favorites) setTx(ctx context.Context, tx pgx.Tx, userID string, key contentref.ContentKey, value int16, retry bool) (bool, error) {
	args := append(keyArgs(key), userID)
	var prev int16
	err := tx.QueryRow(ctx, `SELECT value FROM `+f.s.t.favorites+` WHERE `+keyPred(1)+` AND user_id = $5 FOR UPDATE`, args...).Scan(&prev)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if value == 0 {
			return false, nil
		}
		tag, err := tx.Exec(ctx, `INSERT INTO `+f.s.t.favorites+` (`+keyCols+`, user_id, value, revision) VALUES ($1, $2, $3, $4, $5, 1, `+f.s.nextRevision()+`)
			ON CONFLICT (tenant_id, user_id, content_kind, content_id, content_version_id) DO NOTHING`, args...)
		if err != nil || tag.RowsAffected() == 1 || !retry {
			return err == nil && tag.RowsAffected() == 1, err
		}
		return f.setTx(ctx, tx, userID, key, value, false)
	case err != nil:
		return false, err
	case prev == value:
		return false, nil
	}
	// Re-favoriting restarts the bookmark's age, which orders the wishlist.
	_, err = tx.Exec(ctx, `UPDATE `+f.s.t.favorites+` SET value = $6::smallint, revision = `+f.s.nextRevision()+`, updated_at = now(),
		created_at = CASE WHEN $6::smallint = 1 THEN now() ELSE created_at END WHERE `+keyPred(1)+` AND user_id = $5`, append(args, value)...)
	return err == nil, err
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
		WHERE tenant_id = $1 AND user_id = $2 AND value = 1 AND `+refsIn(3), f.s.tenant, userID, kinds, ids, versions)
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
		FROM `+f.s.t.favorites+` WHERE tenant_id = $1 AND user_id = $2 AND value = 1
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
	if err := f.add(req.Context(), actor, req.PathValue("kind"), req.PathValue("id")); err != nil {
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
	if err := f.remove(req.Context(), actor, req.PathValue("kind"), req.PathValue("id")); err != nil {
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
