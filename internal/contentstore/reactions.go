package contentstore

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	content "github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/signalstore"
	"github.com/open-rails/contentkit/internal/subject"
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

// reactor is whose reaction a request writes and reads: an account, or a
// signed-out browser by its anonymous subject (subject.Anonymous). Zero when
// the request is neither.
type reactor struct {
	user string
	anon uuid.UUID
}

func (w reactor) ok() bool { return w.user != "" || w.anon != uuid.Nil }

// pred selects the reactor's row; n is the placeholder of arg.
func (w reactor) pred(n int) string {
	if w.user != "" {
		return "user_id = $" + strconv.Itoa(n)
	}
	return "anonymous_subject = $" + strconv.Itoa(n)
}

func (w reactor) arg() any {
	if w.user != "" {
		return w.user
	}
	return w.anon
}

// conflict targets the reactor's unique index.
func (w reactor) conflict() string {
	if w.user != "" {
		return ` ON CONFLICT (` + keyCols + `, user_id) WHERE user_id IS NOT NULL DO NOTHING`
	}
	return ` ON CONFLICT (` + keyCols + `, anonymous_subject) WHERE anonymous_subject IS NOT NULL DO NOTHING`
}

// reactor is the person actor reacts as: its account, or signed out the
// anonymous id the request carried (ContentKit-Anonymous-Id).
func (rt *Runtime) reactor(ctx context.Context, actor access.Actor) reactor {
	if actor.Member() {
		return reactor{user: actor.AccountID()}
	}
	if !actor.Anonymous {
		return reactor{}
	}
	who, _ := subject.Anonymous(rt.app, subject.Browser(ctx))
	return reactor{anon: who}
}

// applyTx performs the 3-state upsert for (key, who) inside tx and returns
// the count deltas (each -1/0/+1) so the caller can denormalize a split counter
// in the same transaction. Neutral (0) is a stored state, not a delete (neutral
// with no row is a no-op). A signed-out reactor's write is its activity.
//
// Concurrency: SELECT ... FOR UPDATE locks the existing row; a lost insert race
// re-selects and updates. Exact under concurrent double-like and switches.
func (r *reactions) applyTx(ctx context.Context, tx pgx.Tx, who reactor, key contentref.ContentKey, value int16) (dLikes, dDislikes int, err error) {
	dLikes, dDislikes, err = r.apply(ctx, tx, who, key, value)
	if err == nil && who.anon != uuid.Nil && r.rt.touch != nil {
		err = r.rt.touch(ctx, tx, who.anon)
	}
	return dLikes, dDislikes, err
}

func (r *reactions) apply(ctx context.Context, tx pgx.Tx, who reactor, key contentref.ContentKey, value int16) (dLikes, dDislikes int, err error) {
	if !who.ok() {
		return 0, 0, errNoReactor
	}
	prev, hidden, found, err := r.lockExisting(ctx, tx, who, key)
	if err != nil {
		return 0, 0, err
	}
	if found {
		if prev == value {
			_, err := tx.Exec(ctx, `UPDATE `+r.s.t.reactions+` SET import_source = '' WHERE `+keyPred(1)+` AND `+who.pred(5)+` AND import_source IS DISTINCT FROM ''`, append(keyArgs(key), who.arg())...)
			return 0, 0, err
		}
		if err := r.update(ctx, tx, who, key, value); err != nil {
			return 0, 0, err
		}
		if hidden {
			return 0, 0, nil
		}
		dLikes, dDislikes = delta(prev, value)
		return r.bumpAndReturn(ctx, tx, key, dLikes, dDislikes)
	}
	if value == 0 {
		return 0, 0, nil
	}
	if who.user != "" {
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+r.rt.hiddenAuthorsTable()+` WHERE tenant_id=$1 AND actor_id=$2)`, r.s.tenant, who.user).Scan(&hidden); err != nil {
			return 0, 0, err
		}
	}
	// ON CONFLICT DO NOTHING makes the losing racer block on the other tx then
	// no-op; a bare INSERT would abort the whole transaction.
	var user, anon any
	if who.user != "" {
		user = who.user
	} else {
		anon = who.anon
	}
	tag, err := tx.Exec(ctx, `INSERT INTO `+r.s.t.reactions+`
		(`+keyCols+`, user_id, anonymous_subject, value, author_hidden, import_source) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, '')`+who.conflict(),
		append(keyArgs(key), user, anon, value, hidden)...)
	if err != nil {
		return 0, 0, err
	}
	if tag.RowsAffected() == 1 {
		if hidden {
			return 0, 0, nil
		}
		dLikes, dDislikes = delta(0, value)
		return r.bumpAndReturn(ctx, tx, key, dLikes, dDislikes)
	}
	// Lost the insert race: the row now exists (committed); lock + update it.
	prev, hidden, found, err = r.lockExisting(ctx, tx, who, key)
	if err != nil || !found {
		return 0, 0, err
	}
	if prev == value {
		_, err := tx.Exec(ctx, `UPDATE `+r.s.t.reactions+` SET import_source = '' WHERE `+keyPred(1)+` AND `+who.pred(5)+` AND import_source IS DISTINCT FROM ''`, append(keyArgs(key), who.arg())...)
		return 0, 0, err
	}
	if err := r.update(ctx, tx, who, key, value); err != nil {
		return 0, 0, err
	}
	if hidden {
		return 0, 0, nil
	}
	dLikes, dDislikes = delta(prev, value)
	return r.bumpAndReturn(ctx, tx, key, dLikes, dDislikes)
}

