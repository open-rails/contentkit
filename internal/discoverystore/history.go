package discoverystore

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/readingstore"
	"github.com/open-rails/contentkit/internal/signalstore"
	"github.com/open-rails/contentkit/internal/subject"
)

// History statuses of /me/history.
const (
	historySeen       = "seen"
	historyInProgress = "in_progress"
	historyCompleted  = "completed"
)

// historyStatus is the reading_state predicate (alias r) of a status.
func historyStatus(status string) (string, error) {
	switch status {
	case "", historySeen:
		return "true", nil
	case historyInProgress:
		return "r.best_outcome < " + strconv.Itoa(int(readingstore.OutcomeComplete)), nil
	case historyCompleted:
		return "r.best_outcome = " + strconv.Itoa(int(readingstore.OutcomeComplete)), nil
	}
	return "", fmt.Errorf("%w: status %q is seen, in_progress or completed", ErrInvalid, status)
}

// readIDs pages the content ids subject opened, most recent first, of kind
// (every kind when empty) and status.
func (s *Service) readIDs(ctx context.Context, who uuid.UUID, kind, status string, limit, offset int) ([]string, error) {
	pred, err := historyStatus(status)
	if err != nil {
		return nil, err
	}
	rows, err := s.o.Pool.Query(ctx, `SELECT c.id FROM `+s.t.reading+` r JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = r.content_id::text
 WHERE r.app_id = $2 AND r.subject = $3 AND r.last_seen_at IS NOT NULL AND ($4 = '' OR c.kind = $4) AND `+pred+`
 ORDER BY r.last_seen_at DESC, r.content_id LIMIT $5 OFFSET $6`, s.o.Tenant, s.app(), who, kind, limit, offset)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// History pages the contents the signed-in reader opened, most recent
// first. Current domain, publication, preferences and access rules apply.
func (s *Service) History(ctx context.Context, o discovery.HistoryOptions) (discovery.ContentPage, error) {
	empty := discovery.ContentPage{Contents: []discovery.Card{}}
	if !o.Actor.Member() {
		return empty, fmt.Errorf("%w: history belongs to a signed-in reader", ErrInvalid)
	}
	who, ok := subject.Account(o.Actor.AccountID())
	if !ok {
		return empty, nil
	}
	if _, err := historyStatus(o.Status); err != nil {
		return empty, err
	}
	if o.Kind != "" {
		if _, ok := s.o.Kinds.Kinds[o.Kind]; !ok {
			return empty, fmt.Errorf("%w: kind %q is not configured", ErrInvalid, o.Kind)
		}
	}
	offset := 0
	if o.Cursor != "" {
		n, err := strconv.Atoi(o.Cursor)
		if err != nil || n < 0 {
			return empty, fmt.Errorf("%w: cursor", ErrInvalid)
		}
		offset = n
	}
	limit := pageLimit(o.Limit, 24)
	ids, err := s.readIDs(ctx, who, o.Kind, o.Status, limit+1, offset)
	if err != nil {
		return empty, err
	}
	next := ""
	if len(ids) > limit {
		ids, next = ids[:limit], strconv.Itoa(offset+limit)
	}
	q, err := s.compileKnown(ctx, o.Reader, Filter{Kind: o.Kind})
	if err != nil {
		return empty, err
	}
	picks, err := s.pickAmong(ctx, q, ids)
	if err != nil {
		return empty, err
	}
	cards, err := s.cards(ctx, q, picks, cardOptions{chips: o.CardEntities})
	if err != nil {
		return empty, err
	}
	out := discovery.ContentPage{Contents: cards, NextCursor: next}
	if o.Summary && offset == 0 {
		// Bound the caller's own selection. Large histories are explicitly
		// lower-bound summaries rather than invented totals.
		const summaryLimit = 100_000
		all, err := s.readIDs(ctx, who, o.Kind, o.Status, summaryLimit, 0)
		if err != nil {
			return empty, err
		}
		sort, err := s.parseSort(ctx, discovery.SortNewest, Filter{Kind: o.Kind})
		if err != nil {
			return empty, err
		}
		out.Summary, err = s.listingSummary(ctx, Filter{Kind: o.Kind}, q, sort, &summaryCandidates{visible: all, relaxed: all, truncated: len(all) == summaryLimit})
		if err != nil {
			return empty, err
		}
	}
	return out, nil
}

// subjectsOf are an account's subject and the anonymous ids folded into it.
func (s *Service) subjectsOf(ctx context.Context, account uuid.UUID) ([]uuid.UUID, error) {
	anonymous, err := s.o.Reading.Anonymous(ctx, []uuid.UUID{account})
	if err != nil {
		return nil, err
	}
	return append([]uuid.UUID{account}, anonymous[account]...), nil
}

// ForgetHistory clears the signed-in reader's reading of one content item,
// one kind or everything: their reading state and history, and the activity
// log's rows. Hidden galleries stay hidden.
func (s *Service) ForgetHistory(ctx context.Context, actor access.Actor, kind, contentID string) error {
	if !actor.Member() {
		return fmt.Errorf("%w: history belongs to a signed-in reader", ErrInvalid)
	}
	who, ok := subject.Account(actor.AccountID())
	if !ok {
		return nil
	}
	if kind != "" {
		if _, ok := s.o.Kinds.Kinds[kind]; !ok {
			return fmt.Errorf("%w: kind %q is not configured", ErrInvalid, kind)
		}
	}
	var ids []uuid.UUID
	if contentID != "" {
		kinds, err := s.kindsOf(ctx, []string{contentID})
		if err != nil {
			return err
		}
		k, ok := kinds[contentID]
		if !ok || (kind != "" && kind != k) {
			return ErrNotFound
		}
		ids = []uuid.UUID{uuid.MustParse(contentID)}
	}
	subjects, err := s.subjectsOf(ctx, who)
	if err != nil {
		return err
	}
	if contentID == "" && kind != "" {
		rows, err := s.o.Pool.Query(ctx, `SELECT r.content_id FROM `+s.t.reading+` r JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = r.content_id::text
 WHERE r.app_id = $2 AND r.subject = ANY($3) AND c.kind = $4`, s.o.Tenant, s.app(), subjects, kind)
		if err != nil {
			return err
		}
		if ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil {
			return err
		}
		if ids == nil {
			ids = []uuid.UUID{}
		}
	}
	if s.o.Hub.SignalsConfigured() {
		store := s.o.Hub.Signals()
		if store == nil {
			return ErrSignalsDown
		}
		if err := store.Forget(ctx, s.o.Tenant, subjects, kind, ids); err != nil {
			return err
		}
	}
	return s.o.Reading.Forget(ctx, subjects, ids)
}

func (s *Service) kindsOf(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := s.o.Pool.Query(ctx, `SELECT id, kind FROM `+s.t.items+` WHERE tenant_id = $1 AND id = ANY($2)`, s.o.Tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			return nil, err
		}
		out[id] = kind
	}
	return out, rows.Err()
}

