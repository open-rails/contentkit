package discoverystore

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/entities"
	entitystore "github.com/open-rails/contentkit/internal/entitystore"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/readerprefs"
	"github.com/open-rails/contentkit/internal/resolve"
	"github.com/open-rails/contentkit/internal/signalstore"
	"github.com/open-rails/contentkit/internal/subject"
	"github.com/open-rails/contentkit/internal/subjectfence"
	"github.com/open-rails/contentkit/kinds"
)

// feedbackSubject is whom a prompt is for: an account, or a signed-out
// browser by its anonymous subject.
type feedbackSubject struct {
	user, anon string
	subject    uuid.UUID
}

func (f feedbackSubject) id() string { return cmp.Or(f.user, f.anon) }

// pred selects the subject's rows; $n binds id().
func (f feedbackSubject) pred(n int) string {
	if f.user != "" {
		return "user_id = $" + strconv.Itoa(n) + "::text::uuid"
	}
	return "anonymous_actor_id = $" + strconv.Itoa(n) + "::text::uuid"
}

// feedbackSubject is the actor's subject; ok is false for one never asked:
// an application, or an account whose id is not a UUID. A signed-out reader
// without an anonymous id is refused.
func (s *Service) feedbackSubject(actor access.Actor, anonymousID string) (feedbackSubject, bool, error) {
	if actor.Member() {
		who, ok := subject.Account(actor.AccountID())
		return feedbackSubject{user: who.String(), subject: who}, ok, nil
	}
	if !actor.Anonymous {
		return feedbackSubject{}, false, nil
	}
	who, ok := subject.Anonymous(s.app(), anonymousID)
	if !ok {
		return feedbackSubject{}, false, fmt.Errorf("%w: a signed-out reader sends its anonymous id in %s", ErrInvalid, subject.Header)
	}
	return feedbackSubject{anon: who.String(), subject: who}, true, nil
}

// feedbackItem is the content a prompt is about and the entities its answers
// can name, as the reader may see them.
type feedbackItem struct {
	ref                   contentref.ContentRef
	cfg                   *discovery.FeedbackKind
	tags, artists, series []discovery.FeedbackEntity
	// culprits are a dislike's not_into choices (s.culprits).
	culprits []discovery.FeedbackEntity
}

func (s *Service) feedbackItem(ctx context.Context, actor access.Actor, id, version, language string) (feedbackItem, error) {
	var out feedbackItem
	if !isUUID(id) || version != "" && !isUUID(version) {
		return out, ErrNotFound
	}
	found, err := s.kindsOf(ctx, []string{id})
	if err != nil {
		return out, err
	}
	kind, ok := found[id]
	if !ok {
		return out, ErrNotFound
	}
	out.ref = contentref.New(s.o.Tenant, kind, id)
	if version != "" {
		out.ref = out.ref.WithVersion(version)
	}
	res, err := resolve.One(ctx, s.o.Resolver, out.ref, actor)
	if err != nil {
		return out, err
	}
	if !res.Visible {
		return out, ErrNotFound
	}
	cfg, ok := s.o.Feedback.Kinds[kind]
	if !ok {
		return out, nil
	}
	out.cfg = &cfg
	if !res.Accessible && !s.o.Kinds.Kinds[kind].LockedView().Shows(kinds.ShowEntities) {
		return out, nil
	}
	var ids []entities.EntityID
	rows, err := s.o.Pool.Query(ctx, `SELECT DISTINCT entity_id FROM `+s.t.memberships+` WHERE tenant_id = $1 AND content_id = $2 AND state = 'active'`, s.o.Tenant, id)
	if err != nil {
		return out, err
	}
	var entity string
	if _, err := pgx.ForEachRow(rows, []any{&entity}, func() error {
		ids = append(ids, entities.EntityID(entity))
		return nil
	}); err != nil || len(ids) == 0 {
		return out, err
	}
	cards, err := s.o.Entities.Cards(ctx, ids, entitystore.CardOptions{Language: language, NamesOnly: true})
	if err != nil {
		return out, err
	}
	for _, c := range cards {
		if c.State != entities.StateActive || c.Internal {
			continue
		}
		e := discovery.FeedbackEntity{ID: string(c.ID), Kind: c.Kind, Slug: c.Slug, Name: c.Name, Path: c.Path}
		switch {
		case slices.Contains(cfg.Tags, c.Kind):
			out.tags = append(out.tags, e)
		case slices.Contains(cfg.Artists, c.Kind):
			out.artists = append(out.artists, e)
		case slices.Contains(cfg.Series, c.Kind):
			out.series = append(out.series, e)
		}
	}
	for _, list := range [][]discovery.FeedbackEntity{out.tags, out.artists, out.series} {
		slices.SortFunc(list, func(a, b discovery.FeedbackEntity) int {
			return cmp.Or(strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), strings.Compare(a.ID, b.ID))
		})
	}
	return out, nil
}

