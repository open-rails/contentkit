package critical_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	ckauth "github.com/open-rails/contentkit/adapters/authkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
)

func authoredArticle(t *testing.T, a *application, author *ck.Client, entity string) catalog.Content {
	t.Helper()
	startPublicationRuntime(t, a)
	in := catalog.ContentInput{Kind: "article", ChannelID: entity, Versions: []catalog.VersionInput{{Languages: []string{"en"}, Text: map[string]catalog.VersionText{"en": {Title: "Moderated thread", Body: "Readable article"}}}}}
	if author == nil {
		author = a.engine.Client()
	}
	item, err := author.PostContents(t.Context(), in)
	must(t, err)
	item, err = author.PostContentPublish(t.Context(), item.ID, catalog.PublishInput{})
	must(t, err)
	awaitEditionPublication(t, a, a.engine.Client(), item.ID, item.Versions[0].ID)
	return item
}

func postComment(t *testing.T, c *ck.Client, id, body string) content.Comment {
	t.Helper()
	comment, err := c.PostContentComments(t.Context(), id, content.CommentInput{Body: body}, ck.PostContentCommentsQuery{})
	must(t, err)
	if comment.ID == "" || comment.Body != body || comment.Moderation != "" {
		t.Fatalf("unexpected accepted comment: %+v", comment)
	}
	return comment
}

func assertCommentVisibility(t *testing.T, c *ck.Client, id, cid string, visible bool) {
	t.Helper()
	comments, err := c.GetContentComments(t.Context(), id, ck.GetContentCommentsQuery{})
	must(t, err)
	found := false
	for _, comment := range comments {
		if comment.ID == cid {
			found = true
		}
	}
	if found != visible {
		t.Fatalf("comment %s visible=%v; want %v: %+v", cid, found, visible, comments)
	}
}

