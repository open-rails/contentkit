package contentstore

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	content "github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/httpapi"
)

// polls is the standalone site-wide poll module: admin-authored questions
// with predefined options and one vote per signed-in actor or anonymous IP.
// Polls do not use the content resolver. Staff writes require root:polls:write;
// public voting is available only while the question is open.
type polls struct {
	rt *Runtime
	s  *store
}

func newPolls(rt *Runtime) *polls {
	return &polls{rt: rt, s: rt.store}
}

const (
	maxPollOptions = 10
)

const (
	maxPollQuestionLength = 300
)

const (
	maxPollOptionLength = 200
)

// create inserts a question and its options atomically. Fail-closed on perm.
func (p *polls) create(ctx context.Context, actor access.Actor, in content.PollInput) (content.Poll, error) {
	if err := p.rt.requirePerm(ctx, actor, access.PollsWrite); err != nil {
		return content.Poll{}, err
	}
	in.Question = strings.TrimSpace(in.Question)
	if in.Question == "" {
		return content.Poll{}, badRequest("question is required")
	}
	if utf8.RuneCountInString(in.Question) > maxPollQuestionLength {
		return content.Poll{}, badRequest("question is longer than %d characters", maxPollQuestionLength)
	}
	if in.Language == "" {
		return content.Poll{}, badRequest("language is required")
	}
	if !slices.Contains(p.rt.cfg.Languages, in.Language) {
		return content.Poll{}, badRequest("language %q is not one of the site's languages", in.Language)
	}
	if in.Kind != "" && in.Kind != content.PollMultipleChoice {
		return content.Poll{}, badRequest("only %s polls are supported", content.PollMultipleChoice)
	}
	if len(in.Options) < 2 {
		return content.Poll{}, badRequest("a poll needs at least 2 options")
	}
	if len(in.Options) > maxPollOptions {
		return content.Poll{}, badRequest("a poll has at most %d options", maxPollOptions)
	}
	for i := range in.Options {
		in.Options[i].Label = strings.TrimSpace(in.Options[i].Label)
		if in.Options[i].Label == "" {
			return content.Poll{}, badRequest("option %d label is required", i)
		}
		if utf8.RuneCountInString(in.Options[i].Label) > maxPollOptionLength {
			return content.Poll{}, badRequest("option %d label is longer than %d characters", i, maxPollOptionLength)
		}
	}

	tx, err := p.s.beginMutation(ctx)
	if err != nil {
		return content.Poll{}, err
	}
	defer tx.Rollback(ctx)

	var id string
	err = tx.QueryRow(ctx, `INSERT INTO `+p.s.t.pollQuestions+`
		(id, tenant_id, question, language, live_at, closes_at)
		SELECT $6, $1, $2, $3, COALESCE($4::timestamptz, now()), $5::timestamptz
		WHERE $5::timestamptz IS NULL OR $5::timestamptz > COALESCE($4::timestamptz, now())
		RETURNING id::text`, p.s.tenant, in.Question, in.Language, in.LiveAt, in.ClosesAt, contentref.NewID()).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return content.Poll{}, badRequest("closes_at must be after live_at")
	}
	if err != nil {
		return content.Poll{}, err
	}
	for _, o := range in.Options {
		if _, err := tx.Exec(ctx, `INSERT INTO `+p.s.t.pollOptions+`
			(tenant_id, question_id, label, position) VALUES ($1, $2, $3, $4)`,
			p.s.tenant, id, o.Label, o.Position); err != nil {
			return content.Poll{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return content.Poll{}, err
	}
	return p.get(ctx, actor, id)
}

// update mutates the given fields; nil ones are left as-is via COALESCE, and
// a null closes_at clears it.
func (p *polls) update(ctx context.Context, actor access.Actor, id string, in content.PollUpdate) (content.Poll, error) {
	if err := p.rt.requirePerm(ctx, actor, access.PollsWrite); err != nil {
		return content.Poll{}, err
	}
	if !uuidRe.MatchString(id) {
		return content.Poll{}, ErrNotFound
	}
	if in.Question != nil {
		q := strings.TrimSpace(*in.Question)
		if q == "" {
			return content.Poll{}, badRequest("question cannot be blank")
		}
		if utf8.RuneCountInString(q) > maxPollQuestionLength {
			return content.Poll{}, badRequest("question is longer than %d characters", maxPollQuestionLength)
		}
		in.Question = &q
	}
	tx, err := p.s.beginMutation(ctx)
	if err != nil {
		return content.Poll{}, err
	}
	defer tx.Rollback(ctx)
	var liveAt time.Time
	var closesAt *time.Time
	err = tx.QueryRow(ctx, `SELECT live_at, closes_at FROM `+p.s.t.pollQuestions+`
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL FOR NO KEY UPDATE`, id, p.s.tenant).Scan(&liveAt, &closesAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return content.Poll{}, ErrNotFound
	}
	if err != nil {
		return content.Poll{}, err
	}
	if in.LiveAt != nil {
		liveAt = *in.LiveAt
	}
	if in.ClosesAt.Set {
		closesAt = in.ClosesAt.Value
	}
	if (in.LiveAt != nil || in.ClosesAt.Set) && closesAt != nil && !closesAt.After(liveAt) {
		return content.Poll{}, badRequest("closes_at must be after live_at")
	}
	_, err = tx.Exec(ctx, `UPDATE `+p.s.t.pollQuestions+`
		SET question = COALESCE($2, question), is_active = COALESCE($3, is_active),
		    live_at = COALESCE($4, live_at), closes_at = CASE WHEN $7 THEN $6 ELSE closes_at END, updated_at = now()
		WHERE id = $1 AND tenant_id = $5 AND deleted_at IS NULL`, id, in.Question, in.IsActive, in.LiveAt, p.s.tenant, in.ClosesAt.Value, in.ClosesAt.Set)
	if err != nil {
		return content.Poll{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return content.Poll{}, err
	}
	return p.get(ctx, actor, id)
}

// softDelete flags deleted_at; the row (and its votes) stay for history, and
// its media retirement intent commits with the tombstone. The existing
// content-deletion sweep erases its files; history remains after failures.
func (p *polls) softDelete(ctx context.Context, actor access.Actor, id string) error {
	if err := p.rt.requirePerm(ctx, actor, access.PollsWrite); err != nil {
		return err
	}
	if !uuidRe.MatchString(id) {
		return ErrNotFound
	}
	tx, err := p.s.beginMutation(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `UPDATE `+p.s.t.pollQuestions+`
		SET deleted_at = now(), updated_at = now() WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
		RETURNING id::text`, id, p.s.tenant).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := p.rt.deleteMediaTx(ctx, tx, pollFolder, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// listFilter narrows list: language, a month ("2006-01") or day ("2006-01-02")
// live_at window, and admin (include future/inactive polls; root:polls:write-gated by
// the caller).
type listFilter struct {
	language, month, date string
	// state and query narrow the staff list.
	state, query  string
	admin         bool
	limit, offset int
}

// pollStates are the staff list's state filters.
var pollStates = map[string]string{
	"live":      `is_active AND live_at <= now() AND (closes_at IS NULL OR closes_at > now())`,
	"scheduled": `is_active AND live_at > now()`,
	"closed":    `is_active AND live_at <= now() AND closes_at <= now()`,
	"inactive":  `NOT is_active`,
}

// list returns non-deleted polls newest-live-first with options + caller vote.
// Public mode never leaks future polls (live_at <= now). The default view (no
// month/date window) serves only the active poll(s); browsing a specific
// month/date archive returns all live polls in that window regardless of
// is_active, so historical polls stay readable once a newer poll becomes active.
// Voting remains gated on is_active elsewhere (see vote()).
func (p *polls) list(ctx context.Context, actor access.Actor, f listFilter) ([]content.Poll, error) {
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
	if f.state != "" {
		cond, ok := pollStates[f.state]
		if !ok {
			return nil, badRequest("state must be live, scheduled, closed or inactive")
		}
		sql += ` AND ` + cond
	}
	if q := strings.TrimSpace(f.query); q != "" {
		sql += ` AND strpos(lower(question), lower(` + arg(q) + `)) > 0`
	}
	sql += ` ORDER BY live_at DESC, id DESC LIMIT ` + arg(f.limit) + ` OFFSET ` + arg(f.offset)

	rows, err := p.s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var views []content.Poll
	for rows.Next() {
		v, _, err := p.scan(rows)
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
		views = []content.Poll{}
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

const pollCols = `id::text, question, language, is_active, coalesce(image_name,''), live_at, closes_at, live_at > now(), closes_at IS NOT NULL AND closes_at <= now()`

// scan reads pollCols and derives Closed by the database clock, as open
// does, and whether live_at is still ahead. ImageURL holds the image name
// until attach resolves it.
func (p *polls) scan(row pgx.Row) (content.Poll, bool, error) {
	v := content.Poll{Kind: content.PollMultipleChoice, Options: []content.PollOption{}}
	var pastClose, scheduled bool
	if err := row.Scan(&v.ID, &v.Question, &v.Language, &v.IsActive, &v.ImageURL, &v.LiveAt, &v.ClosesAt, &scheduled, &pastClose); err != nil {
		return content.Poll{}, false, err
	}
	v.Closed = !v.IsActive || pastClose
	return v, scheduled, nil
}

// get returns one non-deleted poll (any state) with options + caller vote; the
// HTTP layer live-gates public access.
func (p *polls) get(ctx context.Context, actor access.Actor, id string) (content.Poll, error) {
	v, _, err := p.getScheduled(ctx, actor, id)
	return v, err
}

// getScheduled is get, and whether the poll's live_at is still ahead.
func (p *polls) getScheduled(ctx context.Context, actor access.Actor, id string) (content.Poll, bool, error) {
	if !uuidRe.MatchString(id) {
		return content.Poll{}, false, ErrNotFound
	}
	v, scheduled, err := p.scan(p.s.pool.QueryRow(ctx, `SELECT `+pollCols+` FROM `+p.s.t.pollQuestions+` WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`, id, p.s.tenant))
	if errors.Is(err, pgx.ErrNoRows) {
		return content.Poll{}, false, ErrNotFound
	}
	if err != nil {
		return content.Poll{}, false, err
	}
	views := []content.Poll{v}
	if err := p.attach(ctx, actor, views); err != nil {
		return content.Poll{}, false, err
	}
	return views[0], scheduled, nil
}

// attach batch-loads options + the caller's votes into views, sums total_votes,
// and resolves every image with one lookup.
func (p *polls) attach(ctx context.Context, actor access.Actor, views []content.Poll) error {
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
	set := p.rt.images()
	for i := range views {
		if o := opts[views[i].ID]; o != nil {
			views[i].Options = o
		}
		set.add(pollFolder, views[i].ID, views[i].ImageURL)
		for _, o := range views[i].Options {
			set.add(pollFolder, views[i].ID, o.ImageURL)
		}
	}
	if err := set.resolve(ctx); err != nil {
		return err
	}
	for i := range views {
		views[i].ImageURL, _ = set.url(pollFolder, views[i].ID, views[i].ImageURL)
		for j := range views[i].Options {
			views[i].Options[j].ImageURL, _ = set.url(pollFolder, views[i].ID, views[i].Options[j].ImageURL)
		}
		total := 0
		for j := range views[i].Options {
			total += views[i].Options[j].VoteCount
		}
		views[i].TotalVotes = total
		if oid, ok := votes[views[i].ID]; ok {
			views[i].Voted, views[i].MyOption = true, oid
		}

	}
	return nil
}

// optionsFor batch-loads options keyed by question id (one array-param query).
func (p *polls) optionsFor(ctx context.Context, q querier, ids []string) (map[string][]content.PollOption, error) {
	rows, err := q.Query(ctx, `SELECT question_id::text, id::text, label, coalesce(image_name, ''), position, vote_count
		FROM `+p.s.t.pollOptions+` WHERE tenant_id = $1 AND question_id = ANY($2::uuid[]) ORDER BY question_id, position`, p.s.tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]content.PollOption{}
	for rows.Next() {
		var qid string
		var o content.PollOption
		if err := rows.Scan(&qid, &o.ID, &o.Label, &o.ImageURL, &o.Position, &o.VoteCount); err != nil {
			return nil, err
		}
		out[qid] = append(out[qid], o)
	}
	return out, rows.Err()
}

// votesFor batch-loads the caller's chosen option per question (qid -> optionID).
func (p *polls) votesFor(ctx context.Context, q querier, actor access.Actor, ids []string) (map[string]string, error) {
	userID, ip, ok := reactionKey(actor)
	if !ok {
		return map[string]string{}, nil
	}
	rows, err := q.Query(ctx, `SELECT question_id::text, option_id::text FROM `+p.s.t.pollVotes+`
		WHERE tenant_id = $1 AND question_id = ANY($2::uuid[]) AND `+actorPred(userID, 3), p.s.tenant, ids, actorArg(userID, ip))
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
func (p *polls) vote(ctx context.Context, actor access.Actor, pollID, optionID string) (content.Poll, error) {
	if actor.Account != nil && !actor.Member() {
		return content.Poll{}, errForbidden
	}
	if optionID == "" {
		return content.Poll{}, badRequest("option_id is required")
	}
	if !uuidRe.MatchString(optionID) {
		return content.Poll{}, badRequest("invalid option_id")
	}
	userID, ip, ok := reactionKey(actor)
	if !ok {
		return content.Poll{}, badRequest("cannot identify voter (no user id or ip)")
	}

	tx, err := p.s.beginMutation(ctx)
	if err != nil {
		return content.Poll{}, err
	}
	defer tx.Rollback(ctx)
	if err := p.rt.guardErasedSubject(ctx, tx, viewerID(actor)); err != nil {
		return content.Poll{}, err
	}

	if err := p.open(ctx, tx, pollID); err != nil {
		return content.Poll{}, err
	}
	// Option must belong to this poll.
	var exists bool
	err = tx.QueryRow(ctx, `SELECT true FROM `+p.s.t.pollOptions+`
		WHERE tenant_id = $3 AND id = $1 AND question_id = $2`, optionID, pollID, p.s.tenant).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return content.Poll{}, badRequest("option does not belong to poll")
	}
	if err != nil {
		return content.Poll{}, err
	}

	if userID != "" {
		ip = ""
	}
	// ON CONFLICT DO NOTHING makes a duplicate voter BLOCK then no-op (0 rows) —
	// a bare INSERT would raise 23505, aborting the whole tx (25P02).
	tag, err := tx.Exec(ctx, `INSERT INTO `+p.s.t.pollVotes+`
		(tenant_id, question_id, option_id, user_id, ip) VALUES ($5, $1, $2, $3, $4)`+pollVoteConflict(userID),
		pollID, optionID, nullIf(userID), nullIf(ip), p.s.tenant)
	if err != nil {
		return content.Poll{}, err
	}
	if tag.RowsAffected() == 1 {
		if _, err := tx.Exec(ctx, `UPDATE `+p.s.t.pollOptions+`
			SET vote_count = vote_count + 1 WHERE tenant_id = $2 AND id = $1`, optionID, p.s.tenant); err != nil {
			return content.Poll{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return content.Poll{}, err
	}
	return p.get(ctx, actor, pollID)
}

// open asserts a choice poll of this tenant takes votes: it must exist, be
// live and not soft-deleted (all hidden as 404 so a future poll never leaks),
// and be neither deactivated nor past closes_at. The share lock holds off an
// option delete until the vote commits, and makes a vote that waited on one
// see the option gone.
func (p *polls) open(ctx context.Context, q querier, pollID string) error {
	if !uuidRe.MatchString(pollID) {
		return ErrNotFound
	}
	var active, closed bool
	err := q.QueryRow(ctx, `SELECT is_active, closes_at IS NOT NULL AND closes_at <= now() FROM `+p.s.t.pollQuestions+`
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL AND live_at <= now() FOR SHARE`, pollID, p.s.tenant).Scan(&active, &closed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !active || closed {
		return badRequest("poll is closed")
	}
	return nil
}

// lockChoices waits for votes already in flight before checking the committed tally.
func (p *polls) lockChoices(ctx context.Context, tx pgx.Tx, id string) error {
	if err := tx.QueryRow(ctx, `SELECT 1 FROM `+p.s.t.pollQuestions+` WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL FOR NO KEY UPDATE`, p.s.tenant, id).Scan(new(int)); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var voted bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+p.s.t.pollVotes+` WHERE tenant_id = $1 AND question_id = $2)`, p.s.tenant, id).Scan(&voted); err != nil {
		return err
	}
	if voted {
		return httpError{status: http.StatusConflict, msg: "poll choices cannot change after voting starts"}
	}
	return nil
}

// pollVoteConflict targets the partial unique index for the voter's dedup key.
func pollVoteConflict(userID string) string {
	if userID != "" {
		return ` ON CONFLICT (tenant_id, question_id, user_id) WHERE user_id IS NOT NULL DO NOTHING`
	}
	return ` ON CONFLICT (tenant_id, question_id, ip) WHERE user_id IS NULL AND ip IS NOT NULL DO NOTHING`
}

var pollRoutes = []httpapi.Route[*polls]{
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/polls", Resource: "polls", Auth: httpapi.Public,
		Doc:       "Live polls, newest first.",
		Query:     append(pollFilter, httpapi.Page...),
		Responses: []httpapi.Reply{httpapi.OK([]content.Poll{})}},
		Serve: httpapi.H((*polls).handleList)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/polls/admin", Resource: "polls", Auth: httpapi.Staff, Perm: string(access.PollsWrite),
		Doc: "Every poll, scheduled and inactive ones included.",
		Query: append(append(slices.Clone(pollFilter), httpapi.Text("state", "only live, scheduled, closed or inactive polls"),
			httpapi.Text("q", "only polls whose question contains this, ignoring case")), httpapi.Page...),
		Responses: []httpapi.Reply{httpapi.OK([]content.Poll{})}},
		Serve: httpapi.H((*polls).handleAdminList)},
	{Spec: httpapi.Spec{Method: httpapi.GET, Path: "/polls/{id}", Resource: "polls", Auth: httpapi.Public,
		Doc:       "A poll with the caller's vote; a scheduled poll only for root:polls:write holders.",
		Responses: []httpapi.Reply{httpapi.OK(content.Poll{})}, Errors: []string{content.CodeNotFound}},
		Serve: httpapi.H((*polls).handleGet)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/polls", Resource: "polls", Auth: httpapi.Staff, Perm: string(access.PollsWrite),
		Doc:       "Creates a poll with at least two predefined options; only multiple_choice is supported.",
		Request:   content.PollInput{},
		Responses: []httpapi.Reply{httpapi.Created(content.Poll{})}, Errors: []string{content.CodeInvalidRequest}},
		Serve: httpapi.H((*polls).handleCreate)},
	{Spec: httpapi.Spec{Method: httpapi.PATCH, Path: "/polls/{id}", Resource: "polls", Auth: httpapi.Staff, Perm: string(access.PollsWrite),
		Doc:       "Updates a poll's given fields; closes_at null reopens it until deactivated.",
		Request:   content.PollUpdate{},
		Responses: []httpapi.Reply{httpapi.OK(content.Poll{})}, Errors: []string{content.CodeInvalidRequest, content.CodeNotFound}},
		Serve: httpapi.H((*polls).handleUpdate)},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/polls/{id}", Resource: "polls", Auth: httpapi.Staff, Perm: string(access.PollsWrite),
		Doc:       "Deletes a poll and its media folder.",
		Responses: []httpapi.Reply{httpapi.NoContent}, Errors: []string{content.CodeNotFound}},
		Serve: httpapi.H((*polls).handleDelete)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/polls/{id}/vote", Resource: "polls", Auth: httpapi.Public,
		Doc:       "Votes for an option of an open multiple-choice poll, signed out where Config.anonymous.votes allows it; a vote is final, and voting again changes nothing.",
		Request:   content.PollVote{},
		Responses: []httpapi.Reply{httpapi.OK(content.Poll{})}, Errors: []string{content.CodeForbidden, content.CodeNotFound, content.CodeRateLimited, content.CodeUnauthorized}},
		Serve: httpapi.H((*polls).handleVote)},
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/polls/{id}/image", Resource: "polls", Auth: httpapi.Staff, Perm: string(access.PollsWrite),
		Doc:       "Sets the question image to an inline image uploaded to the poll's media folder; \"\" clears it.",
		Request:   content.ImageInput{},
		Responses: []httpapi.Reply{httpapi.OK(content.PollImage{})}, Errors: []string{content.CodeNotConfigured, content.CodeNotFound}},
		Serve: httpapi.H((*polls).handleQuestionImage)},
	{Spec: httpapi.Spec{Method: httpapi.POST, Path: "/polls/{id}/options", Resource: "polls", Auth: httpapi.Staff, Perm: string(access.PollsWrite),
		Doc:       "Before voting starts, adds an option to a multiple-choice poll, at the end unless position is given.",
		Request:   content.PollOptionPatch{},
		Responses: []httpapi.Reply{httpapi.Created(content.PollOption{})}, Errors: []string{content.CodeConflict, content.CodeInvalidRequest, content.CodeNotFound}},
		Serve: httpapi.H((*polls).handleAddOption)},
	// Nested under the poll: a bare /polls/options/{oid} DELETE would ambiguously
	// overlap reactions' generic DELETE /{kind}/{id}/reaction in ServeMux.
	{Spec: httpapi.Spec{Method: httpapi.PATCH, Path: "/polls/{id}/options/{oid}", Resource: "polls", Auth: httpapi.Staff, Perm: string(access.PollsWrite),
		Doc:       "Before voting starts, updates an option's label or position.",
		Request:   content.PollOptionPatch{},
		Responses: []httpapi.Reply{httpapi.OK(content.PollOption{})}, Errors: []string{content.CodeConflict, content.CodeInvalidRequest, content.CodeNotConfigured, content.CodeNotFound}},
		Serve: httpapi.H((*polls).handleUpdateOption)},
	{Spec: httpapi.Spec{Method: httpapi.DELETE, Path: "/polls/{id}/options/{oid}", Resource: "polls", Auth: httpapi.Staff, Perm: string(access.PollsWrite),
		Doc:       "Before voting starts, removes an option; a poll keeps at least two.",
		Responses: []httpapi.Reply{httpapi.NoContent}, Errors: []string{content.CodeConflict, content.CodeInvalidRequest, content.CodeNotFound}},
		Serve: httpapi.H((*polls).handleDeleteOption)},
	{Spec: httpapi.Spec{Method: httpapi.PUT, Path: "/polls/{id}/options/{oid}/image", Resource: "polls", Auth: httpapi.Staff, Perm: string(access.PollsWrite),
		Doc:       "Before voting starts, sets an option's image to an inline image uploaded to the poll's media folder; \"\" clears it.",
		Request:   content.ImageInput{},
		Responses: []httpapi.Reply{httpapi.OK(content.PollImage{})}, Errors: []string{content.CodeConflict, content.CodeNotConfigured, content.CodeNotFound}},
		Serve: httpapi.H((*polls).handleOptionImage)},
}

