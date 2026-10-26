package content

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/kinds"
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

// ReactionCounts is the split tally for a reference plus the caller's own
// value. The tally is absent where the kind's locked view hides it from the
// caller.
type ReactionCounts struct {
	Likes    *int  `json:"likes,omitempty"`
	Dislikes *int  `json:"dislikes,omitempty"`
	Mine     int16 `json:"mine"` // -1, 0, or 1; 0 also means "no reaction"
}

// applyTx performs the 3-state upsert for (key, actor) inside tx and returns
// the count deltas (each -1/0/+1) so the caller can denormalize a split counter
// in the same transaction. Neutral (0) is a stored state, not a delete (neutral
// with no row is a no-op). Every value change takes a fresh preference
// revision under the row lock.
//
// Concurrency: SELECT ... FOR UPDATE locks the existing row; a lost insert race
// re-selects and updates. Exact under concurrent double-like and switches.
func (r *reactions) applyTx(ctx context.Context, tx pgx.Tx, actor access.Actor, key contentref.ContentKey, value int16) (dLikes, dDislikes int, err error) {
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
	if value == 0 {
		return 0, 0, nil
	}
	// ON CONFLICT DO NOTHING makes the losing racer block on the other tx then
	// no-op; a bare INSERT would abort the whole transaction.
	tag, err := tx.Exec(ctx, `INSERT INTO `+r.s.t.reactions+`
		(`+keyCols+`, user_id, ip, value, revision) VALUES ($1, $2, $3, $4, $5, $6, $7, `+r.s.nextRevision()+`)`+onConflict(userID),
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
	_, err := tx.Exec(ctx, `UPDATE `+r.s.t.reactions+` SET value = $1, revision = `+r.s.nextRevision()+`, updated_at = now()
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

// react applies the reaction under the canonical reference, which it
// returns. The actor must see the target's reactions: content they open, or
// whose kind's locked view shows reactions.
func (r *reactions) react(ctx context.Context, actor access.Actor, target contentref.ContentRef, value int16) (contentref.ContentRef, error) {
	if actor.Account != nil && !actor.Member() {
		return contentref.ContentRef{}, errForbidden
	}
	ref, res, err := r.rt.resolveTarget(ctx, target, actor, false)
	if err == nil && !r.rt.shows(ref, res, kinds.ShowReactions) {
		err = ErrForbidden
	}
	if err != nil {
		return contentref.ContentRef{}, err
	}
	tx, err := r.s.beginMutation(ctx)
	if err != nil {
		return contentref.ContentRef{}, err
	}
	defer tx.Rollback(ctx)
	if err := r.rt.guardErasedSubject(ctx, tx, viewerID(actor)); err != nil {
		return contentref.ContentRef{}, err
	}
	if _, _, err := r.applyTx(ctx, tx, actor, ref.Key(), value); err != nil {
		return contentref.ContentRef{}, err
	}
	return ref, tx.Commit(ctx)
}

// counts returns the caller's own reaction and, when tally, the split tally:
// an O(1) read of the rollup applyTx maintains in-tx.
func (r *reactions) counts(ctx context.Context, q querier, actor access.Actor, key contentref.ContentKey, tally bool) (ReactionCounts, error) {
	var out ReactionCounts
	if tally {
		var likes, dislikes int
		if err := q.QueryRow(ctx, `SELECT likes, dislikes FROM `+r.s.t.counts+` WHERE `+keyPred(1), keyArgs(key)...).Scan(&likes, &dislikes); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return out, err
		}
		out.Likes, out.Dislikes = &likes, &dislikes
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

func reactionRoutes() []httpapi.Route[*reactions] {
	var out []httpapi.Route[*reactions]
	for _, typ := range TargetTypes {
		noun := targetNoun(typ)
		out = append(out,
			reaction(typ, httpapi.POST, "like", "Likes "+noun+".", 1),
			reaction(typ, httpapi.POST, "dislike", "Dislikes "+noun+".", -1),
			reaction(typ, httpapi.DELETE, "reaction", "Clears the caller's reaction to "+noun+".", 0),
			httpapi.Route[*reactions]{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/" + string(typ) + "/{id}/reaction", Resource: "reactions", Auth: httpapi.Public,
				Doc: "The like and dislike counts of " + noun + lockedNote(typ, "them") + " and the caller's own reaction.", Query: targetQuery(typ),
				Responses: []httpapi.Reply{httpapi.OK(ReactionCounts{})}, Errors: []string{CodeNotFound}},
				Serve: func(r *reactions) http.HandlerFunc { return r.rt.targetHandler(typ, r.handleGet) }})
	}
	return out
}

func reaction(typ TargetType, method, verb, doc string, value int16) httpapi.Route[*reactions] {
	return httpapi.Route[*reactions]{Spec: httpapi.Spec{Method: method, Path: "/" + string(typ) + "/{id}/" + verb, Resource: "reactions", Auth: httpapi.Public,
		Doc: doc, Query: targetQuery(typ), Responses: []httpapi.Reply{httpapi.OK(ReactionCounts{})},
		Errors: []string{CodeForbidden, CodeInvalidRequest, CodeNotFound, CodeRateLimited, CodeUnauthorized}},
		Serve: func(r *reactions) http.HandlerFunc { return r.handleSet(typ, value) }}
}

// lockedNote says in route docs that a content target's counts follow its
// kind's locked view.
func lockedNote(typ TargetType, them string) string {
	if typ != TargetContent {
		return ""
	}
	return " (absent where its kind's locked view hides " + them + ")"
}

// targetNoun names a target type in route docs.
func targetNoun(typ TargetType) string {
	if typ == TargetChannel {
		return "a channel"
	}
	if typ != TargetContent {
		return "an entity"
	}
	return "a content item or one of its versions"
}

func (r *reactions) handleSet(typ TargetType, value int16) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		actor := r.rt.actor(req.Context())
		err := participant(actor, r.rt.anonymous.Reactions)
		if err == nil {
			err = r.rt.limit(req.Context(), ActionReaction, actor)
		}
		var target contentref.ContentRef
		if err == nil {
			target, err = r.rt.target(req, typ)
		}
		var ref contentref.ContentRef
		if err == nil {
			ref, err = r.react(req.Context(), actor, target, value)
		}
		var cnt ReactionCounts
		if err == nil {
			cnt, err = r.counts(req.Context(), r.s.pool, actor, ref.Key(), true)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, cnt)
	}
}

func (r *reactions) handleGet(w http.ResponseWriter, req *http.Request, target contentref.ContentRef) {
	actor := r.rt.actor(req.Context())
	ref, res, err := r.rt.resolveTarget(req.Context(), target, actor, false)
	var cnt ReactionCounts
	if err == nil {
		cnt, err = r.counts(req.Context(), r.s.pool, actor, ref.Key(), r.rt.shows(ref, res, kinds.ShowReactions))
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cnt)
}

// --- reaction key + delta helpers (shared by comments/posts via applyTx) ---

// reactionKey returns the dedup identity: a user id when present, else the IP.
// ok=false when the actor is fully unidentifiable.
func reactionKey(a access.Actor) (userID, ip string, ok bool) {
	if a.Member() {
		return a.AccountID(), a.IP, true
	}
	if a.Anonymous && a.IP != "" {
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