// maxActiveMS bounds one progress report: five active minutes, with slack
// for a report delayed by a hidden tab.
const maxActiveMS = 6 * 60 * 1000

// maxLength bounds a gallery's pages or a video's seconds.
const maxLength = 1 << 20

// maxSurface bounds a surface name.
const maxSurface = 64

// playerOverrunMS is how far a player's timeline may run past the probed
// duration: an audio track a few frames longer.
const playerOverrunMS = 2000

type length struct {
	kind       string
	durationMS int64
}

// lengths are content ids' kinds and their longest version's duration; zero
// when unknown.
func (s *Service) lengths(ctx context.Context, ids []string) (map[string]length, error) {
	rows, err := s.o.Pool.Query(ctx, `SELECT c.id, c.kind, coalesce(max(v.duration_ms), 0) FROM `+s.t.items+` c
 LEFT JOIN `+s.t.versions+` v ON v.tenant_id = c.tenant_id AND v.content_id = c.id WHERE c.tenant_id = $1 AND c.id = ANY($2) GROUP BY c.id, c.kind`, s.o.Tenant, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]length{}
	for rows.Next() {
		var id string
		var l length
		if err := rows.Scan(&id, &l.kind, &l.durationMS); err != nil {
			return nil, err
		}
		out[id] = l
	}
	return out, rows.Err()
}

