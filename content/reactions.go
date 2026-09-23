package content

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
)

// reactions is the 3-state (like/dislike/neutral) reaction system over a
// content key: the reference module every other engagement type mirrors.
//
// applyTx is the shared concurrency-safe upsert primitive: comments and posts
// reuse it inside their own transactions to write a reaction and bump their own
// split counter atomically.
type reactions struct {
	rt *Runtime
	s  *store
}

func newReactions(rt *Runtime) *reactions {
	return &reactions{rt: rt, s: rt.store}
}

// reactionCounts is the split tally for a reference plus the caller's own value.
type reactionCounts struct {
	Likes    int   `json:"likes"`
	Dislikes int   `json:"dislikes"`
	Mine     int16 `json:"mine"` // -1, 0, or 1; 0 also means "no reaction"
}

// applyTx performs the 3-state upsert for (key, actor) inside tx and returns
// the count deltas (each -1/0/+1) so the caller can denormalize a split counter
// in the same transaction. Neutral (0) is a stored state, not a delete.
//
// Concurrency: SELECT ... FOR UPDATE locks the existing row; a lost insert race
// re-selects and updates. Exact under concurrent double-like and switches.
func (r *reactions) applyTx(ctx context.Context, tx pgx.Tx, actor Actor, key contentref.ContentKey, value int16) (dLikes, dDislikes int, err error) {
	userID, ip, ok := reactionKey(actor)
	if !ok {
		return 0, 0, badRequest("cannot identify reactor (no user id or ip)")
	}
	prev, found, err := r.lockExisting(ctx, tx, userID, ip, key)
	if err != nil {
		return 0, 0, err
	}
	if found {
		if prev == value {
			return 0, 0, nil
		}
		if err := r.update(ctx, tx, userID, ip, key, value); err != nil {
			return 0, 0, err
		}
		dLikes, dDislikes = delta(prev, value)
		return r.bumpAndReturn(ctx, tx, key, dLikes, dDislikes)
	}
	// ON CONFLICT DO NOTHING makes the losing racer block on the other tx then
	// no-op; a bare INSERT would abort the whole transaction.
	tag, err := tx.Exec(ctx, `INSERT INTO `+r.s.t.reactions+`
		(`+keyCols+`, user_id, ip, value) VALUES ($1, $2, $3, $4, $5, $6, $7)`+onConflict(userID),
		append(keyArgs(key), nullIf(userID), nullIf(ip), value)...)
	if err != nil {
		return 0, 0, err
	}
	if tag.RowsAffected() == 1 {
		dLikes, dDislikes = delta(0, value)
		return r.bumpAndReturn(ctx, tx, key, dLikes, dDislikes)
	}
	// Lost the insert race: the row now exists (committed); lock + update it.
	prev, found, err = r.lockExisting(ctx, tx, userID, ip, key)
	if err != nil || !found {
		return 0, 0, err
	}
	if prev == value {
		return 0, 0, nil
	}
	if err := r.update(ctx, tx, userID, ip, key, value); err != nil {
		return 0, 0, err
	}
	dLikes, dDislikes = delta(prev, value)
	return r.bumpAndReturn(ctx, tx, key, dLikes, dDislikes)
}

func (r *reactions) update(ctx context.Context, tx pgx.Tx, userID, ip string, key contentref.ContentKey, value int16) error {
	_, err := tx.Exec(ctx, `UPDATE `+r.s.t.reactions+` SET value = $1, updated_at = now()
		WHERE `+keyPred(2)+` AND `+actorPred(userID, 6), append([]any{value}, append(keyArgs(key), actorArg(userID, ip))...)...)
	return err
}

// bumpAndReturn denormalizes the reaction delta into the rollup (same tx).
func (r *reactions) bumpAndReturn(ctx context.Context, tx pgx.Tx, key contentref.ContentKey, dLikes, dDislikes int) (int, int, error) {
	if err := bumpCounts(ctx, tx, r.s, key, dLikes, dDislikes, 0, 0); err != nil {
		return 0, 0, err
	}
	return dLikes, dDislikes, nil
}

// lockExisting selects+locks the caller's current reaction row, if any.
func (r *reactions) lockExisting(ctx context.Context, tx pgx.Tx, userID, ip string, key contentref.ContentKey) (value int16, found bool, err error) {
	err = tx.QueryRow(ctx, `SELECT value FROM `+r.s.t.reactions+`
		WHERE `+keyPred(1)+` AND `+actorPred(userID, 5)+` FOR UPDATE`,
		append(keyArgs(key), actorArg(userID, ip))...).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return value, true, nil
}