func TestCommentModerationBoundaries(t *testing.T) {
	w := newWorld(t, func(cfg *ck.Config) { cfg.Interactions.Limits.Disabled = true })
	a, sibling := w.apps[0], w.apps[1]
	setDatabaseTime := useModerationDatabaseClock(t, w, a)
	manager, managerToken := w.person(t, 0)
	authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(manager.ID), w.commentManager)
	authtest.GrantRole(t, w.tenant[0], sibling.group, iam.UserSubject(manager.ID), w.commentManager)
	managerClient := a.client(managerToken)
	t.Run("CK-J20-S01_owner_ban_only_blocks_owned_targets", func(t *testing.T) {
		owner, ownerToken := w.person(t, 0)
		writer, token := w.person(t, 0)
		authtest.GrantRole(t, w.tenant[0], a.group, iam.UserSubject(owner.ID), w.contributor)
		mine, other := authoredArticle(t, a, a.client(ownerToken), ""), publishedArticle(t, a, "Other owner")
		client := a.client(token)
		old := postComment(t, client, mine.ID, "Existing contribution stays")
		ban, err := a.client(ownerToken).PutCommentBan(t.Context(), writer.ID, content.BanInput{Reason: "Owner boundary"})
		must(t, err)
		if ban.Scope != content.OwnerScope(owner.ID) || ban.BannedBy != owner.ID {
			t.Fatalf("wrong owner ban: %+v", ban)
		}
		for _, write := range []struct {
			name string
			run  func() error
		}{
			{"new", func() error {
				_, err := client.PostContentComments(t.Context(), mine.ID, content.CommentInput{Body: "blocked"}, ck.PostContentCommentsQuery{})
				return err
			}},
			{"reply", func() error {
				_, err := client.PostContentComments(t.Context(), mine.ID, content.CommentInput{Body: "blocked", ReplyToID: old.ID}, ck.PostContentCommentsQuery{})
				return err
			}},
			{"edit", func() error {
				_, err := client.PatchComment(t.Context(), old.ID, content.CommentEdit{Body: "blocked"})
				return err
			}},
		} {
			t.Run(write.name, func(t *testing.T) { requireError(t, write.run(), 403, "comment_banned") })
		}
		assertCommentVisibility(t, a.client(""), mine.ID, old.ID, true)
		standing, err := client.GetContentCanComment(t.Context(), mine.ID, ck.GetContentCanCommentQuery{})
		must(t, err)
		if standing.CanComment || standing.Ban == nil || standing.Ban.Scope != ban.Scope {
			t.Fatalf("ban not reflected by standing: %+v", standing)
		}
		postComment(t, client, other.ID, "Different owner's thread remains open")
		_, err = client.PostContentFavorite(t.Context(), mine.ID, ck.PostContentFavoriteQuery{})
		must(t, err)
		bans, err := a.client(ownerToken).GetCommentBans(t.Context(), ck.GetCommentBansQuery{})
		must(t, err)
		if len(bans) != 1 || bans[0].UserID != writer.ID {
			t.Fatalf("wrong owner ban list: %+v", bans)
		}
		must(t, a.client(ownerToken).DeleteCommentBan(t.Context(), writer.ID))
		postComment(t, client, mine.ID, "Lift restores commenting")
	})
	t.Run("CK-J03-S03_CK-J20-S02_scoped_moderator_cannot_author_or_cross_entity", func(t *testing.T) {
		_, _, one := ownedChannel(t, w, a, "Moderated channel")
		_, _, two := ownedChannel(t, w, a, "Other channel")
		moderator, moderatorToken := w.person(t, 0)
		group, err := ckauth.ChannelScopeID(*a.cfg.Scope, one.AuthScope)
		must(t, err)
		authtest.GrantRole(t, w.tenant[0], iam.GroupByID(group), iam.UserSubject(moderator.ID), w.channelModerator)
		writer, writerToken := w.person(t, 0)
		client, viewer := a.client(moderatorToken), a.client(writerToken)
		first, second := authoredArticle(t, a, nil, string(one.ID)), authoredArticle(t, a, nil, string(two.ID))
		comment := postComment(t, viewer, first.ID, "Scoped moderation target")
		other := postComment(t, viewer, second.ID, "Unrelated entity comment")
		must(t, client.PostCommentHold(t.Context(), comment.ID))
		assertCommentVisibility(t, a.client(""), first.ID, comment.ID, false)
		assertCommentVisibility(t, viewer, first.ID, comment.ID, true)
		held, err := client.GetChannelHeldComments(t.Context(), string(one.ID), ck.GetChannelHeldCommentsQuery{})
		must(t, err)
		if len(held.Items) != 1 || held.Items[0].ID != comment.ID || held.Items[0].Body != comment.Body {
			t.Fatalf("scoped held queue lost actual comment: %+v", held)
		}
		_, err = client.GetChannelHeldComments(t.Context(), string(two.ID), ck.GetChannelHeldCommentsQuery{})
		requireError(t, err, 403, "forbidden")
		requireError(t, client.PostCommentHold(t.Context(), other.ID), 403, "forbidden")
		assertCommentVisibility(t, a.client(""), second.ID, other.ID, true)
		must(t, client.PostCommentRelease(t.Context(), comment.ID))
		assertCommentVisibility(t, a.client(""), first.ID, comment.ID, true)
		_, err = client.PostContents(t.Context(), catalog.ContentInput{Kind: "article", ChannelID: string(one.ID)})
		requireError(t, err, 403, "forbidden")
		_, err = client.PutChannelCommentBan(t.Context(), string(two.ID), writer.ID, content.BanInput{})
		requireError(t, err, 403, "forbidden")
		ban, err := client.PutChannelCommentBan(t.Context(), string(one.ID), writer.ID, content.BanInput{Reason: "Entity boundary"})
		must(t, err)
		if ban.Scope != content.ChannelScope(string(one.ID)) {
			t.Fatalf("wrong entity ban: %+v", ban)
		}
		standing, err := viewer.GetChannelCanComment(t.Context(), string(one.ID), ck.GetChannelCanCommentQuery{})
		must(t, err)
		if standing.CanComment || standing.Ban == nil {
			t.Fatal("entity standing omitted the active scoped ban")
		}
		_, err = viewer.PostChannelComments(t.Context(), string(one.ID), content.CommentInput{Body: "blocked on entity"})
		requireError(t, err, 403, "comment_banned")
		_, err = viewer.PostContentComments(t.Context(), first.ID, content.CommentInput{Body: "blocked on owned content"}, ck.PostContentCommentsQuery{})
		requireError(t, err, 403, "comment_banned")
		postComment(t, viewer, second.ID, "Other channel still open")
		bans, err := client.GetChannelCommentBans(t.Context(), string(one.ID), ck.GetChannelCommentBansQuery{})
		must(t, err)
		if len(bans) != 1 || bans[0].UserID != writer.ID {
			t.Fatalf("entity ban list mismatch: %+v", bans)
		}
		must(t, client.DeleteChannelCommentBan(t.Context(), string(one.ID), writer.ID))
		standing, err = viewer.GetChannelCanComment(t.Context(), string(one.ID), ck.GetChannelCanCommentQuery{})
		must(t, err)
		if !standing.CanComment {
			t.Fatal("entity standing did not observe the lifted ban")
		}
		postComment(t, viewer, first.ID, "Entity lift takes effect")
	})
	t.Run("CK-J20-S03_global_ban_remains_inside_app", func(t *testing.T) {
		writer, token := w.person(t, 0)
		first, second := publishedArticle(t, a, "Global target"), publishedArticle(t, sibling, "Sibling target")
		_, err := managerClient.PutGlobalCommentBan(t.Context(), writer.ID, content.BanInput{})
		must(t, err)
		_, err = a.client(token).PostContentComments(t.Context(), first.ID, content.CommentInput{Body: "blocked globally"}, ck.PostContentCommentsQuery{})
		requireError(t, err, 403, "comment_banned")
		postComment(t, sibling.client(token), second.ID, "Sibling app is independent")
		ban, err := managerClient.GetGlobalCommentBan(t.Context(), writer.ID)
		must(t, err)
		if ban.Scope != content.ScopeGlobal {
			t.Fatalf("wrong global scope: %+v", ban)
		}
		_, err = sibling.client(managerToken).GetGlobalCommentBan(t.Context(), writer.ID)
		requireError(t, err, 404, "not_found")
		bans, err := managerClient.GetGlobalCommentBans(t.Context(), ck.GetGlobalCommentBansQuery{})
		must(t, err)
		if len(bans) != 1 || bans[0].UserID != writer.ID {
			t.Fatalf("global ban list mismatch: %+v", bans)
		}
		must(t, managerClient.DeleteGlobalCommentBan(t.Context(), writer.ID))
	})
	t.Run("CK-J20-S04_expiry_and_lift_restore_eligibility", func(t *testing.T) {
		writer, token := w.person(t, 0)
		item := publishedArticle(t, a, "Expiry boundary")
		until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		setDatabaseTime(until.Add(-time.Microsecond))
		_, err := managerClient.PutGlobalCommentBan(t.Context(), writer.ID, content.BanInput{Until: &until})
		must(t, err)
		standing, err := a.client(token).GetContentCanComment(t.Context(), item.ID, ck.GetContentCanCommentQuery{})
		must(t, err)
		if standing.CanComment || standing.Ban == nil {
			t.Fatal("future ban was not active")
		}
		// Bans use PostgreSQL's time source. The owned connection resolves
		// now() to the fixture clock, so equality is exercised exactly.
		for _, sample := range []struct {
			name    string
			at      time.Time
			allowed bool
		}{{"before", until.Add(-time.Microsecond), false}, {"at", until, true}, {"after", until.Add(time.Microsecond), true}} {
			t.Run(sample.name, func(t *testing.T) {
				setDatabaseTime(sample.at)
				standing, err := a.client(token).GetContentCanComment(t.Context(), item.ID, ck.GetContentCanCommentQuery{})
				must(t, err)
				if standing.CanComment != sample.allowed {
					t.Fatalf("ban boundary %s: %+v", sample.name, standing)
				}
				_, err = a.client(token).PostContentComments(t.Context(), item.ID, content.CommentInput{Body: "Expiry boundary " + sample.name}, ck.PostContentCommentsQuery{})
				if sample.allowed {
					must(t, err)
				} else {
					requireError(t, err, 403, "comment_banned")
				}
			})
		}
		ban, err := managerClient.GetGlobalCommentBan(t.Context(), writer.ID)
		must(t, err)
		if !ban.Expired {
			t.Fatal("elapsed ban was not reported expired")
		}
		_, err = managerClient.PutGlobalCommentBan(t.Context(), writer.ID, content.BanInput{})
		must(t, err)
		must(t, managerClient.DeleteGlobalCommentBan(t.Context(), writer.ID))
		must(t, managerClient.DeleteGlobalCommentBan(t.Context(), writer.ID))
		postComment(t, a.client(token), item.ID, "Idempotent lift restores writes")
	})
	t.Run("CK-J20-S05_normalized_words_and_networks_enforce_rules", func(t *testing.T) {
		_, token := w.person(t, 0)
		item := publishedArticle(t, a, "Rule normalization")
		admin, client := a.engine.Client(), a.client(token)
		words, err := admin.PostModerationBannedWords(t.Context(), []content.BannedWordInput{{Word: "ＦＯＲＢＩＤＤＥＮ", Action: content.WordReject}, {Word: "Masked", Action: content.WordMask}})
		must(t, err)
		if len(words) != 2 {
			t.Fatalf("rule batch not persisted: %+v", words)
		}
		storedWords, err := admin.GetModerationBannedWords(t.Context())
		must(t, err)
		present := map[string]bool{}
		for _, word := range storedWords {
			present[word.Word] = true
		}
		if len(storedWords) != 2 || !present["forbidden"] || !present["masked"] {
			t.Fatalf("rule list lost persisted words: %+v", storedWords)
		}
		for i, text := range []string{"forbidden", "FORBIDDEN", "for\u200bbidden", "ＦＯＲＢＩＤＤＥＮ"} {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				_, err := client.PostContentComments(t.Context(), item.ID, content.CommentInput{Body: text}, ck.PostContentCommentsQuery{})
				requireError(t, err, 422, "moderation_rejected")
			})
		}
		masked, err := client.PostContentComments(t.Context(), item.ID, content.CommentInput{Body: "A MASKED word"}, ck.PostContentCommentsQuery{})
		must(t, err)
		if masked.Body != "A ****** word" {
			t.Fatalf("mask failed: %q", masked.Body)
		}
		must(t, admin.DeleteModerationBannedWords(t.Context(), []string{"forbidden", "masked"}))
		postComment(t, client, item.ID, "forbidden")
		bans, err := admin.PostModerationIpBans(t.Context(), []content.IPBanInput{{Network: "127.0.0.99/24", Reason: "Normalized subnet"}})
		must(t, err)
		if len(bans) != 1 || bans[0].Network != "127.0.0.0/24" {
			t.Fatalf("CIDR not normalized: %+v", bans)
		}
		storedBans, err := admin.GetModerationIpBans(t.Context())
		must(t, err)
		if len(storedBans) != 1 || storedBans[0].Network != "127.0.0.0/24" || storedBans[0].Reason != "Normalized subnet" {
			t.Fatalf("IP-ban list lost persisted rule: %+v", storedBans)
		}
		_, err = client.PostContentComments(t.Context(), item.ID, content.CommentInput{Body: "banned subnet"}, ck.PostContentCommentsQuery{})
		requireError(t, err, 403, "comment_banned")
		must(t, admin.DeleteModerationIpBans(t.Context(), []string{"127.0.0.45/24"}))
		postComment(t, client, item.ID, "Network lift takes effect")
		_, err = admin.PostModerationIpBans(t.Context(), []content.IPBanInput{{Network: "::ffff:127.0.0.1"}})
		must(t, err)
		_, err = client.PostContentComments(t.Context(), item.ID, content.CommentInput{Body: "mapped address ban"}, ck.PostContentCommentsQuery{})
		requireError(t, err, 403, "comment_banned")
		must(t, admin.DeleteModerationIpBans(t.Context(), []string{"127.0.0.1"}))
		_, err = admin.PostModerationIpBans(t.Context(), []content.IPBanInput{{Network: "192.0.2.1"}, {Network: "invalid"}})
		requireError(t, err, 400, "invalid_request")
		remaining, err := admin.GetModerationIpBans(t.Context())
		must(t, err)
		if len(remaining) != 0 {
			t.Fatalf("failed rule batch partially committed: %+v", remaining)
		}
	})
	t.Run("CK-J20-S06_unauthorized_moderation_preserves_public_and_held_state", func(t *testing.T) {
		_, writer := w.person(t, 0)
		_, outsider := w.person(t, 0)
		item := publishedArticle(t, a, "No authority")
		comment := postComment(t, a.client(writer), item.ID, "Original contribution")
		client := a.client(outsider)
		requireError(t, client.PostCommentHold(t.Context(), comment.ID), 403, "forbidden")
		_, err := client.PostModerationBannedWords(t.Context(), []content.BannedWordInput{{Word: "illicit"}})
		requireError(t, err, 403, "forbidden")
		_, err = client.PostModerationIpBans(t.Context(), []content.IPBanInput{{Network: "127.0.0.1"}})
		requireError(t, err, 403, "forbidden")
		assertCommentVisibility(t, a.client(""), item.ID, comment.ID, true)
		must(t, managerClient.PostCommentHold(t.Context(), comment.ID))
		requireError(t, client.PostCommentRelease(t.Context(), comment.ID), 403, "forbidden")
		assertCommentVisibility(t, a.client(""), item.ID, comment.ID, false)
		held, err := a.engine.Client().GetModerationHeld(t.Context(), ck.GetModerationHeldQuery{Kind: "comment"})
		must(t, err)
		found := false
		var revision int64
		for _, row := range held.Items {
			if row.ID == comment.ID {
				found = row.Body == comment.Body && row.Revision > 0
				revision = row.Revision
			}
		}
		if !found {
			t.Fatal("unauthorized release changed held source state")
		}
		decision := content.ResolveInput{Decision: content.DecisionApprove, Revision: revision}
		_, err = client.PostModerationResolve(t.Context(), "comment", comment.ID, decision)
		requireError(t, err, 403, "forbidden")
		if revision < 2 {
			t.Fatal("hold did not advance the review revision")
		}
		stale := decision
		stale.Revision--
		_, err = managerClient.PostModerationResolve(t.Context(), "comment", comment.ID, stale)
		requireError(t, err, 404, "not_found")
		assertCommentVisibility(t, a.client(""), item.ID, comment.ID, false)
		applied, err := managerClient.PostModerationResolve(t.Context(), "comment", comment.ID, decision)
		must(t, err)
		if applied.Decision != content.DecisionApprove {
			t.Fatalf("review applied a different decision: %+v", applied)
		}
		assertCommentVisibility(t, a.client(""), item.ID, comment.ID, true)
		words, err := a.engine.Client().GetModerationBannedWords(t.Context())
		must(t, err)
		for _, word := range words {
			if strings.Contains(word.Word, "illicit") {
				t.Fatal("unauthorized rule persisted")
			}
		}
	})
}