// reasons are the answers a prompt offers: a like names only what the item has.
func (i feedbackItem) reasons(reaction discovery.FeedbackReaction) []discovery.FeedbackReason {
	if reaction == discovery.FeedbackDislike {
		out := []discovery.FeedbackReason{discovery.ReasonBadArt, discovery.ReasonBadStory}
		if len(i.culprits) > 0 {
			out = append(out, discovery.ReasonNotInto)
		}
		return append(out, discovery.ReasonMisleading, discovery.ReasonOther)
	}
	out := []discovery.FeedbackReason{discovery.ReasonArtwork}
	if len(i.artists) > 0 {
		out = append(out, discovery.ReasonArtist)
	}
	out = append(out, discovery.ReasonStory)
	if len(i.series) > 0 {
		out = append(out, discovery.ReasonSeries)
	}
	if len(i.tags) > 0 {
		out = append(out, discovery.ReasonTag)
	}
	return append(out, discovery.ReasonOther)
}

// choices are the entities an answer with reason picks from.
func (i feedbackItem) choices(reason discovery.FeedbackReason) []discovery.FeedbackEntity {
	switch reason {
	case discovery.ReasonTag:
		return i.tags
	case discovery.ReasonArtist:
		return i.artists
	case discovery.ReasonSeries:
		return i.series
	case discovery.ReasonNotInto:
		return i.culprits
	}
	return nil
}

func (i feedbackItem) entity(id string) *discovery.FeedbackEntity {
	for _, list := range [][]discovery.FeedbackEntity{i.culprits, i.tags, i.artists, i.series} {
		for _, e := range list {
			if e.ID == id {
				return &e
			}
		}
	}
	return nil
}

// feedbackRow is a stored prompt.
type feedbackRow struct {
	reaction           discovery.FeedbackReaction
	offeredAt          time.Time
	answeredAt         *time.Time
	reason             *discovery.FeedbackReason
	entity, text, hide *string
	hideTaken          *bool
}

// OfferFeedback records and returns a prompt after the reader's reaction to
// content id, or none: the reader holds no such reaction now, the kind asks
// nothing, sampling passed it over, the reader was asked about the item
// before or had their prompts for the day. Sampling is drawn from the person,
// item and reaction, so repeating a reaction never draws again.
func (s *Service) OfferFeedback(ctx context.Context, actor access.Actor, anonymousID, id, version, language string, in discovery.FeedbackRequest) (discovery.FeedbackOffer, error) {
	var none discovery.FeedbackOffer
	switch in.Reaction {
	case discovery.FeedbackLike, discovery.FeedbackFavorite, discovery.FeedbackDislike:
	default:
		return none, fmt.Errorf("%w: reaction is like, favorite or dislike", ErrInvalid)
	}
	who, ok, err := s.feedbackSubject(actor, anonymousID)
	if err != nil || !ok {
		return none, err
	}
	item, err := s.feedbackItem(ctx, actor, id, version, language)
	if err != nil || item.cfg == nil {
		return none, err
	}
	if reacted, err := s.reacted(ctx, who, item.ref, in.Reaction); err != nil || !reacted {
		return none, err
	}
	if !sampled(s.o.Tenant+"\x00"+who.id()+"\x00"+id+"\x00"+string(in.Reaction), s.o.Feedback.Rate(in.Reaction)) {
		return none, nil
	}
	row := feedbackRow{reaction: in.Reaction, offeredAt: s.now()}
	if in.Reaction == discovery.FeedbackDislike {
		if err := s.culprits(ctx, actor, who, &item); err != nil {
			return none, err
		}
	}
	offered := false
	err = s.feedbackTx(ctx, who, func(tx pgx.Tx) error {
		var asked bool
		var recent int
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.t.feedback+` WHERE app_id = $1::text::uuid AND `+who.pred(2)+` AND content_id = $3::text::uuid),
 (SELECT count(*) FROM `+s.t.feedback+` WHERE app_id = $1::text::uuid AND `+who.pred(2)+` AND offered_at > $4)`,
			s.o.Tenant, who.id(), id, row.offeredAt.Add(-24*time.Hour)).Scan(&asked, &recent); err != nil {
			return err
		}
		if asked || recent >= s.o.Feedback.Budget() {
			return nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.feedback+` (app_id, user_id, anonymous_actor_id, content_id, reaction, offered_at)
 VALUES ($1::text::uuid, $2::text::uuid, $3::text::uuid, $4::text::uuid, $5, $6)`, s.o.Tenant, nullable(who.user), nullable(who.anon), id, string(in.Reaction), row.offeredAt); err != nil {
			return err
		}
		offered = true
		return s.o.Reading.Touch(ctx, tx, who.subject)
	})
	if err != nil || !offered {
		return none, err
	}
	return discovery.FeedbackOffer{Prompt: feedbackView(item, row)}, nil
}