// react is the entry point for host-registered kinds: it gates on
// accessibility, applies the reaction under the canonical preference
// reference (or the resolver's for a declined target) and writes the
// preference snapshot in the same transaction. Returns the reference callers
// read counts by and the committed snapshot (nil when nothing changed or the
// target is not exported).
func (r *reactions) react(ctx context.Context, actor Actor, kind, id string, value int16) (contentref.ContentRef, *PreferenceSnapshot, error) {
	ref, err := r.rt.gate(ctx, kind, id, actor, true)
	if err != nil {
		return contentref.ContentRef{}, nil, err
	}
	storage, key, exportable := r.rt.preferences.target(actor, ref, PreferenceAxisReaction)
	tx, err := r.s.beginMutation(ctx)
	if err != nil {
		return contentref.ContentRef{}, nil, err
	}
	defer tx.Rollback(ctx)
	if err := r.rt.guardErasedSubject(ctx, tx, viewerID(actor)); err != nil {
		return contentref.ContentRef{}, nil, err
	}
	snap, err := r.rt.preferences.mutate(ctx, tx, key, exportable, value, func() (bool, error) {
		dLikes, dDislikes, err := r.applyTx(ctx, tx, actor, storage.Key(), value)
		return dLikes != 0 || dDislikes != 0, err
	})
	if err != nil {
		return contentref.ContentRef{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return contentref.ContentRef{}, nil, err
	}
	return storage, snap, nil
}

// counts returns the split tally plus the caller's own reaction: an O(1) read
// of the rollup applyTx maintains in-tx.
func (r *reactions) counts(ctx context.Context, q querier, actor Actor, key contentref.ContentKey) (reactionCounts, error) {
	var out reactionCounts
	if err := q.QueryRow(ctx, `SELECT likes, dislikes FROM `+r.s.t.counts+` WHERE `+keyPred(1), keyArgs(key)...).Scan(&out.Likes, &out.Dislikes); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if userID, ip, ok := reactionKey(actor); ok {
		var mine int16
		err := q.QueryRow(ctx, `SELECT value FROM `+r.s.t.reactions+` WHERE `+keyPred(1)+` AND `+actorPred(userID, 5),
			append(keyArgs(key), actorArg(userID, ip))...).Scan(&mine)
		if err == nil {
			out.Mine = mine
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return out, err
		}
	}
	return out, nil
}

// --- HTTP ---

func (r *reactions) mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /{kind}/{id}/like", r.handleSet(1))
	mux.HandleFunc("POST /{kind}/{id}/dislike", r.handleSet(-1))
	mux.HandleFunc("POST /{kind}/{id}/neutral", r.handleSet(0))
	mux.HandleFunc("DELETE /{kind}/{id}/reaction", r.handleSet(0))
	mux.HandleFunc("GET /{kind}/{id}/reaction", r.handleGet)
}

func (r *reactions) handleSet(value int16) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		actor := r.rt.actor(req.Context())
		ref, _, err := r.react(req.Context(), actor, req.PathValue("kind"), req.PathValue("id"), value)
		if err != nil {
			writeErr(w, err)
			return
		}
		cnt, err := r.counts(req.Context(), r.s.pool, actor, ref.Key())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, cnt)
	}
}

func (r *reactions) handleGet(w http.ResponseWriter, req *http.Request) {
	actor := r.rt.actor(req.Context())
	ref, err := r.rt.gate(req.Context(), req.PathValue("kind"), req.PathValue("id"), actor, false)
	if err != nil {
		writeErr(w, err)
		return
	}
	ref, _ = r.rt.preferences.work(ref) // counts live under the canonical reference
	cnt, err := r.counts(req.Context(), r.s.pool, actor, ref.Key())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cnt)
}

// --- reaction key + delta helpers (shared by comments/posts via applyTx) ---

// reactionKey returns the dedup identity: a user id when present, else the IP.
// ok=false when the actor is fully unidentifiable.
func reactionKey(a Actor) (userID, ip string, ok bool) {
	if a.ID != "" && !a.Anonymous {
		return a.ID, a.IP, true
	}
	if a.IP != "" {
		return "", a.IP, true
	}
	return "", "", false
}

// actorPred/actorArg build the predicate + bound arg selecting the caller's
// row; n is the 1-based placeholder index of the actor argument.
func actorPred(userID string, n int) string {
	if userID != "" {
		return "user_id = $" + strconv.Itoa(n)
	}
	return "user_id IS NULL AND ip = $" + strconv.Itoa(n)
}

func actorArg(userID, ip string) string {
	if userID != "" {
		return userID
	}
	return ip
}

// onConflict targets the partial unique index for the actor's dedup key.
func onConflict(userID string) string {
	if userID != "" {
		return ` ON CONFLICT (` + keyCols + `, user_id) WHERE user_id IS NOT NULL DO NOTHING`
	}
	return ` ON CONFLICT (` + keyCols + `, ip) WHERE user_id IS NULL AND ip IS NOT NULL DO NOTHING`
}

func delta(prev, next int16) (dLikes, dDislikes int) {
	return b2i(next == 1) - b2i(prev == 1), b2i(next == -1) - b2i(prev == -1)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIf(s string) any {
	if s == "" {
		return nil
	}
	return s
}
