package content

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
)

// Moderation states stored in social_comments.moderation / social_posts.moderation.
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
func (rt *Runtime) screen(ctx context.Context, in ModerationInput) (screening, error) {
	if in.SubjectID == "" {
		in.SubjectID = viewerID(in.Actor)
	}
	if err := rt.privateSubjectAllowed(ctx, rt.store.pool, in.SubjectID); err != nil {
		return screening{}, err
	}
	if rt.moderator == nil {
		return screening{state: ModerationApproved}, nil
	}
	in.Tenant = rt.tenant
	v, err := rt.moderator.Screen(ctx, in)
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
func viewerID(a Actor) string {
	if a.Anonymous {
		return ""
	}
	return a.ID
}

// visiblePred renders the moderation predicate for a reader; n is the
// placeholder bound to viewerID (an empty id matches no author).
func visiblePred(n int) string {
	return "(moderation = '" + ModerationApproved + "' OR user_id = $" + strconv.Itoa(n) + ")"
}

// --- Chain and BasicModerator ---

// Chain runs moderators in order; the first reject or review verdict wins and
// an error fails the chain. Put a BasicModerator in front of an AI moderator.
type Chain []ContentModerator

func (c Chain) Screen(ctx context.Context, in ModerationInput) (Verdict, error) {
	for _, m := range c {
		v, err := m.Screen(ctx, in)
		if err != nil || v.Decision != DecisionApprove {
			return v, err
		}
	}
	return Verdict{Decision: DecisionApprove, Model: "contentkit/chain"}, nil
}

// BasicModerator is the deterministic policy: reject links, near-instant
// duplicate submissions from one actor and a censor word list. The dup guard
// is per process; multi-replica hosts wanting global dedup wire their own.
type BasicModerator struct {
	AllowLinks  bool          // default false: reject bodies containing URLs
	DupWindow   time.Duration // 0 = 30s; negative disables
	CensorWords []string      // nil = built-in set; empty = none; whole words, case-insensitive

	once     sync.Once
	censorRe *regexp.Regexp
	mu       sync.Mutex
	recent   map[duplicateKey]time.Time
	erased   map[privateSubjectKey]bool
	swept    time.Time
	now      func() time.Time
}

const basicModel = "contentkit/basic"

var (
	defaultCensor = []string{"childporn", "cp"}
	urlRe         = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+`)
)

func (m *BasicModerator) init() {
	m.once.Do(func() {
		if m.DupWindow == 0 {
			m.DupWindow = 30 * time.Second
		}
		if m.now == nil {
			m.now = time.Now
		}
		words := m.CensorWords
		if words == nil {
			words = defaultCensor
		}
		if len(words) > 0 {
			quoted := make([]string, len(words))
			for i, w := range words {
				quoted[i] = regexp.QuoteMeta(w)
			}
			m.censorRe = regexp.MustCompile(`(?i)\b(` + strings.Join(quoted, "|") + `)\b`)
		}
		m.recent = map[duplicateKey]time.Time{}
		m.erased = map[privateSubjectKey]bool{}
	})
}

// Screen applies the rules to Title and Text. Edits skip the dup guard.
func (m *BasicModerator) Screen(_ context.Context, in ModerationInput) (Verdict, error) {
	m.init()
	text := strings.TrimSpace(in.Title + "\n" + in.Text)
	if !m.AllowLinks && urlRe.MatchString(text) {
		return Verdict{Decision: DecisionReject, Reason: "links are not allowed", Model: basicModel}, nil
	}
	if m.censorRe != nil && m.censorRe.MatchString(text) {
		return Verdict{Decision: DecisionReject, Reason: "content violates policy", Model: basicModel}, nil
	}
	if m.DupWindow > 0 && in.ItemID == "" {
		actor := in.Actor.ID
		if actor == "" {
			actor = in.Actor.IP
		}
		subject := privateSubjectKey{in.Tenant, actor}
		key := duplicateKey{subject, sha256.Sum256([]byte(text))}
		now := m.now()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.erased[subject] {
			return Verdict{}, ErrSubjectErased
		}
		if now.Sub(m.swept) >= m.DupWindow { // bound memory once per window
			m.swept = now
			for k, t := range m.recent {
				if now.Sub(t) >= m.DupWindow {
					delete(m.recent, k)
				}
			}
		}
		if t, ok := m.recent[key]; ok && now.Sub(t) < m.DupWindow {
			return Verdict{Decision: DecisionReject, Reason: "duplicate submission, slow down", Model: basicModel}, nil
		}
		m.recent[key] = now
	}
	return Verdict{Decision: DecisionApprove, Model: basicModel}, nil
}

// --- review queue ---

// HeldItem is one comment or post awaiting review.
type HeldItem struct {
	Kind     string `json:"kind"`     // KindComment | KindPost
	Revision int64  `json:"revision"` // required when resolving this exact screened text
	ID       string `json:"id"`
	// Ref is the commented content, or the post's own reference.
	Ref           contentref.ContentRef `json:"ref"`
	AuthorID      string                `json:"author_id,omitempty"`
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

// ListHeld pages this tenant's held comments or posts oldest-first; cursor is
// the previous page's Next ("" = start). limit is clamped to [1, 100].
func (rt *Runtime) ListHeld(ctx context.Context, kind, cursor string, limit int) (HeldPage, error) {
	if kind != KindComment && kind != KindPost {
		return HeldPage{}, badRequest("kind must be %s or %s", KindComment, KindPost)
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
	var sql string
	if kind == KindComment {
		sql = `SELECT id::text, ` + keyCols + `, coalesce(user_id, ''), coalesce(anon_name, ''), '', body,
			coalesce(moderation_reason, ''), moderation_verdict, updated_at, created_at, moderation_revision
			FROM ` + rt.store.t.comments + ` WHERE tenant_id = $1 AND moderation = 'held' AND deleted_at IS NULL AND (created_at, id::text) > ($2, $3)
			ORDER BY created_at, id LIMIT $4`
	} else {
		sql = `SELECT id, tenant_id, '` + KindPost + `', id, '', author_id, '', title, body,
			coalesce(moderation_reason, ''), moderation_verdict, updated_at, created_at, moderation_revision
			FROM ` + rt.store.t.posts + ` WHERE tenant_id = $1 AND moderation = 'held' AND deleted_at IS NULL AND (created_at, id) > ($2, $3)
			ORDER BY created_at, id LIMIT $4`
	}
	rows, err := rt.store.pool.Query(ctx, sql, rt.tenant, after, afterID, limit+1)
	if err != nil {
		return HeldPage{}, err
	}
	defer rows.Close()
	page := HeldPage{Items: []HeldItem{}}
	for rows.Next() {
		it := HeldItem{Kind: kind}
		var k contentref.ContentKey
		var meta []byte
		if err := rows.Scan(&it.ID, &k.TenantID, &k.ContentKind, &k.ContentID, &k.ContentVersionID, &it.AuthorID, &it.AnonName, &it.Title, &it.Body, &it.Reason, &meta, &it.HeldAt, &it.CreatedAt, &it.Revision); err != nil {
			return HeldPage{}, err
		}
		it.Ref = k.Ref()
		if len(meta) > 0 {
			var m verdictMeta
			if err := json.Unmarshal(meta, &m); err == nil {
				it.Model, it.PromptVersion, it.Confidence, it.Error = m.Model, m.PromptVersion, m.Confidence, m.Error
			}
		}
		page.Items = append(page.Items, it)
	}
	if err := rows.Err(); err != nil {
		return HeldPage{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.Next = strconv.FormatInt(last.CreatedAt.UnixMicro(), 10) + ":" + last.ID
	}
	return page, nil
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
// it (counts and, for posts, the keyword index follow); reject keeps it
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
	switch kind {
	case KindComment:
		return rt.comments.resolve(ctx, id, state, d)
	case KindPost:
		return rt.posts.resolve(ctx, id, state, d)
	default:
		return badRequest("kind must be %s or %s", KindComment, KindPost)
	}
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
	if err := c.rt.guardPrivateSubject(ctx, tx, subject); err != nil {
		return err
	}
	var replyTo *string
	var deletedAt *time.Time
	var k contentref.ContentKey
	err = tx.QueryRow(ctx, `UPDATE `+c.s.t.comments+` SET published_body = CASE WHEN $3='approved' THEN NULL ELSE published_body END, moderation = $3, moderation_reason = CASE WHEN $3 = 'approved' THEN NULL ELSE coalesce(nullif($4, ''), moderation_reason) END,
		moderated_by = $5, moderated_at = now(), updated_at = now()
		WHERE id = $1 AND tenant_id = $2 AND moderation = 'held' AND deleted_at IS NULL AND moderation_revision = $6
		RETURNING reply_to_id::text, deleted_at, `+keyCols, cid, c.s.tenant, state, d.Reason, d.Reviewer, d.Revision).
		Scan(&replyTo, &deletedAt, &k.TenantID, &k.ContentKind, &k.ContentID, &k.ContentVersionID)
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
	}
	return tx.Commit(ctx)
}

// count applies a published-comment delta: the replied-to comment's
// reply_count for a reply, the reference's comment_count for a top-level one.
func (c *comments) count(ctx context.Context, tx pgx.Tx, replyTo *string, k contentref.ContentKey, delta int) error {
	if replyTo != nil {
		_, err := tx.Exec(ctx, `UPDATE `+c.s.t.comments+` SET reply_count = GREATEST(reply_count + $2, 0), updated_at = now() WHERE id = $1`, *replyTo, delta)
		return err
	}
	return bumpCounts(ctx, tx, c.s, k, 0, 0, 0, delta)
}

// resolve moves a held post to its final state and re-queues its keyword
// document (an approved post indexes; anything else drops the stale entry).
func (p *posts) resolve(ctx context.Context, id, state string, d ReviewDecision) error {
	tx, err := p.s.beginMutation(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var subject string
	if err := tx.QueryRow(ctx, `SELECT coalesce(author_id,'') FROM `+p.s.t.posts+` WHERE id=$1 AND tenant_id=$2`, id, p.s.tenant).Scan(&subject); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if err := p.rt.guardPrivateSubject(ctx, tx, subject); err != nil {
		return err
	}
	var language string
	err = tx.QueryRow(ctx, `UPDATE `+p.s.t.posts+` SET published_content = CASE WHEN $3='approved' THEN NULL ELSE published_content END, moderation = $3, moderation_reason = CASE WHEN $3 = 'approved' THEN NULL ELSE coalesce(nullif($4, ''), moderation_reason) END,
		moderated_by = $5, moderated_at = now(), updated_at = now()
		WHERE id = $1 AND tenant_id = $2 AND moderation = 'held' AND deleted_at IS NULL AND moderation_revision = $6 RETURNING language`,
		id, p.s.tenant, state, d.Reason, d.Reviewer, d.Revision).Scan(&language)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := p.markDirty(ctx, tx, id, language, false); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- HTTP (ModerationReview-gated) ---

func (rt *Runtime) mountModeration(mux *http.ServeMux) {
	mux.HandleFunc("GET /moderation/held", rt.handleListHeld)
	mux.HandleFunc("POST /moderation/{kind}/{id}/resolve", rt.handleResolve)
}

func (rt *Runtime) handleListHeld(w http.ResponseWriter, req *http.Request) {
	if err := rt.requirePerm(req.Context(), rt.actor(req.Context()), rt.perms.ModerationReview); err != nil {
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
	if err := rt.requirePerm(req.Context(), actor, rt.perms.ModerationReview); err != nil {
		writeErr(w, err)
		return
	}
	var in struct {
		Decision Decision `json:"decision"`
		Revision int64    `json:"revision"`
		Reason   string   `json:"reason,omitempty"`
	}
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
	writeJSON(w, http.StatusOK, map[string]string{"decision": string(in.Decision)})
}

func (*BasicModerator) StatelessPolicy() {}

func policyIsStateless(p any) bool {
	if p == nil {
		return true
	}
	if chain, ok := p.(Chain); ok {
		for _, m := range chain {
			if !policyIsStateless(m) {
				return false
			}
		}
		return true
	}
	_, ok := p.(StatelessPolicy)
	return ok
}

type privateSubjectKey struct{ tenant, subject string }
type duplicateKey struct {
	subject privateSubjectKey
	digest  [32]byte
}

func (m *BasicModerator) EraseSubjects(_ context.Context, tenant string, ids []string) error {
	m.init()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		k := privateSubjectKey{tenant, id}
		m.erased[k] = true
		for cached := range m.recent {
			if cached.subject == k {
				delete(m.recent, cached)
			}
		}
	}
	return nil
}

func eraseLocalPolicy(ctx context.Context, p any, tenant string, ids []string) {
	switch m := p.(type) {
	case *BasicModerator:
		_ = m.EraseSubjects(ctx, tenant, ids)
	case Chain:
		for _, part := range m {
			eraseLocalPolicy(ctx, part, tenant, ids)
		}
	}
}