// AnswerFeedback answers the reader's prompt on content id, decides hiding
// the tag a not_into answer names, or both. A later answer replaces an
// earlier one; answered_at keeps the first. A not_into answer naming a
// hideable tag records it as the tag suggested for hiding. An answer is
// logged as its reaction with the reason, never the text, so the day's
// outcome carries it.
func (s *Service) AnswerFeedback(r *http.Request, actor access.Actor, id, language string, in discovery.FeedbackInput) (discovery.ReactionFeedback, error) {
	ctx, anonymousID := r.Context(), r.Header.Get(subject.Header)
	var out discovery.ReactionFeedback
	if in.Reason == nil && in.Hide == nil {
		return out, fmt.Errorf("%w: answer with a reason, a hide decision or both", ErrInvalid)
	}
	if in.Reason == nil && (in.EntityID != nil || in.Text != nil) {
		return out, fmt.Errorf("%w: entity_id and text go with a reason", ErrInvalid)
	}
	who, ok, err := s.feedbackSubject(actor, anonymousID)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, ErrNotFound
	}
	item, err := s.feedbackItem(ctx, actor, id, "", language)
	if err != nil {
		return out, err
	}
	if item.cfg == nil {
		return out, ErrNotFound
	}
	// A prompt's reaction never changes: what depends on it is read before
	// the transaction, which then needs no other connection.
	var offered string
	err = s.o.Pool.QueryRow(ctx, `SELECT reaction FROM `+s.t.feedback+` WHERE app_id = $1::text::uuid AND `+who.pred(2)+` AND content_id = $3::text::uuid`,
		s.o.Tenant, who.id(), id).Scan(&offered)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, fmt.Errorf("%w: no feedback prompt was offered", ErrNotFound)
	}
	if err != nil {
		return out, err
	}
	if discovery.FeedbackReaction(offered) == discovery.FeedbackDislike {
		if err := s.culprits(ctx, actor, who, &item); err != nil {
			return out, err
		}
	}
	var filters map[string]bool
	if in.Hide != nil && *in.Hide && who.user != "" {
		inventory, err := s.filterEntities(ctx)
		if err != nil {
			return out, err
		}
		filters = inventory.allowed()
	}
	var row feedbackRow
	err = s.feedbackTx(ctx, who, func(tx pgx.Tx) error {
		var reaction string
		var reason *string
		err := tx.QueryRow(ctx, `SELECT reaction, offered_at, answered_at, reason, entity_id, text, hide_entity_id, hide_taken FROM `+s.t.feedback+`
 WHERE app_id = $1::text::uuid AND `+who.pred(2)+` AND content_id = $3::text::uuid FOR UPDATE`, s.o.Tenant, who.id(), id).
			Scan(&reaction, &row.offeredAt, &row.answeredAt, &reason, &row.entity, &row.text, &row.hide, &row.hideTaken)
		row.reaction = discovery.FeedbackReaction(reaction)
		if reason != nil {
			r := discovery.FeedbackReason(*reason)
			row.reason = &r
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: no feedback prompt was offered", ErrNotFound)
		}
		if err != nil {
			return err
		}
		if in.Reason != nil {
			if row.reason, row.entity, row.text, err = feedbackAnswer(item, row.reaction, in); err != nil {
				return err
			}
			if row.answeredAt == nil {
				now := s.now()
				row.answeredAt = &now
			}
			if *row.reason == discovery.ReasonNotInto && item.entity(*row.entity).Hideable {
				row.hide = row.entity
			}
		}
		if in.Hide != nil {
			if row.hide == nil {
				return fmt.Errorf("%w: hiding follows a not_into answer naming a tag the filters can hide", ErrInvalid)
			}
			if *in.Hide && who.user != "" {
				if !filters[*row.hide] {
					return fmt.Errorf("%w: the tag is not a content filter on this domain", ErrInvalid)
				}
				if err := s.patchPrefs(ctx, tx, who.user, map[string]string{readerprefs.Key(*row.hide): readerprefs.Hide}); err != nil {
					return err
				}
			}
			row.hideTaken = in.Hide
		}
		reason = nil
		if row.reason != nil {
			r := string(*row.reason)
			reason = &r
		}
		if _, err = tx.Exec(ctx, `UPDATE `+s.t.feedback+` SET reason = $4, entity_id = $5, text = $6, answered_at = $7, hide_entity_id = $8, hide_taken = $9
 WHERE app_id = $1::text::uuid AND `+who.pred(2)+` AND content_id = $3::text::uuid`, s.o.Tenant, who.id(), id, reason, row.entity, row.text, row.answeredAt, row.hide, row.hideTaken); err != nil {
			return err
		}
		return s.o.Reading.Touch(ctx, tx, who.subject)
	})
	if err != nil {
		return out, err
	}
	if in.Reason != nil {
		s.logAnswer(r, actor, item.ref, row.reaction, *row.reason)
	}
	return *feedbackView(item, row), nil
}

