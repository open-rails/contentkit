package contentstore

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	content "github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/kinds"
)

// favorites is the user-only bookmark (wishlist) over a content key: value 1
// favorited, 0 unfavorited (the row is kept so the change exports). A
// favorite requires the target be VISIBLE only, not accessible: premium
// content can be wishlisted before it is owned. No anonymous favorites.
type favorites struct {
	rt *Runtime
	s  *store
}

func newFavorites(rt *Runtime) *favorites {
	return &favorites{rt: rt, s: rt.store}
}

// add gates on visibility only, then favorites under the canonical
// reference. Re-favoriting is a no-op success. Locked content is favorited
// whatever its kind's locked view shows: a bookmark is the caller's own, and
// a wishlist holds what they don't own yet.
func (f *favorites) add(ctx context.Context, actor access.Actor, target contentref.ContentRef) (contentref.ContentRef, resolve.Resolution, error) {
	ref, res, err := f.rt.resolveTarget(ctx, target, actor, false)
	if err != nil {
		return ref, res, err
	}
	return ref, res, f.set(ctx, actor, ref, 1)
}

// remove unfavorites (idempotent), keeping the row at value 0. No visibility
// gate: unfavoriting content that later became hidden must still work.
func (f *favorites) remove(ctx context.Context, actor access.Actor, target contentref.ContentRef) (contentref.ContentRef, resolve.Resolution, error) {
	ref, res := f.rt.canonical(ctx, target, actor)
	return ref, res, f.set(ctx, actor, ref, 0)
}

func (f *favorites) set(ctx context.Context, actor access.Actor, ref contentref.ContentRef, value int16) error {
	if !actor.Member() {
		return errForbidden
	}
	tx, err := f.s.beginMutation(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := f.rt.guardErasedSubject(ctx, tx, viewerID(actor)); err != nil {
		return err
	}
	changed, err := f.setTx(ctx, tx, actor.AccountID(), ref.Key(), value, true)
	if err != nil {
		return err
	}
	if changed {
		if err := bumpCounts(ctx, tx, f.s, ref.Key(), 0, 0, int(2*value-1), 0); err != nil {
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
			ON CONFLICT (tenant_id, user_id, content_kind, content_id, content_version_id) WHERE user_id IS NOT NULL DO NOTHING`, args...)
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

func favoriteRoutes() []httpapi.Route[*favorites] {
	var out []httpapi.Route[*favorites]
	for _, typ := range TargetTypes {
		noun := targetNoun(typ)
		out = append(out,
			httpapi.Route[*favorites]{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/" + string(typ) + "/{id}/favorite", Resource: "favorites", Auth: httpapi.User,
				Doc:   "Favorites " + noun + " the caller sees; favoriting again is a no-op. Answers the state with its favorite count.",
				Query: targetQuery(typ), Responses: []httpapi.Reply{httpapi.OK(content.FavoriteState{})}, Errors: []string{content.CodeForbidden, content.CodeNotFound, content.CodeRateLimited}},
				Serve: func(f *favorites) http.HandlerFunc { return f.handleSet(typ, true) }},
			httpapi.Route[*favorites]{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/" + string(typ) + "/{id}/favorite", Resource: "favorites", Auth: httpapi.User,
				Doc:   "Unfavorites " + noun + ", also one no longer visible. Answers the state with its favorite count.",
				Query: targetQuery(typ), Responses: []httpapi.Reply{httpapi.OK(content.FavoriteState{})}, Errors: []string{content.CodeForbidden, content.CodeInvalidRequest, content.CodeNotFound, content.CodeRateLimited}},
				Serve: func(f *favorites) http.HandlerFunc { return f.handleSet(typ, false) }},
			httpapi.Route[*favorites]{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/" + string(typ) + "/{id}/favorite", Resource: "favorites", Auth: httpapi.Public,
				Doc:   "The favorite count of " + noun + lockedNote(typ, "it") + " and whether the caller favorited it (never, signed out: then it must be visible).",
				Query: targetQuery(typ), Responses: []httpapi.Reply{httpapi.OK(content.FavoriteState{})}, Errors: []string{content.CodeInvalidRequest, content.CodeNotFound}},
				Serve: func(f *favorites) http.HandlerFunc { return f.rt.targetHandler(typ, f.handleStatus) }})
	}
	return out
}

// state reads userID's bookmark of ref ("" for none) and, when count, ref's
// favorite count.
func (f *favorites) state(ctx context.Context, userID string, ref contentref.ContentRef, count bool) (content.FavoriteState, error) {
	args := append(keyArgs(ref.Key()), userID)
	var out content.FavoriteState
	var n int
	err := f.s.pool.QueryRow(ctx, `SELECT coalesce((SELECT favorites FROM `+f.s.t.counts+` WHERE `+keyPred(1)+`), 0),
		EXISTS (SELECT 1 FROM `+f.s.t.favorites+` WHERE `+keyPred(1)+` AND user_id = $5 AND value = 1)`, args...).Scan(&n, &out.Favorited)
	if count {
		out.Count = &n
	}
	return out, err
}

func (f *favorites) handleSet(typ content.TargetType, on bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		actor, err := f.rt.requireActor(req.Context())
		if err == nil {
			err = f.rt.limit(req.Context(), content.ActionFavorite, actor)
		}
		var ref contentref.ContentRef
		var res resolve.Resolution
		if err == nil {
			ref, err = f.rt.target(req, typ)
		}
		if err == nil && on {
			ref, res, err = f.add(req.Context(), actor, ref)
		} else if err == nil {
			ref, res, err = f.remove(req.Context(), actor, ref)
		}
		f.writeState(w, req, actor.AccountID(), ref, res, err)
	}
}

// writeState answers the state of ref as res resolved it (err its failure).
func (f *favorites) writeState(w http.ResponseWriter, req *http.Request, userID string, ref contentref.ContentRef, res resolve.Resolution, err error) {
	var st content.FavoriteState
	if err == nil {
		st, err = f.state(req.Context(), userID, ref, f.rt.shows(ref, res, kinds.ShowFavorites))
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleStatus answers the count and the caller's bookmark. A signed-in
// caller reads it also for a target no longer visible (to unfavorite it); a
// signed-out one only for a visible target.
func (f *favorites) handleStatus(w http.ResponseWriter, req *http.Request, target contentref.ContentRef) {
	ctx := req.Context()
	actor := f.rt.actor(ctx)
	var ref contentref.ContentRef
	var res resolve.Resolution
	var err error
	if userID := viewerID(actor); userID != "" {
		ref, res = f.rt.canonical(ctx, target, actor)
	} else {
		ref, res, err = f.rt.resolveTarget(ctx, target, actor, false)
	}
	f.writeState(w, req, viewerID(actor), ref, res, err)
}