// RecordActivity logs a browser's opens and progress reports for the
// request's reader. Content that does not exist is skipped; nothing waits
// for ClickHouse.
func (s *Service) RecordActivity(r *http.Request, actor access.Actor, in discovery.Activity) error {
	if !actor.Anonymous && !actor.Member() {
		return auth.ErrForbidden
	}
	if s.o.Activity == nil || s.o.Activity.Write == nil {
		return ErrNoSignals
	}
	if len(in.Events) == 0 || len(in.Events) > discovery.MaxActivityEvents {
		return fmt.Errorf("%w: 1-%d events", ErrInvalid, discovery.MaxActivityEvents)
	}
	base, ok := s.o.Activity.Row(r, actor, in.Language)
	if !ok {
		return fmt.Errorf("%w: a signed-out reader sends its anonymous id in %s", ErrInvalid, subject.Header)
	}
	ids := make([]string, 0, len(in.Events))
	for _, e := range in.Events {
		if err := contentref.ValidateID(e.ContentID); err != nil {
			return fmt.Errorf("%w: content_id: %w", ErrInvalid, err)
		}
		switch e.Type {
		case discovery.ActivityOpen:
			if e.RequestID != "" && uuid.Validate(e.RequestID) != nil || len(e.Surface) > maxSurface || e.Position > 1<<16-1 {
				return fmt.Errorf("%w: an open's request_id is a UUID, its surface at most %d bytes and its position at most 65535", ErrInvalid, maxSurface)
			}
		case discovery.ActivityProgress:
			if e.ActiveMS > maxActiveMS || e.Length > maxLength || e.Furthest > e.Length || e.Current > e.Length {
				return fmt.Errorf("%w: progress is at most %d active ms since the last report, within its length of at most %d", ErrInvalid, maxActiveMS, maxLength)
			}
		default:
			return fmt.Errorf("%w: type %q is open or progress", ErrInvalid, e.Type)
		}
		ids = append(ids, e.ContentID)
	}
	targets, err := s.lengths(r.Context(), ids)
	if err != nil {
		return err
	}
	rows := make([]signalstore.Row, 0, len(in.Events))
	for _, e := range in.Events {
		target, ok := targets[e.ContentID]
		config, configured := s.o.Kinds.Kinds[target.kind]
		if !ok || !configured {
			continue
		}
		kind := target.kind
		if e.Type == discovery.ActivityProgress && config.Presentation == "player" && target.durationMS > 0 {
			// A player's timeline may run up to two seconds past the probed
			// duration (a longer audio track), not further.
			if bound := (target.durationMS + playerOverrunMS + 999) / 1000; int64(max(e.Furthest, e.Current, e.Length)) > bound {
				return fmt.Errorf("%w: progress of %s is past its duration", ErrInvalid, e.ContentID)
			}
		}
		row := base
		row.ContentKind, row.ContentID = kind, uuid.MustParse(e.ContentID)
		switch e.Type {
		case discovery.ActivityOpen:
			row.Action, row.Surface, row.Position = signalstore.ActionOpen, e.Surface, uint16(e.Position)
			if e.RequestID != "" {
				row.RequestID = uuid.MustParse(e.RequestID)
			}
		case discovery.ActivityProgress:
			row.Action, row.ActiveMS, row.Furthest, row.Current, row.Length = signalstore.ActionProgress, e.ActiveMS, e.Furthest, e.Current, e.Length
			row.Unit = signalstore.UnitPage
			if config.Presentation == "player" {
				row.Unit = signalstore.UnitSecond
			}
		}
		rows = append(rows, row)
	}
	s.o.Activity.Add(rows...)
	return nil
}

// LinkAnonymous records that the browser's anonymous id signed in as the
// actor's account; the next analytics run folds its reading in.
func (s *Service) LinkAnonymous(ctx context.Context, actor access.Actor, in discovery.AnonymousLink) error {
	if !actor.Member() {
		return fmt.Errorf("%w: a link needs a signed-in reader", ErrInvalid)
	}
	account, ok := subject.Account(actor.AccountID())
	if !ok {
		return nil
	}
	anonymous, ok := subject.Anonymous(s.app(), in.AnonymousID)
	if !ok {
		return fmt.Errorf("%w: anonymous_id is the browser's anonymous UUID", ErrInvalid)
	}
	return s.o.Reading.Link(ctx, anonymous, account)
}