// feedbackAnswer validates an answer against what the prompt offers.
func feedbackAnswer(item feedbackItem, reaction discovery.FeedbackReaction, in discovery.FeedbackInput) (reason *discovery.FeedbackReason, entity, text *string, err error) {
	if !slices.Contains(item.reasons(reaction), *in.Reason) {
		return nil, nil, nil, fmt.Errorf("%w: reason %q is not offered", ErrInvalid, *in.Reason)
	}
	choices := item.choices(*in.Reason)
	switch {
	case in.EntityID != nil:
		if !slices.ContainsFunc(choices, func(e discovery.FeedbackEntity) bool { return e.ID == *in.EntityID }) {
			return nil, nil, nil, fmt.Errorf("%w: entity_id is not one of the answer's choices", ErrInvalid)
		}
		entity = in.EntityID
	case *in.Reason == discovery.ReasonTag || *in.Reason == discovery.ReasonNotInto:
		return nil, nil, nil, fmt.Errorf("%w: a %s answer names its tag", ErrInvalid, *in.Reason)
	case len(choices) == 1:
		entity = &choices[0].ID
	}
	if in.Text != nil {
		t := strings.TrimSpace(*in.Text)
		switch {
		case *in.Reason != discovery.ReasonOther:
			return nil, nil, nil, fmt.Errorf("%w: text goes with other", ErrInvalid)
		case !utf8.ValidString(t) || strings.ContainsRune(t, 0):
			return nil, nil, nil, fmt.Errorf("%w: text is not valid text", ErrInvalid)
		case utf8.RuneCountInString(t) > discovery.MaxFeedbackText:
			return nil, nil, nil, fmt.Errorf("%w: text is at most %d characters", ErrInvalid, discovery.MaxFeedbackText)
		case t != "":
			text = &t
		}
	}
	return in.Reason, entity, text, nil
}