var pollFilter = []httpapi.Param{
	httpapi.Text("language", "only polls in this language"),
	httpapi.Text("month", "only polls live in this month, YYYY-MM"),
	httpapi.Text("date", "only polls live on this day, YYYY-MM-DD"),
}

// handleAddOption appends an option to an existing multiple_choice poll.
// Position defaults to end-of-list. root:polls:write-gated.
func (p *polls) handleAddOption(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, access.PollsWrite); err != nil {
		writeErr(w, err)
		return
	}
	id := req.PathValue("id")
	if !uuidRe.MatchString(id) {
		writeErr(w, ErrNotFound)
		return
	}
	var in content.PollOptionPatch
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.Label == nil || strings.TrimSpace(*in.Label) == "" {
		writeErr(w, badRequest("label is required"))
		return
	}
	label := strings.TrimSpace(*in.Label)
	if utf8.RuneCountInString(label) > maxPollOptionLength {
		writeErr(w, badRequest("label is longer than %d characters", maxPollOptionLength))
		return
	}
	ctx := req.Context()
	tx, err := p.s.beginMutation(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)
	if err := p.lockChoices(ctx, tx, id); err != nil {
		writeErr(w, err)
		return
	}
	var options int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+p.s.t.pollOptions+`
		WHERE tenant_id = $1 AND question_id = $2`, p.s.tenant, id).Scan(&options); err != nil {
		writeErr(w, err)
		return
	}
	if options >= maxPollOptions {
		writeErr(w, badRequest("a poll has at most %d options", maxPollOptions))
		return
	}
	var o content.PollOption
	err = tx.QueryRow(ctx, `INSERT INTO `+p.s.t.pollOptions+`
		(tenant_id, question_id, label, position)
		VALUES ($4, $1, $2, COALESCE($3, (SELECT COALESCE(MAX(position)+1, 0) FROM `+p.s.t.pollOptions+` WHERE tenant_id = $4 AND question_id = $1)))
		RETURNING id::text, label, position, vote_count`,
		id, label, in.Position, p.s.tenant).
		Scan(&o.ID, &o.Label, &o.Position, &o.VoteCount)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, o)
}

