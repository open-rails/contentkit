package content

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/internal/versionid"
)

// Moderation states stored in content_comments.moderation.
const (
	ModerationApproved = "approved"
	ModerationHeld     = "held"
	ModerationRejected = "rejected"
)

// heldReason is shown to the author when the moderator itself failed.
const heldReason = "awaiting review"

// verdictMeta is the moderation_verdict jsonb: screening provenance kept for
// the reviewer. Error records a moderator failure that forced the hold.
type verdictMeta struct {
	Model         string  `json:"model,omitempty"`
	PromptVersion string  `json:"prompt_version,omitempty"`
	Confidence    float64 `json:"confidence,omitempty"`
	Error         string  `json:"error,omitempty"`
}

// screening is what a write stores after the moderator ran.
type screening struct {
	state  string // ModerationApproved | ModerationHeld
	reason *string
	meta   []byte // jsonb or nil
}

// screen runs the moderator over one write. A reject verdict is returned as
// RejectedError (nothing stored). An error or unknown decision fails closed to
// a hold. Without a moderator every write is approved.
//
// A moderator slower than ModeratorTimeout counts as failed (held); after
// repeated failures the breaker holds writes without calling it.
func (rt *Runtime) screen(ctx context.Context, in ModerationInput) (screening, error) {
	if in.SubjectID == "" {
		in.SubjectID = viewerID(in.Actor)
	}
	if err := rt.checkSubjectNotErased(ctx, rt.store.pool, in.SubjectID); err != nil {
		return screening{}, err
	}
	if rt.moderator == nil {
		return screening{state: ModerationApproved}, nil
	}
	in.Tenant = rt.tenant
	v, err := rt.breaker.screen(ctx, rt.moderator, in)
	if err != nil {
		rt.log.Warn("content moderator failed; holding for review", "kind", in.Kind)
		meta, _ := json.Marshal(verdictMeta{Error: "moderator unavailable"})
		return screening{state: ModerationHeld, reason: ptrTo(heldReason), meta: meta}, nil
	}
	meta, _ := json.Marshal(verdictMeta{Model: v.Model, PromptVersion: v.PromptVersion, Confidence: v.Confidence})
	switch v.Decision {
	case DecisionApprove:
		return screening{state: ModerationApproved, meta: meta}, nil
	case DecisionReject:
		reason := strings.TrimSpace(v.Reason)
		if reason == "" {
			reason = "content violates policy"
		}
		return screening{}, RejectedError{Reason: reason}
	default:
		reason := strings.TrimSpace(v.Reason)
		if reason == "" {
			reason = heldReason
		}
		if v.Decision != DecisionReview {
			rt.log.Warn("content moderator returned an unknown decision; holding for review", "kind", in.Kind, "decision", string(v.Decision))
		}
		return screening{state: ModerationHeld, reason: &reason, meta: meta}, nil
	}
}

func ptrTo[T any](v T) *T { return &v }

// viewerID is the user id bound to reader predicates: an author sees their
// own held and rejected items; anonymous actors see approved items only.
func viewerID(a access.Actor) string {
	return a.AccountID()
}

// visiblePred renders the moderation predicate for a reader; n is the
// placeholder bound to viewerID (an empty id matches no author).
func visiblePred(n int) string {
	return "(moderation = '" + ModerationApproved + "' OR user_id = $" + strconv.Itoa(n) + ")"
}

// --- review queue ---

// HeldItem is one comment awaiting review.
type HeldItem struct {
	Kind     string `json:"kind"`     // KindComment
	Revision int64  `json:"revision"` // required when resolving this exact screened text
	ID       string `json:"id"`
	// Ref is the commented content, or the held item's own reference.
	Ref           contentref.ContentRef `json:"ref"`
	AuthorID      string                `json:"author_id,omitempty"`
	Author        *PublicUser           `json:"author,omitempty"`
	AnonName      string                `json:"anon_name,omitempty"`
	Title         string                `json:"title,omitempty"`
	Body          string                `json:"body"`
	Reason        string                `json:"reason"`
	Model         string                `json:"model,omitempty"`
	PromptVersion string                `json:"prompt_version,omitempty"`
	Confidence    float64               `json:"confidence,omitempty"`
	Error         string                `json:"error,omitempty"` // moderator failure that forced the hold
	HeldAt        time.Time             `json:"held_at"`
	CreatedAt     time.Time             `json:"created_at"`
}

// HeldPage is one page of the review queue; Next resumes after the last item.
type HeldPage struct {
	Items []HeldItem `json:"items"`
	Next  string     `json:"next,omitempty"`
}

