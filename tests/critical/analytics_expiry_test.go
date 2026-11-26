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
// recent searches and reactions linked to it.
func (x *analyticsWorld) personal(t *testing.T, who uuid.UUID) [4]int {
	t.Helper()
	var n [4]int
	must(t, x.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM `+x.schema+`.reading_state WHERE app_id = $1 AND subject = $2),
 (SELECT count(*) FROM `+x.schema+`.reading_history WHERE app_id = $1 AND subject = $2),
 (SELECT count(*) FROM `+x.schema+`.content_searches WHERE tenant_id = $3 AND subject = $4),
 (SELECT count(*) FROM `+x.schema+`.content_reactions WHERE tenant_id = $3 AND anonymous_subject = $2)`,
		x.app(), who, x.tenant(), who.String()).Scan(&n[0], &n[1], &n[2], &n[3]))
	return n
}

func TestAnalyticsAnonymousExpiry(t *testing.T) {
	x := newAnalyticsWorld(t)
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
		_, err = x.reader(t, "", idleID).GetSearch(t.Context(), ck.GetSearchQuery{Q: "Expiring", Kind: "article"})
		must(t, err)
		x.rows(t, idle, 4)
		x.rows(t, active, 2)
		x.rows(t, linked, 2)
		x.runJob(t)
		if got := x.personal(t, idle); got != [4]int{1, 1, 1, 1} {
			t.Fatalf("before expiry the idle visitor has state, history, searches, reactions = %v", got)
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
		if got := x.personal(t, idle); got != [4]int{} {
			t.Fatalf("expiry kept the idle visitor's state, history, searches, reaction links = %v", got)
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
// record their own activity, so the rule holds.
func TestAnalyticsAnonymousExpiryWithoutClickHouse(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) { cfg.Interactions.Anonymous = content.Anonymous{Reactions: true} })
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
		// One signed-out reaction per client address: the rig's visitors share one.
		_, err := x.reader(t, "", idleID).PostContentLike(t.Context(), liked.ID, ck.PostContentLikeQuery{})
		must(t, err)
		if got := x.personal(t, idle); got != [4]int{1, 0, 1, 1} {
			t.Fatalf("before expiry the idle visitor has state, history, searches, reactions = %v", got)
		}
		before := x.likes(t, liked.ID)
		x.advance(31 * 24 * time.Hour)
		_, err = x.reader(t, "", activeID).GetSearch(t.Context(), ck.GetSearchQuery{Q: "Unlogged", Kind: "article"})
		must(t, err)
		x.runContentJob(t, expiryJob)
		if got := x.personal(t, idle); got != [4]int{} {
			t.Fatalf("expiry kept the idle visitor's state, history, searches, reaction links = %v", got)
		}
		if got := x.personal(t, active); got != [4]int{1, 0, 1, 0} {
			t.Fatalf("expiry touched an active visitor: %v", got)
		}
		if after := x.likes(t, liked.ID); before != 1 || after != before {
			t.Fatalf("likes %d before expiry, %d after", before, after)
		}
	})
}