// handleUpdateOption edits an option's label/position. root:polls:write-gated.
func (p *polls) handleUpdateOption(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, access.PollsWrite); err != nil {
		writeErr(w, err)
		return
	}
	pollID, oid := req.PathValue("id"), req.PathValue("oid")
	if !uuidRe.MatchString(pollID) || !uuidRe.MatchString(oid) {
		writeErr(w, ErrNotFound)
		return
	}
	var in content.PollOptionPatch
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
		if utf8.RuneCountInString(l) > maxPollOptionLength {
			writeErr(w, badRequest("label is longer than %d characters", maxPollOptionLength))
			return
		}
		in.Label = &l
	}
	ctx := req.Context()
	tx, err := p.s.beginMutation(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)
	if err := p.lockChoices(ctx, tx, pollID); err != nil {
		writeErr(w, err)
		return
	}
	var o content.PollOption
	err = tx.QueryRow(ctx, `UPDATE `+p.s.t.pollOptions+`
		SET label = COALESCE($2, label), position = COALESCE($3, position)
		WHERE id = $1 AND question_id = $4 AND tenant_id = $5
 AND EXISTS (SELECT 1 FROM `+p.s.t.pollQuestions+` q WHERE q.tenant_id=$5 AND q.id=$4 AND q.deleted_at IS NULL)
		RETURNING id::text, question_id::text, label, coalesce(image_name,''), position, vote_count`,
		oid, in.Label, in.Position, pollID, p.s.tenant).
		Scan(&o.ID, &pollID, &o.Label, &o.ImageURL, &o.Position, &o.VoteCount)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, ErrNotFound)
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	set := p.rt.images()
	set.add(pollFolder, pollID, o.ImageURL)
	if err := set.resolve(req.Context()); err != nil {
		writeErr(w, err)
		return
	}
	o.ImageURL, _ = set.url(pollFolder, pollID, o.ImageURL)
	writeJSON(w, http.StatusOK, o)
}