func feedbackView(item feedbackItem, row feedbackRow) *discovery.ReactionFeedback {
	dislike := row.reaction == discovery.FeedbackDislike
	list := func(l []discovery.FeedbackEntity) []discovery.FeedbackEntity {
		if l == nil || dislike {
			return []discovery.FeedbackEntity{}
		}
		return l
	}
	v := &discovery.ReactionFeedback{ContentID: item.ref.ContentID, Reaction: row.reaction, Polarity: discovery.PolarityLike,
		Reasons: item.reasons(row.reaction), Tags: list(item.tags), Artists: list(item.artists), Series: list(item.series), HideTaken: row.hideTaken, OfferedAt: row.offeredAt}
	if dislike {
		v.Polarity, v.Tags = discovery.PolarityDislike, append([]discovery.FeedbackEntity{}, item.culprits...)
	}
	if row.hide != nil {
		v.Hide = item.entity(*row.hide)
	}
	if row.reason == nil {
		return v
	}
	v.Answer = &discovery.FeedbackAnswer{Reason: *row.reason, EntityID: row.entity, Text: row.text, AnsweredAt: *row.answeredAt}
	if row.entity == nil {
		return v
	}
	if e := item.entity(*row.entity); e != nil && !dislike {
		action := discovery.FollowUpFollow
		if *row.reason == discovery.ReasonTag {
			action = discovery.FollowUpBrowse
		}
		v.FollowUp = &discovery.FeedbackFollowUp{Action: action, Entity: *e}
	}
	return v
}

// logAnswer logs an answer as its reaction, with the reason.
func (s *Service) logAnswer(r *http.Request, actor access.Actor, ref contentref.ContentRef, reaction discovery.FeedbackReaction, reason discovery.FeedbackReason) {
	content, err := uuid.Parse(ref.ContentID)
	if err != nil {
		return
	}
	row, ok := s.o.Activity.Row(r, actor, "")
	if !ok {
		return
	}
	row.Action, row.Value = signalstore.ActionReaction, 1
	switch reaction {
	case discovery.FeedbackFavorite:
		row.Action = signalstore.ActionFavorite
	case discovery.FeedbackDislike:
		row.Value = -1
	}
	row.ContentKind, row.ContentID, row.Reason = ref.ContentKind, content, string(reason)
	s.o.Activity.Add(row)
}

// feedbackTx runs fn holding the subject's lock, which keeps concurrent
// offers within the budget. An account's is its erasure lock, so an erased
// account is never asked again. fn records a signed-out subject's activity
// with its write, so anonymous expiry sees it.
func (s *Service) feedbackTx(ctx context.Context, who feedbackSubject, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error {
		if who.user != "" {
			if err := subjectfence.Guard(ctx, tx, s.o.Schema, s.o.Tenant, who.user); err != nil {
				if errors.Is(err, subjectfence.ErrErased) {
					return auth.ErrForbidden
				}
				return err
			}
		} else if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, s.o.Schema+"\x1ffeedback\x1f"+s.o.Tenant+"\x1f"+who.anon); err != nil {
			return err
		}
		return fn(tx)
	})
}

// reacted reports whether the subject holds the reaction on ref now: a
// favorite (accounts only), or a like or dislike, which a signed-out
// visitor's row records as its anonymous subject.
func (s *Service) reacted(ctx context.Context, who feedbackSubject, ref contentref.ContentRef, reaction discovery.FeedbackReaction) (bool, error) {
	var ok bool
	if reaction == discovery.FeedbackFavorite {
		if who.user == "" {
			return false, nil
		}
		err := s.o.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.t.favorites+` WHERE app_id = $1::text::uuid AND user_id = $2::text::uuid
 AND content_kind = $3 AND content_id = $4::text::uuid AND content_version_id IS NOT DISTINCT FROM $5::text::uuid AND value = 1)`,
			s.o.Tenant, who.user, ref.ContentKind, ref.ContentID, nullable(ref.Version())).Scan(&ok)
		return ok, err
	}
	value := 1
	if reaction == discovery.FeedbackDislike {
		value = -1
	}
	actor := "user_id = $5"
	if who.user == "" {
		actor = "user_id IS NULL AND anonymous_subject = $5::text::uuid"
	}
	err := s.o.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.t.reactions+` WHERE tenant_id = $1 AND content_kind = $2 AND content_id = $3
 AND content_version_id IS NOT DISTINCT FROM $4::text AND `+actor+` AND value = $6)`,
		s.o.Tenant, ref.ContentKind, ref.ContentID, nullable(ref.Version()), who.id(), value).Scan(&ok)
	return ok, err
}

