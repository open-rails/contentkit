package critical_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
	discovery "github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/internal/subject"
)

const expiryJob = "contentkit_anonymous_expiry"

// likes is a content item's public like count.
func (x *analyticsWorld) likes(t *testing.T, id string) int {
	t.Helper()
	cards, err := x.reader(t, "", "").GetContentCards(t.Context(), ck.GetContentCardsQuery{Ids: []string{id}})
	must(t, err)
	if len(cards.Contents) != 1 || cards.Contents[0].Counts.Likes == nil {
		t.Fatalf("cards = %+v", cards.Contents)
	}
	return *cards.Contents[0].Counts.Likes
}

// personal counts a subject's expirable rows: reading state, history days,
// recent searches, reactions linked to it and feedback answers.
func (x *analyticsWorld) personal(t *testing.T, who uuid.UUID) [5]int {
	t.Helper()
	var n [5]int
	must(t, x.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM `+x.schema+`.reading_state WHERE app_id = $1 AND subject = $2),
 (SELECT count(*) FROM `+x.schema+`.interaction_history WHERE app_id = $1 AND subject = $2),
 (SELECT count(*) FROM `+x.schema+`.content_searches WHERE tenant_id = $3 AND subject = $4),
 (SELECT count(*) FROM `+x.schema+`.content_reactions WHERE tenant_id = $3 AND anonymous_subject = $2),
 (SELECT count(*) FROM `+x.schema+`.content_reaction_feedback WHERE app_id = $1 AND anonymous_subject = $2)`,
		x.app(), who, x.tenant(), who.String()).Scan(&n[0], &n[1], &n[2], &n[3], &n[4]))
	return n
}

// askEveryReaction asks about every like and dislike of an article.
func askEveryReaction(cfg *ck.Config) {
	always := 1.0
	cfg.Interactions.Feedback = discovery.FeedbackConfig{Kinds: map[string]discovery.FeedbackKind{"article": {}}, Like: &always, Dislike: &always}
}

// answers answers a signed-out browser's prompt after its reaction, with free text.
func (x *analyticsWorld) answers(t *testing.T, browser string, id string, reaction discovery.FeedbackReaction) {
	t.Helper()
	c := x.reader(t, "", browser)
	offer, err := c.PostContentFeedback(t.Context(), id, discovery.FeedbackRequest{Reaction: reaction}, ck.PostContentFeedbackQuery{})
	must(t, err)
	if offer.Prompt == nil {
		t.Fatal("the signed-out reaction was not asked about")
	}
	other, text := discovery.ReasonOther, "words that expire"
	_, err = c.PatchContentFeedback(t.Context(), id, discovery.FeedbackInput{Reason: &other, Text: &text}, ck.PatchContentFeedbackQuery{})
	must(t, err)
}

func TestAnalyticsAnonymousExpiry(t *testing.T) {
	x := newAnalyticsWorld(t, askEveryReaction)
	read, liked := x.article(t, "Expiring read", 4), x.article(t, "Expiring like", 4)
	idleID, activeID, linkedID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	idle, _ := subject.Anonymous(x.app(), idleID)
	active, _ := subject.Anonymous(x.app(), activeID)
	linked, _ := subject.Anonymous(x.app(), linkedID)
	user, token := x.person(t, 0)
	account := accountSubject(t, user.ID)
	t.Run("CK-J32-S16_idle_anonymous_visitors_expire", func(t *testing.T) {
		for _, id := range []string{idleID, activeID, linkedID} {
			must(t, x.reader(t, "", id).PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(read.ID), progress(read.ID, 10*time.Second, 4, 4, 4)}}))
		}
		_, err := x.reader(t, "", idleID).PostContentLike(t.Context(), liked.ID, ck.PostContentLikeQuery{})
		must(t, err)
		x.answers(t, idleID, liked.ID, discovery.FeedbackLike)
		_, err = x.reader(t, "", idleID).GetSearch(t.Context(), ck.GetSearchQuery{Q: "Expiring", Kind: "article"})
		must(t, err)
		x.rows(t, idle, 5)
		// The answer is logged as the like with its reason; its text never is.
		rows, err := x.a.deps.ClickHouse.Query(t.Context(), `SELECT countIf(action = 'reaction' AND value = 1 AND reason = 'other'), countIf(position(reason, 'expire') > 0 OR position(query, 'expire') > 0)
 FROM `+x.a.cfg.ClickHouse.Database+`.activity WHERE tenant = ? AND subject = ?`, x.tenant(), idle.String())
		must(t, err)
		var reasoned, texts uint64
		for rows.Next() {
			must(t, rows.Scan(&reasoned, &texts))
		}
		must(t, rows.Close())
		if reasoned != 1 || texts != 0 {
			t.Fatalf("logged answer rows %d, rows with its text %d", reasoned, texts)
		}
		x.rows(t, active, 2)
		x.rows(t, linked, 2)
		x.runJob(t)
		if got := x.personal(t, idle); got != [5]int{1, 1, 1, 1, 1} {
			t.Fatalf("before expiry the idle visitor has state, history, searches, reactions, feedback = %v", got)
		}
		must(t, x.reader(t, token, "").PostMeAnonymousLink(t.Context(), discovery.AnonymousLink{AnonymousID: linkedID}))
		x.runJob(t)
		before := x.likes(t, liked.ID)
		// A month on, only the active visitor came back.
		x.advance(31 * 24 * time.Hour)
		must(t, x.reader(t, "", activeID).PostActivity(t.Context(), discovery.Activity{Events: []discovery.ActivityEvent{open(read.ID)}}))
		x.rows(t, active, 3)
		x.runJob(t)
		x.runContentJob(t, expiryJob)
		if got := x.personal(t, idle); got != [5]int{} {
			t.Fatalf("expiry kept the idle visitor's state, history, searches, reaction links, feedback = %v", got)
		}
		if _, ok := x.state(t, active, read.ID); !ok {
			t.Fatal("expiry deleted an active visitor's reading state")
		}
		if _, ok := x.state(t, account, read.ID); !ok {
			t.Fatal("expiry deleted the reading a linked visitor folded into the account")
		}
		var links int
		must(t, x.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+x.schema+`.anonymous_links WHERE app_id = $1 AND anonymous_id = $2`, x.app(), linked).Scan(&links))
		if links != 1 {
			t.Fatalf("expiry removed the sign-in link (%d)", links)
		}
		if after := x.likes(t, liked.ID); before != 1 || after != before {
			t.Fatalf("likes %d before expiry, %d after", before, after)
		}
	})
}

// Without ClickHouse there is no log; the rows a signed-out visitor writes
// record their own activity, feedback answers included, so the rule holds.
func TestAnalyticsAnonymousExpiryWithoutClickHouse(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) { cfg.Interactions.Anonymous = content.Anonymous{Reactions: true} }, askEveryReaction)
	a := w.apps[0]
	startPublicationRuntime(t, a)
	x := &analyticsWorld{world: w, a: a}
	hidden, liked := x.article(t, "Unlogged hide", 4), x.article(t, "Unlogged like", 4)
	idleID, activeID := uuid.NewString(), uuid.NewString()
	idle, _ := subject.Anonymous(x.app(), idleID)
	active, _ := subject.Anonymous(x.app(), activeID)
	t.Run("CK-J32-S17_without_clickhouse_idle_anonymous_visitors_expire", func(t *testing.T) {
		for _, id := range []string{idleID, activeID} {
			browser := x.reader(t, "", id)
			must(t, browser.PutContentHidden(t.Context(), hidden.ID))
			_, err := browser.GetSearch(t.Context(), ck.GetSearchQuery{Q: "Unlogged", Kind: "article"})
			must(t, err)
		}
		_, err := x.reader(t, "", idleID).PostContentLike(t.Context(), liked.ID, ck.PostContentLikeQuery{})
		must(t, err)
		x.answers(t, idleID, liked.ID, discovery.FeedbackLike)
		_, err = x.reader(t, "", activeID).PostContentDislike(t.Context(), hidden.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		offer, err := x.reader(t, "", activeID).PostContentFeedback(t.Context(), hidden.ID, discovery.FeedbackRequest{Reaction: discovery.FeedbackDislike}, ck.PostContentFeedbackQuery{})
		must(t, err)
		if offer.Prompt == nil {
			t.Fatal("the active visitor's dislike was not asked about")
		}
		if got := x.personal(t, idle); got != [5]int{1, 0, 1, 1, 1} {
			t.Fatalf("before expiry the idle visitor has state, history, searches, reactions, feedback = %v", got)
		}
		before := x.likes(t, liked.ID)
		// A startup run after the jump would expire the active visitor first.
		x.awaitStartupRun(t, expiryJob)
		x.advance(31 * 24 * time.Hour)
		// The active visitor's only activity since is answering its prompt.
		badArt := discovery.ReasonBadArt
		_, err = x.reader(t, "", activeID).PatchContentFeedback(t.Context(), hidden.ID, discovery.FeedbackInput{Reason: &badArt}, ck.PatchContentFeedbackQuery{})
		must(t, err)
		x.runContentJob(t, expiryJob)
		if got := x.personal(t, idle); got != [5]int{} {
			t.Fatalf("expiry kept the idle visitor's state, history, searches, reaction links, feedback = %v", got)
		}
		if got := x.personal(t, active); got != [5]int{1, 0, 1, 1, 1} {
			t.Fatalf("expiry touched a visitor active through its feedback: %v", got)
		}
		if after := x.likes(t, liked.ID); before != 1 || after != before {
			t.Fatalf("likes %d before expiry, %d after", before, after)
		}
	})
}