// handleDeleteOption removes an unvoted option. Refuses to shrink a
// poll below 2 options — that would break voting. root:polls:write-gated.
func (p *polls) handleDeleteOption(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, access.PollsWrite); err != nil {
		writeErr(w, err)
		return
	}
	pollID, oid := req.PathValue("id"), req.PathValue("oid")
	if !uuidRe.MatchString(pollID) || !uuidRe.MatchString(oid) {
		writeErr(w, ErrNotFound)
		return
	}
	if err := p.deleteOption(req.Context(), pollID, oid); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// deleteOption keeps at least two choices and never removes recorded votes.
func (p *polls) deleteOption(ctx context.Context, pollID, oid string) error {
	tx, err := p.s.beginMutation(ctx) // the count must see a delete committed during the lock wait
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := p.lockChoices(ctx, tx, pollID); err != nil {
		return err
	}
	var options int
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(bool_or(id = $3), false) FROM `+p.s.t.pollOptions+`
		WHERE tenant_id = $1 AND question_id = $2`, p.s.tenant, pollID, oid).Scan(&options, &exists); err != nil {
		return err
	}
	switch {
	case !exists:
		return ErrNotFound
	case options < 3:
		return badRequest("a poll needs at least 2 options")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+p.s.t.pollOptions+` WHERE tenant_id = $1 AND id = $2`, p.s.tenant, oid); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// handleQuestionImage sets (or with "" clears) the question image to an
// inline image of the poll's media folder. root:polls:write-gated.
func (p *polls) handleQuestionImage(w http.ResponseWriter, req *http.Request) {
	p.setImage(w, req, `UPDATE `+p.s.t.pollQuestions+` SET image_name = NULLIF($3, ''), updated_at = now()
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`)
}

// handleOptionImage sets (or with "" clears) an option image to an inline
// image of the poll's media folder. root:polls:write-gated.
func (p *polls) handleOptionImage(w http.ResponseWriter, req *http.Request) {
	if !uuidRe.MatchString(req.PathValue("oid")) {
		writeErr(w, ErrNotFound)
		return
	}
	p.setImage(w, req, `UPDATE `+p.s.t.pollOptions+` SET image_name = NULLIF($3, '') WHERE id = $4 AND question_id = $1 AND tenant_id = $2`, req.PathValue("oid"))
}

// setImage runs update with $1 the poll id, $2 the tenant, $3 the image name
// (NULL clears) and then extra.
func (p *polls) setImage(w http.ResponseWriter, req *http.Request, update string, extra ...any) {
	ctx := req.Context()
	if err := p.rt.requirePerm(ctx, p.rt.actor(ctx), access.PollsWrite); err != nil {
		writeErr(w, err)
		return
	}
	id := req.PathValue("id")
	if !uuidRe.MatchString(id) {
		writeErr(w, ErrNotFound)
		return
	}
	name, err := decodeImage(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	tx, err := p.s.beginMutation(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer tx.Rollback(ctx)
	if len(extra) > 0 {
		if err := p.lockChoices(ctx, tx, id); err != nil {
			writeErr(w, err)
			return
		}
	}
	if p.rt.media == nil {
		writeErr(w, errMediaNotConfigured)
		return
	}
	if err := tx.QueryRow(ctx, `SELECT id::text FROM `+p.s.t.pollQuestions+` WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`,
		id, p.s.tenant).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		writeErr(w, err)
		return
	}
	tag, err := tx.Exec(ctx, update, append([]any{id, p.s.tenant, name}, extra...)...)
	if err != nil {
		writeErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, ErrNotFound)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, err)
		return
	}
	set := p.rt.images()
	set.add(pollFolder, id, name)
	if err := set.resolve(ctx); err != nil {
		writeErr(w, err)
		return
	}
	var out content.PollImage
	if url, ok := set.url(pollFolder, id, name); ok {
		out.ImageURL = &url
	}
	writeJSON(w, http.StatusOK, out)
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
// the poll-management UI. root:polls:write-gated.
func (p *polls) handleAdminList(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, access.PollsWrite); err != nil {
		writeErr(w, err)
		return
	}
	q := req.URL.Query()
	limit, offset := parsePage(req)
	views, err := p.list(req.Context(), actor, listFilter{
		language: q.Get("language"), month: q.Get("month"), date: q.Get("date"),
		state: q.Get("state"), query: q.Get("q"), admin: true, limit: limit, offset: offset,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (p *polls) handleGet(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	v, scheduled, err := p.getScheduled(req.Context(), actor, req.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	// Future polls are admin-only; hide as 404 (don't leak schedules).
	if scheduled {
		if err := p.rt.requirePerm(req.Context(), actor, access.PollsWrite); err != nil {
			writeErr(w, ErrNotFound)
			return
		}
	}
	writeJSON(w, http.StatusOK, v)
}

func (p *polls) handleCreate(w http.ResponseWriter, req *http.Request) {
	var in content.PollInput
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
	var in content.PollUpdate
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

func (p *polls) handleVote(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	err := participant(actor, p.rt.anonymous.Votes)
	if err == nil {
		err = p.rt.limit(req.Context(), content.ActionPollVote, actor)
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	var in content.PollVote
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	v, err := p.vote(req.Context(), actor, req.PathValue("id"), in.OptionID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