// culprits fills a dislike's likely culprit tags, at most three: the item's
// tags the domain's content filters offer, which count as polarizing until
// polarization is measured, or that the reader already dislikes. Those the
// reader dislikes come first, then the filters' order. One the filters let
// the reader hide, and the reader still shows, is hideable.
func (s *Service) culprits(ctx context.Context, actor access.Actor, who feedbackSubject, item *feedbackItem) error {
	if len(item.tags) == 0 {
		return nil
	}
	inventory, err := s.filterEntities(ctx)
	if err != nil {
		return err
	}
	allowed := inventory.allowed()
	values := map[string]string{}
	if actor.Member() {
		if values, err = s.prefs(ctx, actor.AccountID()); err != nil {
			return err
		}
	} else if values, err = readerprefs.Overrides(ctx, allowed); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	d, _ := s.o.Kinds.DomainFor(gate.Domain(ctx))
	position, shown := map[string]int{}, map[string]bool{}
	for i, f := range d.Filters {
		id := inventory[f.Entity]
		if _, seen := position[id]; seen || !allowed[id] {
			continue
		}
		visible, set := readerprefs.Stored(values, id)
		position[id], shown[id] = i, visible || !set && f.Visible
	}
	ids := make([]string, len(item.tags))
	for i, t := range item.tags {
		ids[i] = t.ID
	}
	disliked, err := s.dislikedEntities(ctx, who, item.ref.ContentID, ids)
	if err != nil {
		return err
	}
	rank := func(id string) int {
		if p, ok := position[id]; ok {
			return p
		}
		return len(d.Filters)
	}
	var out []discovery.FeedbackEntity
	for _, t := range item.tags {
		_, filtered := position[t.ID]
		if filtered || disliked[t.ID] {
			t.Hideable = filtered && shown[t.ID]
			out = append(out, t)
		}
	}
	slices.SortStableFunc(out, func(a, b discovery.FeedbackEntity) int {
		if disliked[a.ID] != disliked[b.ID] {
			if disliked[a.ID] {
				return -1
			}
			return 1
		}
		return cmp.Compare(rank(a.ID), rank(b.ID))
	})
	item.culprits = out[:min(len(out), 3)]
	return nil
}

// dislikedEntities are those of ids the subject already dislikes on other
// content: the tags of what they dislike, and those their not_into answers
// name.
func (s *Service) dislikedEntities(ctx context.Context, who feedbackSubject, current string, ids []string) (map[string]bool, error) {
	source := `SELECT content_id FROM ` + s.t.reactions + ` WHERE tenant_id = $1 AND user_id IS NULL AND anonymous_subject = $2::text::uuid AND value = -1`
	if who.user != "" {
		source = `SELECT content_id FROM ` + s.t.reactions + ` WHERE tenant_id = $1 AND user_id = $2 AND value = -1`
	}
	rows, err := s.o.Pool.Query(ctx, `SELECT m.entity_id FROM (`+source+`) AS d(id)
 JOIN `+s.t.memberships+` m ON m.tenant_id = $1 AND m.content_id = d.id
 WHERE d.id <> $3 AND m.state = 'active' AND m.entity_id = ANY($4)
 UNION SELECT entity_id FROM `+s.t.feedback+` WHERE app_id = $1::text::uuid AND `+who.pred(2)+`
 AND reason = 'not_into' AND entity_id = ANY($4) AND content_id <> $3::text::uuid`, s.o.Tenant, who.id(), current, ids)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	var id string
	_, err = pgx.ForEachRow(rows, []any{&id}, func() error {
		out[id] = true
		return nil
	})
	return out, err
}

// sampled draws key's share of rate, the same way every time.
func sampled(key string, rate float64) bool {
	if rate <= 0 {
		return false
	}
	if rate >= 1 {
		return true
	}
	sum := sha256.Sum256([]byte(key))
	return float64(binary.BigEndian.Uint64(sum[:8]))/(1<<64) < rate
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