// ReviewDecision resolves a held item: approve publishes it, reject keeps it
// author-only with Reason (or the moderator's reason when empty).
type ReviewDecision struct {
	Revision int64 // the revision returned by ListHeld; stale decisions cannot publish edits
	Decision Decision
	Reviewer string
	Reason   string
}

// SetVerdict fills the screening provenance from a moderation_verdict.
func (it *HeldItem) SetVerdict(meta []byte) {
	var m verdictMeta
	if len(meta) > 0 && json.Unmarshal(meta, &m) == nil {
		it.Model, it.PromptVersion, it.Confidence, it.Error = m.Model, m.PromptVersion, m.Confidence, m.Error
	}
}

// ListHeld pages this tenant's held comments oldest first; cursor is the previous page's Next ("" = start). limit is
// clamped to [1, 100].
func (rt *Runtime) ListHeld(ctx context.Context, kind, cursor string, limit int) (HeldPage, error) {
	if kind != KindComment {
		return HeldPage{}, badRequest("kind must be %s", KindComment)
	}
	if limit <= 0 {
		limit = 20
	} else if limit > 100 {
		limit = 100
	}
	after, afterID, err := parseHeldCursor(cursor)
	if err != nil {
		return HeldPage{}, err
	}
	page := HeldPage{Items: []HeldItem{}}
	if err := rt.heldComments(ctx, &page, after, afterID, limit+1, nil); err != nil {
		return HeldPage{}, err
	}
	page.trim(limit)
	rt.enrichHeld(ctx, page.Items)
	return page, nil
}

// heldComments pages held comments, of g and the content it owns when set.
func (rt *Runtime) heldComments(ctx context.Context, page *HeldPage, after time.Time, afterID string, limit int, g *resolve.OwnerEntity) error {
	var kind, id string
	if g != nil {
		kind, id = g.Kind, g.ID
	}
	rows, err := rt.store.pool.Query(ctx, `SELECT id::text, `+keyCols+`, coalesce(user_id, ''), coalesce(anon_name, ''), body,
			coalesce(moderation_reason, ''), moderation_verdict, updated_at, created_at, moderation_revision
			FROM `+rt.store.t.comments+` c WHERE tenant_id = $1 AND moderation = 'held' AND deleted_at IS NULL AND (created_at, id::text) > ($2, $3)
			AND ($5 = '' OR c.content_kind = $5 AND c.content_id = $6 OR (c.content_kind, c.content_id) IN (
			 SELECT i.kind, i.id FROM `+rt.store.t.items+` i WHERE i.tenant_id = $1 AND i.owner_entity_id = $6))
			ORDER BY created_at, id LIMIT $4`, rt.tenant, after, afterID, limit, kind, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		it := HeldItem{Kind: KindComment}
		var k contentref.ContentKey
		var meta []byte
		if err := rows.Scan(&it.ID, &k.TenantID, &k.ContentKind, &k.ContentID, versionid.Into(&k.ContentVersionID), &it.AuthorID, &it.AnonName, &it.Body, &it.Reason, &meta, &it.HeldAt, &it.CreatedAt, &it.Revision); err != nil {
			return err
		}
		it.Ref = k.Ref()
		it.SetVerdict(meta)
		page.Items = append(page.Items, it)
	}
	return rows.Err()
}

// enrichHeld attaches the authors' display data; a failed lookup leaves bare ids.
func (rt *Runtime) enrichHeld(ctx context.Context, items []HeldItem) {
	var ids []string
	for _, it := range items {
		if it.AuthorID != "" {
			ids = append(ids, it.AuthorID)
		}
	}
	users := rt.authors(ctx, ids)
	for i := range items {
		if u, ok := users[items[i].AuthorID]; ok {
			items[i].Author = &u
		}
	}
}

// parseHeldCursor decodes "<unix micros>:<id>"; "" starts from the beginning.
func parseHeldCursor(cursor string) (time.Time, string, error) {
	if cursor == "" {
		return time.Time{}, "", nil
	}
	micros, id, ok := strings.Cut(cursor, ":")
	n, err := strconv.ParseInt(micros, 10, 64)
	if !ok || err != nil || id == "" {
		return time.Time{}, "", badRequest("invalid cursor")
	}
	return time.UnixMicro(n).UTC(), id, nil
}