// Only this owned pool's search path changes the provider clock; AuthKit,
// sibling apps and every other fixture keep the ordinary PostgreSQL clock.
func useModerationDatabaseClock(t *testing.T, w *world, a *application) func(time.Time) {
	t.Helper()
	qs := pgx.Identifier{w.schema}.Sanitize()
	_, err := w.pool.Exec(t.Context(), `CREATE TABLE `+qs+`.critical_clock(instant timestamptz NOT NULL); CREATE FUNCTION `+qs+`.now() RETURNS timestamptz LANGUAGE SQL STABLE AS $$ SELECT instant FROM `+qs+`.critical_clock $$`)
	must(t, err)
	_, err = w.pool.Exec(t.Context(), `INSERT INTO `+qs+`.critical_clock VALUES ($1)`, w.clock.now())
	must(t, err)
	cfg := w.pool.Config().Copy()
	cfg.ConnConfig.RuntimeParams["search_path"] = qs + ",pg_catalog,public"
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	must(t, err)
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.cfg.Database.SkipMigrations = true
	a.deps.Postgres = pool
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		a.server.Close()
		must(t, a.engine.Close(ctx))
		pool.Close()
	})
	return func(at time.Time) {
		_, err := w.pool.Exec(t.Context(), `UPDATE `+qs+`.critical_clock SET instant=$1`, at)
		must(t, err)
		var observed time.Time
		must(t, pool.QueryRow(t.Context(), "SELECT now()").Scan(&observed))
		if !observed.Equal(at) {
			t.Fatalf("database clock control failed: got %s want %s", observed, at)
		}
	}
}