// SetHidden hides a gallery from every list the reader is served, or shows
// it again, at once. A signed-out reader hides by its anonymous id.
func (s *Service) SetHidden(r *http.Request, actor access.Actor, contentID string, hidden bool) error {
	if err := contentref.ValidateID(contentID); err != nil {
		return fmt.Errorf("%w: content id: %w", ErrInvalid, err)
	}
	if !actor.Anonymous && !actor.Member() {
		return auth.ErrForbidden
	}
	who, ok := subject.FromRequest(s.app(), actor, r)
	if !ok {
		return fmt.Errorf("%w: a signed-out reader sends its anonymous id in %s", ErrInvalid, subject.Header)
	}
	kinds, err := s.kindsOf(r.Context(), []string{contentID})
	if err != nil {
		return err
	}
	kind, found := kinds[contentID]
	if !found {
		return ErrNotFound
	}
	id := uuid.MustParse(contentID)
	if err := s.o.Reading.SetHidden(r.Context(), who, id, hidden); err != nil {
		return err
	}
	if row, ok := s.o.Activity.Row(r, actor, ""); ok {
		row.Action, row.ContentKind, row.ContentID = signalstore.ActionHide, kind, id
		if hidden {
			row.Value = 1
		}
		s.o.Activity.Add(row)
	}
	return nil
}

// subjectKey is a reader's recent-search subject: the account id, else the
// anonymous subject.
func (s *Service) subjectKey(actor access.Actor, anonymousID string) (string, bool) {
	if actor.Member() {
		return actor.AccountID(), true
	}
	who, ok := subject.Of(s.app(), actor, anonymousID)
	return who.String(), ok
}

// Continue pages the reader's galleries and videos started but not
// finished, most recent first, each card with its position: not bounced
// from, finished, hidden or disliked. A signed-out reader's come from its
// anonymous id. The reader's filters and access apply.
func (s *Service) Continue(ctx context.Context, o discovery.ContinueOptions) (discovery.ContentPage, error) {
	empty := discovery.ContentPage{Contents: []discovery.Card{}}
	if !o.Actor.Anonymous && !o.Actor.Member() {
		return empty, auth.ErrForbidden
	}
	who, known := s.reader(o.Reader)
	if !known {
		return empty, fmt.Errorf("%w: continue reading needs a signed-in reader or an anonymous id", ErrInvalid)
	}
	if o.Kind != "" {
		if _, ok := s.o.Kinds.Kinds[o.Kind]; !ok {
			return empty, fmt.Errorf("%w: kind %q is not configured", ErrInvalid, o.Kind)
		}
	}
	offset := 0
	if o.Cursor != "" {
		n, err := strconv.Atoi(o.Cursor)
		if err != nil || n < 0 {
			return empty, fmt.Errorf("%w: cursor", ErrInvalid)
		}
		offset = n
	}
	limit := pageLimit(o.Limit, 24)
	rows, err := s.o.Pool.Query(ctx, `SELECT c.id FROM `+s.t.reading+` r JOIN `+s.t.items+` c ON c.tenant_id = $1 AND c.id = r.content_id::text
 WHERE r.app_id = $2 AND r.subject = $3 AND r.best_outcome IN ($4, $5) AND NOT r.hidden AND ($6 = '' OR c.kind = $6)
  AND NOT EXISTS (SELECT 1 FROM `+s.t.reactions+` x WHERE x.tenant_id = $1 AND x.content_id = c.id AND x.value < 0
   AND (x.user_id = $7 OR x.anonymous_subject = $3))
 ORDER BY r.last_seen_at DESC, r.content_id LIMIT $8 OFFSET $9`, s.o.Tenant, s.app(), who, readingstore.OutcomeNeutral, readingstore.OutcomeGood,
		o.Kind, who.String(), limit+1, offset)
	if err != nil {
		return empty, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return empty, err
	}
	next := ""
	if len(ids) > limit {
		ids, next = ids[:limit], strconv.Itoa(offset+limit)
	}
	q, err := s.compile(ctx, o.Reader, Filter{Kind: o.Kind})
	if err != nil {
		return empty, err
	}
	picks, err := s.pickAmong(ctx, q, ids)
	if err != nil {
		return empty, err
	}
	cards, err := s.cards(ctx, q, picks, cardOptions{chips: o.CardEntities})
	return discovery.ContentPage{Contents: cards, NextCursor: next}, err
}