// Resolve writes a reviewer's final decision on a held item. Approve publishes
// it (counts follow); reject keeps it
// author-visible with the reason. A missing, foreign-tenant or not-held item
// is ErrNotFound.
func (rt *Runtime) Resolve(ctx context.Context, kind, id string, d ReviewDecision) error {
	if d.Decision != DecisionApprove && d.Decision != DecisionReject {
		return badRequest("decision must be %s or %s", DecisionApprove, DecisionReject)
	}
	if d.Revision < 1 {
		return badRequest("reviewed revision is required")
	}
	if strings.TrimSpace(d.Reviewer) == "" {
		return badRequest("reviewer is required")
	}
	state := ModerationApproved
	if d.Decision == DecisionReject {
		state = ModerationRejected
	}
	if kind != KindComment {
		return badRequest("kind must be %s", KindComment)
	}
	return rt.comments.resolve(ctx, id, state, d)
}

// resolve moves a held comment to its final state; approving publishes it
// into the thread counts.
func (c *comments) resolve(ctx context.Context, cid, state string, d ReviewDecision) error {
	if !uuidRe.MatchString(cid) {
		return ErrNotFound
	}
	tx, err := c.s.beginMutation(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var subject string
	if err := tx.QueryRow(ctx, `SELECT coalesce(user_id,'') FROM `+c.s.t.comments+` WHERE id=$1 AND tenant_id=$2`, cid, c.s.tenant).Scan(&subject); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if err := c.rt.guardErasedSubject(ctx, tx, subject); err != nil {
		return err
	}
	if err := c.lockThread(ctx, tx, cid); err != nil {
		return err
	}
	var replyTo *string
	var deletedAt *time.Time
	var k contentref.ContentKey
	err = tx.QueryRow(ctx, `UPDATE `+c.s.t.comments+` SET published_body = CASE WHEN $3='approved' THEN NULL ELSE published_body END, moderation = $3, moderation_reason = CASE WHEN $3 = 'approved' THEN NULL ELSE coalesce(nullif($4, ''), moderation_reason) END,
		moderated_by = $5, moderated_at = now(), updated_at = now()
		WHERE id = $1 AND tenant_id = $2 AND moderation = 'held' AND deleted_at IS NULL AND moderation_revision = $6
		RETURNING reply_to_id::text, deleted_at, `+keyCols, cid, c.s.tenant, state, d.Reason, d.Reviewer, d.Revision).
		Scan(&replyTo, &deletedAt, &k.TenantID, &k.ContentKind, &k.ContentID, versionid.Into(&k.ContentVersionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state == ModerationApproved && deletedAt == nil {
		if err := c.count(ctx, tx, replyTo, k, 1); err != nil {
			return err
		}
		if err := c.restoreReplyNotices(ctx, tx, cid); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// lockThread locks a comment and the comment it replies to in id order, the
// order erasure locks comments in, before a write changes the reply's state
// and its parent's reply_count. reply_to_id never changes, so the parent is
// known up front; imported comments keep explicit ids, so it need not sort
// first.
func (c *comments) lockThread(ctx context.Context, tx pgx.Tx, cid string) error {
	_, err := tx.Exec(ctx, `SELECT 1 FROM `+c.s.t.comments+` WHERE tenant_id = $2 AND (id = $1 OR id = (SELECT reply_to_id FROM `+c.s.t.comments+` WHERE tenant_id = $2 AND id = $1))
		ORDER BY id FOR UPDATE`, cid, c.s.tenant)
	return err
}

// count applies a published-comment delta: the replied-to comment's
// reply_count for a reply, the reference's comment_count for a top-level one.
func (c *comments) count(ctx context.Context, tx pgx.Tx, replyTo *string, k contentref.ContentKey, delta int) error {
	if replyTo != nil {
		_, err := tx.Exec(ctx, `UPDATE `+c.s.t.comments+` SET reply_count = GREATEST(reply_count + $2, 0), updated_at = now() WHERE tenant_id = $3 AND id = $1`, *replyTo, delta, c.s.tenant)
		return err
	}
	return bumpCounts(ctx, tx, c.s, k, 0, 0, 0, delta)
}

// --- HTTP (ModerationReview-gated) ---

var moderationRoutes = []httpapi.Route[*Runtime]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/moderation/held", Resource: "moderation", Auth: httpapi.Staff, Perm: string(access.ModerationReview),
		Doc: "Held comments awaiting review, oldest first.",
		Query: []httpapi.Param{httpapi.Text("kind", "comment"), httpapi.Text("cursor", "the previous page's next"),
			httpapi.Int("limit", "page size: 20 by default, at most 100")},
		Responses: []httpapi.Reply{httpapi.OK(HeldPage{})}},
		Serve: httpapi.H((*Runtime).handleListHeld)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/moderation/{kind}/{id}/resolve", Resource: "moderation", Auth: httpapi.Staff, Perm: string(access.ModerationReview),
		Doc:       "Approves (publishes) or rejects a held comment, at the revision the queue listed.",
		Request:   ResolveInput{},
		Responses: []httpapi.Reply{httpapi.OK(ReviewOutcome{})}, Errors: []string{CodeConflict, CodeNotFound}},
		Serve: httpapi.H((*Runtime).handleResolve)},
}

// ResolveInput resolves a held item at the revision the queue listed; Reason
// replaces the moderator's on a rejection.
type ResolveInput struct {
	Decision Decision `json:"decision"`
	Revision int64    `json:"revision"`
	Reason   string   `json:"reason,omitempty"`
}

// ReviewOutcome is the decision applied.
type ReviewOutcome struct {
	Decision Decision `json:"decision"`
}

func (rt *Runtime) handleListHeld(w http.ResponseWriter, req *http.Request) {
	if err := rt.requirePerm(req.Context(), rt.actor(req.Context()), access.ModerationReview); err != nil {
		writeErr(w, err)
		return
	}
	q := req.URL.Query()
	limit, _ := parsePage(req)
	page, err := rt.ListHeld(req.Context(), q.Get("kind"), q.Get("cursor"), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (rt *Runtime) handleResolve(w http.ResponseWriter, req *http.Request) {
	actor := rt.actor(req.Context())
	if err := rt.requirePerm(req.Context(), actor, access.ModerationReview); err != nil {
		writeErr(w, err)
		return
	}
	var in ResolveInput
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	reviewer := actor.ID
	if reviewer == "" {
		reviewer = fmt.Sprintf("%s:%s", actor.Kind, actor.IP)
	}
	if err := rt.Resolve(req.Context(), req.PathValue("kind"), req.PathValue("id"), ReviewDecision{Revision: in.Revision, Decision: in.Decision, Reviewer: reviewer, Reason: in.Reason}); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ReviewOutcome{Decision: in.Decision})
}

func policyIsStateless(p any) bool {
	if p == nil {
		return true
	}
	_, ok := p.(StatelessPolicy)
	return ok
}

// Moderator breaker defaults.
const (
	moderatorBreakerFailures = 3
	defaultModeratorTimeout  = 5 * time.Second
	defaultModeratorCooldown = 30 * time.Second
)

var errModeratorOpen = errors.New("content: moderator failing; writes held for review")

// moderatorBreaker bounds each Screen call and, after consecutive failures,
// skips the moderator for a cooldown. Then it is half-open: one trial call
// reaches the moderator while the others are held without calling it; the
// trial closes the breaker or reopens it for another cooldown.
type moderatorBreaker struct {
	timeout, cooldown time.Duration
	mu                sync.Mutex
	failures          int
	openUntil         time.Time
	trial             bool // a half-open trial call is in flight
	lastErr           error
}

func newModeratorBreaker(timeout, cooldown time.Duration) *moderatorBreaker {
	if timeout <= 0 {
		timeout = defaultModeratorTimeout
	}
	if cooldown <= 0 {
		cooldown = defaultModeratorCooldown
	}
	return &moderatorBreaker{timeout: timeout, cooldown: cooldown}
}

func (b *moderatorBreaker) screen(ctx context.Context, m ContentModerator, in ModerationInput) (Verdict, error) {
	b.mu.Lock()
	tripped := b.failures >= moderatorBreakerFailures
	if tripped && (b.trial || time.Now().Before(b.openUntil)) {
		b.mu.Unlock()
		return Verdict{}, errModeratorOpen
	}
	b.trial = tripped
	b.mu.Unlock()
	sctx, cancel := context.WithTimeout(ctx, b.timeout)
	v, err := m.Screen(sctx, in)
	cancel()
	b.mu.Lock()
	defer b.mu.Unlock()
	if tripped {
		b.trial = false
	}
	if err == nil {
		b.failures, b.lastErr = 0, nil
		return v, nil
	}
	if ctx.Err() != nil {
		if tripped {
			b.openUntil = time.Time{} // the trial's caller gave up: let the next caller try
		}
		return v, err // the caller gave up; not the moderator's failure
	}
	b.failures++
	b.lastErr = err
	if b.failures >= moderatorBreakerFailures {
		b.openUntil = time.Now().Add(b.cooldown)
	}
	return v, err
}

// CheckModerator reports the moderator's state without calling it: an error
// while repeated failures hold writes for review. Hosts register it as an
// optional dependency (helpers deps) for statusz and app_dependency_up.
func (rt *Runtime) CheckModerator(context.Context) error {
	if rt.moderator == nil {
		return nil
	}
	b := rt.breaker
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failures >= moderatorBreakerFailures {
		return fmt.Errorf("%w: %d consecutive failures, last: %v", errModeratorOpen, b.failures, b.lastErr)
	}
	return nil
}