func (r *reactions) update(ctx context.Context, tx pgx.Tx, who reactor, key contentref.ContentKey, value int16) error {
	_, err := tx.Exec(ctx, `UPDATE `+r.s.t.reactions+` SET import_source = '', value = $1, updated_at = now()
		WHERE `+keyPred(2)+` AND `+who.pred(6), append([]any{value}, append(keyArgs(key), who.arg())...)...)
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
func (r *reactions) lockExisting(ctx context.Context, tx pgx.Tx, who reactor, key contentref.ContentKey) (value int16, hidden, found bool, err error) {
	err = tx.QueryRow(ctx, `SELECT value,author_hidden FROM `+r.s.t.reactions+`
		WHERE `+keyPred(1)+` AND `+who.pred(5)+` FOR UPDATE`,
		append(keyArgs(key), who.arg())...).Scan(&value, &hidden)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false, err
	}
	return value, hidden, true, nil
}

// react applies the reaction under the canonical reference, which it
// returns. The actor must see the target's reactions: content they open, or
// whose kind's locked view shows reactions.
func (r *reactions) react(ctx context.Context, actor access.Actor, who reactor, target contentref.ContentRef, value int16) (contentref.ContentRef, error) {
	if actor.Account != nil && !actor.Member() {
		return contentref.ContentRef{}, errForbidden
	}
	ref, res, err := r.rt.resolveTarget(ctx, target, actor, false)
	if err == nil && !r.rt.shows(ref, res, kinds.ShowReactions) {
		err = ErrForbidden
	}
	if err == nil {
		err = r.rt.limitBallot(ctx, actor, who, ref.Key())
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
	if _, _, err := r.applyTx(ctx, tx, who, ref.Key(), value); err != nil {
		return contentref.ContentRef{}, err
	}
	return ref, tx.Commit(ctx)
}

// counts returns the caller's own reaction and, when tally, the split tally:
// an O(1) read of the rollup applyTx maintains in-tx.
func (r *reactions) counts(ctx context.Context, q querier, who reactor, key contentref.ContentKey, tally bool) (content.ReactionCounts, error) {
	var out content.ReactionCounts
	if tally {
		var likes, dislikes int
		if err := q.QueryRow(ctx, `SELECT likes, dislikes FROM `+r.s.t.counts+` WHERE `+keyPred(1), keyArgs(key)...).Scan(&likes, &dislikes); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return out, err
		}
		out.Likes, out.Dislikes = &likes, &dislikes
	}
	if who.ok() {
		var mine int16
		err := q.QueryRow(ctx, `SELECT value FROM `+r.s.t.reactions+` WHERE `+keyPred(1)+` AND `+who.pred(5),
			append(keyArgs(key), who.arg())...).Scan(&mine)
		if err == nil {
			out.Mine = mine
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return out, err
		}
	}
	return out, nil
}

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
				Responses: []httpapi.Reply{httpapi.OK(content.ReactionCounts{})}, Errors: []string{content.CodeNotFound}},
				Serve: func(r *reactions) http.HandlerFunc { return r.rt.targetHandler(typ, r.handleGet) }})
	}
	return out
}

func reaction(typ content.TargetType, method, verb, doc string, value int16) httpapi.Route[*reactions] {
	return httpapi.Route[*reactions]{Spec: httpapi.Spec{Method: method, Path: "/" + string(typ) + "/{id}/" + verb, Resource: "reactions", Auth: httpapi.Public,
		Doc: doc, Query: targetQuery(typ), Responses: []httpapi.Reply{httpapi.OK(content.ReactionCounts{})},
		Errors: []string{content.CodeForbidden, content.CodeInvalidRequest, content.CodeNotFound, content.CodeRateLimited, content.CodeUnauthorized}},
		Serve: func(r *reactions) http.HandlerFunc { return r.handleSet(typ, value) }}
}

// lockedNote says in route docs that a content target's counts follow its
// kind's locked view.
func lockedNote(typ content.TargetType, them string) string {
	if typ != content.TargetContent {
		return ""
	}
	return " (absent where its kind's locked view hides " + them + ")"
}

// targetNoun names a target type in route docs.
func targetNoun(typ content.TargetType) string {
	if typ == content.TargetChannel {
		return "a channel"
	}
	if typ != content.TargetContent {
		return "an entity"
	}
	return "a content item or one of its versions"
}

func (r *reactions) handleSet(typ content.TargetType, value int16) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		actor := r.rt.actor(req.Context())
		who := r.rt.reactor(req.Context(), actor)
		err := participant(actor, r.rt.anonymous.Reactions)
		if err == nil && !who.ok() {
			err = errNoReactor
		}
		if err == nil {
			err = r.rt.limit(req.Context(), content.ActionReaction, actor)
		}
		var target contentref.ContentRef
		if err == nil {
			target, err = r.rt.target(req, typ)
		}
		var ref contentref.ContentRef
		if err == nil {
			ref, err = r.react(req.Context(), actor, who, target, value)
		}
		if err == nil {
			r.rt.logActivity(req, actor, signalstore.ActionReaction, ref, value)
		}
		var cnt content.ReactionCounts
		if err == nil {
			cnt, err = r.counts(req.Context(), r.s.pool, who, ref.Key(), true)
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
	var cnt content.ReactionCounts
	if err == nil {
		cnt, err = r.counts(req.Context(), r.s.pool, r.rt.reactor(req.Context(), actor), ref.Key(), r.rt.shows(ref, res, kinds.ShowReactions))
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cnt)
}

// errNoReactor refuses a signed-out reaction without the browser's
// anonymous id.
var errNoReactor = badRequest("a signed-out reaction needs the browser's anonymous id (" + subject.Header + ")")

func delta(prev, next int16) (dLikes, dDislikes int) {
	return b2i(next == 1) - b2i(prev == 1), b2i(next == -1) - b2i(prev == -1)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
