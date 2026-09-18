package content

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// polls is the standalone site-wide poll module: admin-authored questions,
// either multiple_choice (options, anon-capable one-vote-per-(poll,user)/
// (poll,ip) tallying) or free_text (one editable answer per signed-in actor,
// grouped by the host's AnswerClassifier). It is not tied to host content, so
// it never gates on the ContentResolver: writes gate on the PollWrite perm;
// reads/votes/answers are public until the poll closes. Questions carry the
// tenant; options, votes and answers hang off their question.
type polls struct {
	rt *Runtime
	s  *store
}

func newPolls(rt *Runtime) *polls {
	return &polls{rt: rt, s: rt.store}
}

// pollOption is one choice with its denormalized vote_count.
type pollOption struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	ImageURL  string `json:"image_url,omitempty"`
	Position  int    `json:"position"`
	VoteCount int    `json:"vote_count"`
}

// Poll kinds.
const (
	PollMultipleChoice = "multiple_choice"
	PollFreeText       = "free_text"
)

// pollAnswer is the caller's own free-text answer.
type pollAnswer struct {
	ID         string    `json:"id"`
	Text       string    `json:"text"`
	Classified bool      `json:"classified"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// pollView is a question plus its options and the caller's own vote (if
// any); a free_text poll carries its answer groups and the caller's answer.
type pollView struct {
	ID         string       `json:"id"`
	Kind       string       `json:"kind"`
	Question   string       `json:"question"`
	Language   string       `json:"language"`
	IsActive   bool         `json:"is_active"`
	ImageURL   string       `json:"image_url,omitempty"`
	LiveAt     time.Time    `json:"live_at"`
	ClosesAt   *time.Time   `json:"closes_at,omitempty"`
	Closed     bool         `json:"closed"` // inactive or past closes_at: no more votes/answers
	TotalVotes int          `json:"total_votes"`
	Options    []pollOption `json:"options"`
	Voted      bool         `json:"voted"`
	MyOption   string       `json:"my_option,omitempty"`
	// free_text only
	AnswerCount       int         `json:"answer_count,omitempty"`
	Groups            []Group     `json:"groups,omitempty"`
	GroupsUnavailable bool        `json:"groups_unavailable,omitempty"` // the classifier failed to read groups
	MyAnswer          *pollAnswer `json:"my_answer,omitempty"`
}

type createPollInput struct {
	Kind     string              `json:"kind,omitempty"` // default multiple_choice
	Question string              `json:"question"`
	Language string              `json:"language"`
	ImageURL string              `json:"image_url,omitempty"`
	LiveAt   *time.Time          `json:"live_at,omitempty"`   // nil = live now
	ClosesAt *time.Time          `json:"closes_at,omitempty"` // nil = open until deactivated
	Options  []createOptionInput `json:"options,omitempty"`
}

// createOptionInput takes image_url directly (the MediaStore upload path is the
// host's job before calling — the kit stores the resolved url).
type createOptionInput struct {
	Label    string `json:"label"`
	ImageURL string `json:"image_url"`
	Position int    `json:"position"`
}

// updatePollInput uses pointers so absent fields are left untouched (COALESCE).
type updatePollInput struct {
	Question *string    `json:"question"`
	IsActive *bool      `json:"is_active"`
	LiveAt   *time.Time `json:"live_at"`
	ClosesAt *time.Time `json:"closes_at"`
	ImageURL *string    `json:"image_url"`
}

// --- admin (PollWrite-gated) ---

// create inserts a question and its options atomically. Fail-closed on perm.
// A free_text poll is refused without a registered AnswerClassifier.
func (p *polls) create(ctx context.Context, actor Actor, in createPollInput) (pollView, error) {
	if err := p.rt.requirePerm(ctx, actor, p.rt.perms.PollWrite); err != nil {
		return pollView{}, err
	}
	in.Question = strings.TrimSpace(in.Question)
	if in.Question == "" {
		return pollView{}, badRequest("question is required")
	}
	switch in.Kind {
	case "", PollMultipleChoice:
		in.Kind = PollMultipleChoice
		if len(in.Options) < 2 {
			return pollView{}, badRequest("a poll needs at least 2 options")
		}
	case PollFreeText:
		if p.rt.classifier == nil {
			return pollView{}, ErrNoClassifier
		}
		if len(in.Options) != 0 {
			return pollView{}, badRequest("a free-text poll takes no options")
		}
	default:
		return pollView{}, badRequest("kind must be %s or %s", PollMultipleChoice, PollFreeText)
	}
	if in.ClosesAt != nil && in.LiveAt != nil && !in.ClosesAt.After(*in.LiveAt) {
		return pollView{}, badRequest("closes_at must be after live_at")
	}
	for i := range in.Options {
		in.Options[i].Label = strings.TrimSpace(in.Options[i].Label)
		if in.Options[i].Label == "" {
			return pollView{}, badRequest("option %d label is required", i)
		}
	}

	tx, err := p.s.pool.Begin(ctx)
	if err != nil {
		return pollView{}, err
	}
	defer tx.Rollback(ctx)

	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO `+p.s.t.pollQuestions+`
		(tenant_id, kind, question, language, image_url, live_at, closes_at) VALUES ($1, $2, $3, $4, $5, COALESCE($6, now()), $7)
		RETURNING id::text`, p.s.tenant, in.Kind, in.Question, in.Language, nullIf(in.ImageURL), in.LiveAt, in.ClosesAt).Scan(&id); err != nil {
		return pollView{}, err
	}
	for _, o := range in.Options {
		if _, err := tx.Exec(ctx, `INSERT INTO `+p.s.t.pollOptions+`
			(question_id, label, image_url, position) VALUES ($1, $2, $3, $4)`,
			id, o.Label, nullIf(o.ImageURL), o.Position); err != nil {
			return pollView{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return pollView{}, err
	}
	return p.get(ctx, actor, id)
}

// update mutates question/is_active; nil fields are left as-is via COALESCE.
func (p *polls) update(ctx context.Context, actor Actor, id string, in updatePollInput) (pollView, error) {
	if err := p.rt.requirePerm(ctx, actor, p.rt.perms.PollWrite); err != nil {
		return pollView{}, err
	}
	if in.Question != nil {
		q := strings.TrimSpace(*in.Question)
		if q == "" {
			return pollView{}, badRequest("question cannot be blank")
		}
		in.Question = &q
	}
	tag, err := p.s.pool.Exec(ctx, `UPDATE `+p.s.t.pollQuestions+`
		SET question = COALESCE($2, question), is_active = COALESCE($3, is_active),
		    live_at = COALESCE($4, live_at), image_url = COALESCE($5, image_url),
		    closes_at = COALESCE($7, closes_at), updated_at = now()
		WHERE id = $1 AND tenant_id = $6 AND deleted_at IS NULL`, id, in.Question, in.IsActive, in.LiveAt, in.ImageURL, p.s.tenant, in.ClosesAt)
	if err != nil {
		return pollView{}, err
	}
	if tag.RowsAffected() == 0 {
		return pollView{}, ErrNotFound
	}
	return p.get(ctx, actor, id)
}

// softDelete flags deleted_at; the row (and its votes) stay for history.
func (p *polls) softDelete(ctx context.Context, actor Actor, id string) error {
	if err := p.rt.requirePerm(ctx, actor, p.rt.perms.PollWrite); err != nil {
		return err
	}
	tag, err := p.s.pool.Exec(ctx, `UPDATE `+p.s.t.pollQuestions+`
		SET deleted_at = now(), updated_at = now() WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`, id, p.s.tenant)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- public read ---

// listFilter narrows list: language, a month ("2006-01") or day ("2006-01-02")
// live_at window, and admin (include future/inactive polls; PollWrite-gated by
// the caller).
type listFilter struct {
	language, month, date string
	admin                 bool
	limit, offset         int
}

// list returns non-deleted polls newest-live-first with options + caller vote.
// Public mode never leaks future polls (live_at <= now). The default view (no
// month/date window) serves only the active poll(s); browsing a specific
// month/date archive returns all live polls in that window regardless of
// is_active, so historical polls stay readable once a newer poll becomes active.
// Voting remains gated on is_active elsewhere (see vote()).
func (p *polls) list(ctx context.Context, actor Actor, f listFilter) ([]pollView, error) {
	from, to, hasWindow := parseWindow(f.month, f.date)
	sql := `SELECT ` + pollCols + ` FROM ` + p.s.t.pollQuestions + ` WHERE tenant_id = $1 AND deleted_at IS NULL`
	if !f.admin {
		sql += ` AND live_at <= now()`
		// Only the default (windowless) view is restricted to the active poll;
		// an explicit month/date archive shows all live polls in that window.
		if !hasWindow {
			sql += ` AND is_active = true`
		}
	}
	args := []any{p.s.tenant}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if f.language != "" {
		sql += ` AND language = ` + arg(f.language)
	}
	if hasWindow {
		sql += ` AND live_at >= ` + arg(from) + ` AND live_at < ` + arg(to)
	}
	sql += ` ORDER BY live_at DESC LIMIT ` + arg(f.limit) + ` OFFSET ` + arg(f.offset)

	rows, err := p.s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var views []pollView
	for rows.Next() {
		v, err := scanPoll(rows)
		if err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := p.attach(ctx, actor, views); err != nil {
		return nil, err
	}
	if views == nil {
		views = []pollView{}
	}
	return views, nil
}

// parseWindow converts a month ("2006-01") or day ("2006-01-02") into a
// [from, to) UTC range; date wins when both are given.
func parseWindow(month, date string) (from, to time.Time, ok bool) {
	if date != "" {
		if d, err := time.Parse("2006-01-02", date); err == nil {
			return d, d.AddDate(0, 0, 1), true
		}
	}
	if month != "" {
		if m, err := time.Parse("2006-01", month); err == nil {
			return m, m.AddDate(0, 1, 0), true
		}
	}
	return time.Time{}, time.Time{}, false
}

const pollCols = `id::text, kind, question, language, is_active, coalesce(image_url,''), live_at, closes_at`

// scanPoll reads one pollCols row and derives Closed.
func scanPoll(row pgx.Row) (pollView, error) {
	v := pollView{Options: []pollOption{}}
	if err := row.Scan(&v.ID, &v.Kind, &v.Question, &v.Language, &v.IsActive, &v.ImageURL, &v.LiveAt, &v.ClosesAt); err != nil {
		return pollView{}, err
	}
	v.Closed = !v.IsActive || (v.ClosesAt != nil && !v.ClosesAt.After(time.Now()))
	return v, nil
}

// get returns one non-deleted poll (any state) with options + caller vote; the
// HTTP layer live-gates public access.
func (p *polls) get(ctx context.Context, actor Actor, id string) (pollView, error) {
	if !uuidRe.MatchString(id) {
		return pollView{}, ErrNotFound
	}
	v, err := scanPoll(p.s.pool.QueryRow(ctx, `SELECT `+pollCols+` FROM `+p.s.t.pollQuestions+` WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`, id, p.s.tenant))
	if errors.Is(err, pgx.ErrNoRows) {
		return pollView{}, ErrNotFound
	}
	if err != nil {
		return pollView{}, err
	}
	views := []pollView{v}
	if err := p.attach(ctx, actor, views); err != nil {
		return pollView{}, err
	}
	return views[0], nil
}

// attach batch-loads options + the caller's votes into views, sums total_votes,
// absolutizes stored-relative image paths (backfilled legacy rows) and, for
// free_text polls, loads the answer count, the caller's answer and the
// classifier's groups.
func (p *polls) attach(ctx context.Context, actor Actor, views []pollView) error {
	if len(views) == 0 {
		return nil
	}
	ids := make([]string, len(views))
	for i := range views {
		ids[i] = views[i].ID
	}
	opts, err := p.optionsFor(ctx, p.s.pool, ids)
	if err != nil {
		return err
	}
	votes, err := p.votesFor(ctx, p.s.pool, actor, ids)
	if err != nil {
		return err
	}
	counts, mine, err := p.answersFor(ctx, actor, ids)
	if err != nil {
		return err
	}
	for i := range views {
		views[i].ImageURL = p.rt.absMediaURL(views[i].ImageURL)
		if o := opts[views[i].ID]; o != nil {
			views[i].Options = o
		}
		total := 0
		for j := range views[i].Options {
			views[i].Options[j].ImageURL = p.rt.absMediaURL(views[i].Options[j].ImageURL)
			total += views[i].Options[j].VoteCount
		}
		views[i].TotalVotes = total
		if oid, ok := votes[views[i].ID]; ok {
			views[i].Voted, views[i].MyOption = true, oid
		}
		if views[i].Kind != PollFreeText {
			continue
		}
		views[i].AnswerCount = counts[views[i].ID]
		if a, ok := mine[views[i].ID]; ok {
			views[i].MyAnswer = &a
		}
		views[i].Groups = []Group{}
		if p.rt.classifier == nil {
			views[i].GroupsUnavailable = true
			continue
		}
		groups, err := p.rt.classifier.Groups(ctx, p.s.tenant, views[i].ID)
		if err != nil {
			p.rt.log.Warn("answer classifier groups failed", "poll", views[i].ID, "err", err.Error())
			views[i].GroupsUnavailable = true
			continue
		}
		sort.Slice(groups, func(a, b int) bool {
			if groups[a].Count != groups[b].Count {
				return groups[a].Count > groups[b].Count
			}
			if groups[a].Label != groups[b].Label {
				return groups[a].Label < groups[b].Label
			}
			return groups[a].ID < groups[b].ID
		})
		views[i].Groups = orEmpty(groups)
	}
	return nil
}

// answersFor batch-loads the answer count per question and the caller's own
// answers (signed-in actors only).
func (p *polls) answersFor(ctx context.Context, actor Actor, ids []string) (map[string]int, map[string]pollAnswer, error) {
	counts, mine := map[string]int{}, map[string]pollAnswer{}
	rows, err := p.s.pool.Query(ctx, `SELECT question_id::text, count(*) FROM `+p.s.t.pollAnswers+`
		WHERE tenant_id = $1 AND question_id = ANY($2::uuid[]) GROUP BY question_id`, p.s.tenant, ids)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var qid string
		var n int
		if err := rows.Scan(&qid, &n); err != nil {
			return nil, nil, err
		}
		counts[qid] = n
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if actor.Anonymous || actor.ID == "" {
		return counts, mine, nil
	}
	own, err := p.s.pool.Query(ctx, `SELECT question_id::text, id::text, text, classified_at IS NOT NULL, updated_at FROM `+p.s.t.pollAnswers+`
		WHERE tenant_id = $1 AND question_id = ANY($2::uuid[]) AND actor_id = $3`, p.s.tenant, ids, actor.ID)
	if err != nil {
		return nil, nil, err
	}
	defer own.Close()
	for own.Next() {
		var qid string
		var a pollAnswer
		if err := own.Scan(&qid, &a.ID, &a.Text, &a.Classified, &a.UpdatedAt); err != nil {
			return nil, nil, err
		}
		mine[qid] = a
	}
	return counts, mine, own.Err()
}

// optionsFor batch-loads options keyed by question id (one array-param query).
func (p *polls) optionsFor(ctx context.Context, q querier, ids []string) (map[string][]pollOption, error) {
	rows, err := q.Query(ctx, `SELECT question_id::text, id::text, label, coalesce(image_url, ''), position, vote_count
		FROM `+p.s.t.pollOptions+` WHERE question_id = ANY($1::uuid[]) ORDER BY question_id, position`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]pollOption{}
	for rows.Next() {
		var qid string
		var o pollOption
		if err := rows.Scan(&qid, &o.ID, &o.Label, &o.ImageURL, &o.Position, &o.VoteCount); err != nil {
			return nil, err
		}
		out[qid] = append(out[qid], o)
	}
	return out, rows.Err()
}

// votesFor batch-loads the caller's chosen option per question (qid -> optionID).
func (p *polls) votesFor(ctx context.Context, q querier, actor Actor, ids []string) (map[string]string, error) {
	userID, ip, ok := reactionKey(actor)
	if !ok {
		return map[string]string{}, nil
	}
	rows, err := q.Query(ctx, `SELECT question_id::text, option_id::text FROM `+p.s.t.pollVotes+`
		WHERE question_id = ANY($1::uuid[]) AND `+actorPred(userID, 2), ids, actorArg(userID, ip))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var qid, oid string
		if err := rows.Scan(&qid, &oid); err != nil {
			return nil, err
		}
		out[qid] = oid
	}
	return out, rows.Err()
}

// vote records one vote and bumps the option's counter — but only the winning
// insert (RowsAffected==1) counts, so concurrent duplicates never double-count.
func (p *polls) vote(ctx context.Context, actor Actor, pollID, optionID string) (pollView, error) {
	if optionID == "" {
		return pollView{}, badRequest("option_id is required")
	}
	userID, ip, ok := reactionKey(actor)
	if !ok {
		return pollView{}, badRequest("cannot identify voter (no user id or ip)")
	}

	tx, err := p.s.pool.Begin(ctx)
	if err != nil {
		return pollView{}, err
	}
	defer tx.Rollback(ctx)

	if err := p.open(ctx, tx, pollID, PollMultipleChoice); err != nil {
		return pollView{}, err
	}
	// Option must belong to this poll.
	var exists bool
	err = tx.QueryRow(ctx, `SELECT true FROM `+p.s.t.pollOptions+`
		WHERE id = $1 AND question_id = $2`, optionID, pollID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return pollView{}, badRequest("option does not belong to poll")
	}
	if err != nil {
		return pollView{}, err
	}

	// ON CONFLICT DO NOTHING makes a duplicate voter BLOCK then no-op (0 rows) —
	// a bare INSERT would raise 23505, aborting the whole tx (25P02).
	tag, err := tx.Exec(ctx, `INSERT INTO `+p.s.t.pollVotes+`
		(question_id, option_id, user_id, ip) VALUES ($1, $2, $3, $4)`+pollVoteConflict(userID),
		pollID, optionID, nullIf(userID), nullIf(ip))
	if err != nil {
		return pollView{}, err
	}
	if tag.RowsAffected() == 1 {
		if _, err := tx.Exec(ctx, `UPDATE `+p.s.t.pollOptions+`
			SET vote_count = vote_count + 1 WHERE id = $1`, optionID); err != nil {
			return pollView{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return pollView{}, err
	}
	return p.get(ctx, actor, pollID)
}

// open asserts a poll of this tenant takes votes/answers: it must exist, be
// live and not soft-deleted (all hidden as 404 so a future poll never leaks),
// be of kind, and be neither deactivated nor past closes_at.
func (p *polls) open(ctx context.Context, q querier, pollID, kind string) error {
	if !uuidRe.MatchString(pollID) {
		return ErrNotFound
	}
	var got string
	var active, closed bool
	err := q.QueryRow(ctx, `SELECT kind, is_active, closes_at IS NOT NULL AND closes_at <= now() FROM `+p.s.t.pollQuestions+`
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL AND live_at <= now()`, pollID, p.s.tenant).Scan(&got, &active, &closed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if got != kind {
		if kind == PollFreeText {
			return badRequest("poll takes votes, not free-text answers")
		}
		return badRequest("poll takes free-text answers, not votes")
	}
	if !active || closed {
		return badRequest("poll is closed")
	}
	return nil
}

// maxAnswerLen bounds a free-text answer (runes).
const maxAnswerLen = 2000

// answer stores or replaces the signed-in caller's free-text answer while the
// poll is open, then classifies it. A classifier failure keeps the answer
// unclassified for ReclassifyPending.
func (p *polls) answer(ctx context.Context, actor Actor, pollID, text string) (pollView, error) {
	if actor.Anonymous || actor.ID == "" {
		return pollView{}, errUnauthorized
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return pollView{}, badRequest("text is required")
	}
	if utf8.RuneCountInString(text) > maxAnswerLen {
		return pollView{}, badRequest("text exceeds %d characters", maxAnswerLen)
	}
	tx, err := p.s.pool.Begin(ctx)
	if err != nil {
		return pollView{}, err
	}
	defer tx.Rollback(ctx)
	if err := p.open(ctx, tx, pollID, PollFreeText); err != nil {
		return pollView{}, err
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO `+p.s.t.pollAnswers+` (tenant_id, question_id, actor_id, text)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, question_id, actor_id) DO UPDATE
		SET text = EXCLUDED.text, group_id = NULL, classified_at = NULL, updated_at = now()
		RETURNING id::text`, p.s.tenant, pollID, actor.ID, text).Scan(&id); err != nil {
		return pollView{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return pollView{}, err
	}
	if err := p.classify(ctx, Answer{Tenant: p.s.tenant, QuestionID: pollID, AnswerID: id, Text: text}); err != nil {
		p.rt.log.Warn("answer classifier failed; answer kept unclassified", "poll", pollID, "answer", id, "err", err.Error())
	}
	return p.get(ctx, actor, pollID)
}

// classify runs the classifier over one answer and records the assignment,
// unless the text changed meanwhile (that edit classifies itself).
func (p *polls) classify(ctx context.Context, a Answer) error {
	if p.rt.classifier == nil {
		return ErrNoClassifier
	}
	g, err := p.rt.classifier.Classify(ctx, a)
	if err != nil {
		return err
	}
	_, err = p.s.pool.Exec(ctx, `UPDATE `+p.s.t.pollAnswers+` SET group_id = $3, classified_at = now()
		WHERE id = $1 AND tenant_id = $2 AND text = $4 AND classified_at IS NULL`, a.AnswerID, p.s.tenant, g.GroupID, a.Text)
	return err
}

// ReclassifyPending retries classification of this tenant's unclassified
// free-text answers, oldest write first, up to limit (default 100). It
// returns how many were classified and the first classifier error, so a
// persistent failure surfaces to the host's scheduler.
func (rt *Runtime) ReclassifyPending(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := rt.store.pool.Query(ctx, `SELECT id::text, question_id::text, text FROM `+rt.store.t.pollAnswers+`
		WHERE tenant_id = $1 AND classified_at IS NULL ORDER BY updated_at LIMIT $2`, rt.tenant, limit)
	if err != nil {
		return 0, err
	}
	var pending []Answer
	for rows.Next() {
		a := Answer{Tenant: rt.tenant}
		if err := rows.Scan(&a.AnswerID, &a.QuestionID, &a.Text); err != nil {
			rows.Close()
			return 0, err
		}
		pending = append(pending, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var n int
	var first error
	for _, a := range pending {
		if err := rt.polls.classify(ctx, a); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		n++
	}
	return n, first
}

// ownsQuestion renders the predicate tying an option/vote row to a question of
// this tenant; col is the question id column, n the tenant placeholder.
func (p *polls) ownsQuestion(col string, n int) string {
	return `EXISTS (SELECT 1 FROM ` + p.s.t.pollQuestions + ` tq WHERE tq.id = ` + col + ` AND tq.tenant_id = $` + strconv.Itoa(n) + `)`
}

// pollVoteConflict targets the partial unique index for the voter's dedup key.
func pollVoteConflict(userID string) string {
	if userID != "" {
		return ` ON CONFLICT (question_id, user_id) WHERE user_id IS NOT NULL DO NOTHING`
	}
	return ` ON CONFLICT (question_id, ip) WHERE user_id IS NULL AND ip IS NOT NULL DO NOTHING`
}

// --- HTTP ---

func (p *polls) mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /polls", p.handleList)
	mux.HandleFunc("GET /polls/admin", p.handleAdminList) // literal beats {id}
	mux.HandleFunc("GET /polls/{id}", p.handleGet)
	mux.HandleFunc("POST /polls", p.handleCreate)
	mux.HandleFunc("PATCH /polls/{id}", p.handleUpdate)
	mux.HandleFunc("DELETE /polls/{id}", p.handleDelete)
	mux.HandleFunc("POST /polls/{id}/vote", p.handleVote)
	mux.HandleFunc("POST /polls/{id}/answer", p.handleAnswer)
	mux.HandleFunc("POST /polls/{id}/image", p.handleQuestionImage)
	mux.HandleFunc("POST /polls/{id}/options", p.handleAddOption)
	// Nested under the poll: a bare /polls/options/{oid} DELETE would ambiguously
	// overlap reactions' generic DELETE /{type}/{id}/reaction in ServeMux.
	mux.HandleFunc("PATCH /polls/{id}/options/{oid}", p.handleUpdateOption)
	mux.HandleFunc("DELETE /polls/{id}/options/{oid}", p.handleDeleteOption)
	mux.HandleFunc("POST /polls/options/{oid}/image", p.handleOptionImage)
}

// optionPatch is the add/update body; pointers so an absent field is untouched.
type optionPatch struct {
	Label    *string `json:"label"`
	ImageURL *string `json:"image_url"`
	Position *int    `json:"position"`
}

// handleAddOption appends an option to an existing multiple_choice poll.
// Position defaults to end-of-list. PollWrite-gated.
func (p *polls) handleAddOption(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, p.rt.perms.PollWrite); err != nil {
		writeErr(w, err)
		return
	}
	id := req.PathValue("id")
	if !uuidRe.MatchString(id) {
		writeErr(w, ErrNotFound)
		return
	}
	var in optionPatch
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.Label == nil || strings.TrimSpace(*in.Label) == "" {
		writeErr(w, badRequest("label is required"))
		return
	}
	var o pollOption
	err := p.s.pool.QueryRow(req.Context(), `INSERT INTO `+p.s.t.pollOptions+`
		(question_id, label, image_url, position)
		SELECT q.id, $2, $3, COALESCE($4, (SELECT COALESCE(MAX(position)+1, 0) FROM `+p.s.t.pollOptions+` WHERE question_id = q.id))
		FROM `+p.s.t.pollQuestions+` q WHERE q.id = $1 AND q.tenant_id = $5 AND q.deleted_at IS NULL AND q.kind = '`+PollMultipleChoice+`'
		RETURNING id::text, label, coalesce(image_url,''), position, vote_count`,
		id, strings.TrimSpace(*in.Label), in.ImageURL, in.Position, p.s.tenant).
		Scan(&o.ID, &o.Label, &o.ImageURL, &o.Position, &o.VoteCount)
	if errors.Is(err, pgx.ErrNoRows) { // poll missing/deleted, or free_text
		var kind string
		if p.s.pool.QueryRow(req.Context(), `SELECT kind FROM `+p.s.t.pollQuestions+` WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`, id, p.s.tenant).Scan(&kind) == nil && kind == PollFreeText {
			writeErr(w, badRequest("a free-text poll takes no options"))
			return
		}
		writeErr(w, ErrNotFound)
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	o.ImageURL = p.rt.absMediaURL(o.ImageURL)
	writeJSON(w, http.StatusCreated, o)
}

// handleUpdateOption edits an option's label/image/position. PollWrite-gated.
func (p *polls) handleUpdateOption(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, p.rt.perms.PollWrite); err != nil {
		writeErr(w, err)
		return
	}
	pollID, oid := req.PathValue("id"), req.PathValue("oid")
	if !uuidRe.MatchString(pollID) || !uuidRe.MatchString(oid) {
		writeErr(w, ErrNotFound)
		return
	}
	var in optionPatch
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.Label != nil {
		l := strings.TrimSpace(*in.Label)
		if l == "" {
			writeErr(w, badRequest("label cannot be blank"))
			return
		}
		in.Label = &l
	}
	var o pollOption
	err := p.s.pool.QueryRow(req.Context(), `UPDATE `+p.s.t.pollOptions+`
		SET label = COALESCE($2, label), image_url = COALESCE($3, image_url), position = COALESCE($4, position)
		WHERE id = $1 AND question_id = $5 AND `+p.ownsQuestion("question_id", 6)+`
		RETURNING id::text, label, coalesce(image_url,''), position, vote_count`,
		oid, in.Label, in.ImageURL, in.Position, pollID, p.s.tenant).
		Scan(&o.ID, &o.Label, &o.ImageURL, &o.Position, &o.VoteCount)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, ErrNotFound)
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	o.ImageURL = p.rt.absMediaURL(o.ImageURL)
	writeJSON(w, http.StatusOK, o)
}

// handleDeleteOption removes an option (its votes cascade). Refuses to shrink a
// poll below 2 options — that would break voting. PollWrite-gated.
func (p *polls) handleDeleteOption(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, p.rt.perms.PollWrite); err != nil {
		writeErr(w, err)
		return
	}
	pollID, oid := req.PathValue("id"), req.PathValue("oid")
	if !uuidRe.MatchString(pollID) || !uuidRe.MatchString(oid) {
		writeErr(w, ErrNotFound)
		return
	}
	// Single statement: delete only when >= 3 siblings exist, so a poll never
	// shrinks below 2 votable options.
	tag, err := p.s.pool.Exec(req.Context(), `DELETE FROM `+p.s.t.pollOptions+` o
		WHERE o.id = $1 AND o.question_id = $2 AND `+p.ownsQuestion("o.question_id", 3)+` AND (
			SELECT count(*) FROM `+p.s.t.pollOptions+` s WHERE s.question_id = $2) >= 3`, oid, pollID, p.s.tenant)
	if err != nil {
		writeErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		// Missing option and too-few-options both land here; disambiguate.
		var exists bool
		if err := p.s.pool.QueryRow(req.Context(), `SELECT true FROM `+p.s.t.pollOptions+`
			WHERE id = $1 AND question_id = $2 AND `+p.ownsQuestion("question_id", 3), oid, pollID, p.s.tenant).Scan(&exists); err == nil && exists {
			writeErr(w, badRequest("a poll needs at least 2 options"))
			return
		}
		writeErr(w, ErrNotFound)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// handleQuestionImage uploads a question-level image and stores its public URL.
// PollWrite-gated.
func (p *polls) handleQuestionImage(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, p.rt.perms.PollWrite); err != nil {
		writeErr(w, err)
		return
	}
	id := req.PathValue("id")
	if !uuidRe.MatchString(id) {
		writeErr(w, ErrNotFound)
		return
	}
	data, ct, ext, err := readUpload(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	url, err := p.rt.media.Put(req.Context(), "polls/"+id+"."+ext, data, ct)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Remember the previous image so a replace under a different key (extension
	// changed) can drop the old object instead of orphaning it. Best-effort.
	var prev *string
	_ = p.s.pool.QueryRow(req.Context(), `SELECT image_url FROM `+p.s.t.pollQuestions+`
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`, id, p.s.tenant).Scan(&prev)
	tag, err := p.s.pool.Exec(req.Context(), `UPDATE `+p.s.t.pollQuestions+`
		SET image_url = $2, updated_at = now() WHERE id = $1 AND tenant_id = $3 AND deleted_at IS NULL`, id, url, p.s.tenant)
	if err != nil {
		writeErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, ErrNotFound)
		return
	}
	if prev != nil && *prev != url {
		p.rt.deleteMediaByURL(req.Context(), *prev)
	}
	writeJSON(w, http.StatusOK, map[string]string{"image_url": url})
}

// handleOptionImage uploads an option image to the media store and
// stores the resulting public URL on the option. PollWrite-gated.
func (p *polls) handleOptionImage(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, p.rt.perms.PollWrite); err != nil {
		writeErr(w, err)
		return
	}
	oid := req.PathValue("oid")
	if !uuidRe.MatchString(oid) {
		writeErr(w, ErrNotFound)
		return
	}
	data, ct, ext, err := readUpload(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	url, err := p.rt.media.Put(req.Context(), "polls/options/"+oid+"."+ext, data, ct)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Best-effort old-object cleanup on a key-changing replace (see question image).
	var prev *string
	_ = p.s.pool.QueryRow(req.Context(), `SELECT image_url FROM `+p.s.t.pollOptions+` WHERE id = $1 AND `+p.ownsQuestion("question_id", 2), oid, p.s.tenant).Scan(&prev)
	tag, err := p.s.pool.Exec(req.Context(), `UPDATE `+p.s.t.pollOptions+` SET image_url = $2 WHERE id = $1 AND `+p.ownsQuestion("question_id", 3), oid, url, p.s.tenant)
	if err != nil {
		writeErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, ErrNotFound)
		return
	}
	if prev != nil && *prev != url {
		p.rt.deleteMediaByURL(req.Context(), *prev)
	}
	writeJSON(w, http.StatusOK, map[string]string{"image_url": url})
}

func (p *polls) handleList(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	limit, offset := parsePage(req)
	views, err := p.list(req.Context(), p.rt.actor(req.Context()), listFilter{
		language: q.Get("language"), month: q.Get("month"), date: q.Get("date"),
		limit: limit, offset: offset,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

// handleAdminList returns ALL non-deleted polls (future/inactive included) for
// the poll-management UI. PollWrite-gated.
func (p *polls) handleAdminList(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, p.rt.perms.PollWrite); err != nil {
		writeErr(w, err)
		return
	}
	q := req.URL.Query()
	limit, offset := parsePage(req)
	views, err := p.list(req.Context(), actor, listFilter{
		language: q.Get("language"), month: q.Get("month"), date: q.Get("date"),
		admin: true, limit: limit, offset: offset,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (p *polls) handleGet(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	v, err := p.get(req.Context(), actor, req.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	// Future/inactive polls are admin-only; hide as 404 (don't leak schedules).
	if !v.IsActive || v.LiveAt.After(time.Now()) {
		if err := p.rt.requirePerm(req.Context(), actor, p.rt.perms.PollWrite); err != nil {
			writeErr(w, ErrNotFound)
			return
		}
	}
	writeJSON(w, http.StatusOK, v)
}

func (p *polls) handleCreate(w http.ResponseWriter, req *http.Request) {
	var in createPollInput
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	v, err := p.create(req.Context(), p.rt.actor(req.Context()), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (p *polls) handleUpdate(w http.ResponseWriter, req *http.Request) {
	var in updatePollInput
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	v, err := p.update(req.Context(), p.rt.actor(req.Context()), req.PathValue("id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (p *polls) handleDelete(w http.ResponseWriter, req *http.Request) {
	if err := p.softDelete(req.Context(), p.rt.actor(req.Context()), req.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (p *polls) handleAnswer(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	v, err := p.answer(req.Context(), p.rt.actor(req.Context()), req.PathValue("id"), in.Text)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (p *polls) handleVote(w http.ResponseWriter, req *http.Request) {
	var in struct {
		OptionID string `json:"option_id"`
	}
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	v, err := p.vote(req.Context(), p.rt.actor(req.Context()), req.PathValue("id"), in.OptionID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
