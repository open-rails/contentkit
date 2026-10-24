package critical_test

import (
	"testing"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/content"
)

func TestAccountAttributionAndAnonymization(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	t.Run("CK-J24-S01_soft_delete_keeps_contributions_and_hides_attribution", func(t *testing.T) {
		user, token := w.person(t, 0)
		item := publishedArticle(t, a, "soft deletion")
		client := a.client(token)
		comment, err := client.PostContentComments(t.Context(), item.ID, content.CommentInput{Body: "Retained discussion", Language: "en"}, ck.PostContentCommentsQuery{})
		must(t, err)
		favorite, err := client.PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
		must(t, err)
		if favorite.Count == nil || *favorite.Count != 1 {
			t.Fatal("favorite fixture did not record one contribution")
		}
		must(t, a.engine.SetAccountDeleted(t.Context(), user.ID, true))
		got := readComment(t, a, item.ID, comment.ID)
		if got.Body != "Retained discussion" || got.Deleted || got.UserID != "" || got.AnonName != "<Deleted>" || got.Author != nil {
			t.Fatalf("soft deletion changed discussion or exposed attribution: %+v", got)
		}
		state, err := client.GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
		must(t, err)
		if !state.Favorited || state.Count == nil || *state.Count != 1 {
			t.Fatalf("reversible deletion destroyed favorite: %+v", state)
		}
	})
	t.Run("CK-J24-S02_restore_recovers_attribution", func(t *testing.T) {
		user, token := w.person(t, 0)
		item := publishedArticle(t, a, "restoration")
		comment, err := a.client(token).PostContentComments(t.Context(), item.ID, content.CommentInput{Body: "Recoverable attribution"}, ck.PostContentCommentsQuery{})
		must(t, err)
		must(t, a.engine.SetAccountDeleted(t.Context(), user.ID, true))
		must(t, a.engine.SetAccountDeleted(t.Context(), user.ID, false))
		got := readComment(t, a, item.ID, comment.ID)
		if got.UserID != user.ID || got.AnonName != "" || got.Author == nil || got.Body != "Recoverable attribution" {
			t.Fatalf("restore did not recover the original public author: %+v", got)
		}
	})
	t.Run("CK-J24-S03_permanent_anonymization_keeps_counts_without_account_links", func(t *testing.T) {
		user, token := w.person(t, 0)
		item := publishedArticle(t, a, "permanent deletion")
		client := a.client(token)
		comment, err := client.PostContentComments(t.Context(), item.ID, content.CommentInput{Body: "Anonymous contribution"}, ck.PostContentCommentsQuery{})
		must(t, err)
		_, err = client.PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
		must(t, err)
		must(t, a.engine.AnonymizeAccount(t.Context(), user.ID))
		got := readComment(t, a, item.ID, comment.ID)
		if got.UserID != "" || got.Author != nil || got.AnonName != "<Deleted>" || got.Body != "Anonymous contribution" {
			t.Fatalf("permanent erasure lost discussion or retained attribution: %+v", got)
		}
		state, err := a.client("").GetContentFavorite(t.Context(), item.ID, ck.GetContentFavoriteQuery{})
		must(t, err)
		if state.Count == nil || *state.Count != 1 {
			t.Fatalf("anonymization destroyed aggregate contribution: %+v", state)
		}
		must(t, a.engine.SetAccountDeleted(t.Context(), user.ID, false))
		got = readComment(t, a, item.ID, comment.ID)
		if got.UserID != "" || got.Author != nil {
			t.Fatal("restore relinked a permanently anonymized author")
		}
	})
	t.Run("CK-J24-S04_replayed_terminal_event_is_idempotent", func(t *testing.T) {
		user, token := w.person(t, 0)
		item := publishedArticle(t, a, "duplicate terminal event")
		comment, err := a.client(token).PostContentComments(t.Context(), item.ID, content.CommentInput{Body: "Exactly one surviving comment"}, ck.PostContentCommentsQuery{})
		must(t, err)
		must(t, a.engine.AnonymizeAccount(t.Context(), user.ID))
		must(t, a.engine.AnonymizeAccount(t.Context(), user.ID))
		got := readComment(t, a, item.ID, comment.ID)
		if got.Body != "Exactly one surviving comment" || got.UserID != "" {
			t.Fatalf("repeat cleanup changed the surviving contribution: %+v", got)
		}
		all, err := a.client("").GetContentComments(t.Context(), item.ID, ck.GetContentCommentsQuery{})
		must(t, err)
		if len(all) != 1 {
			t.Fatalf("repeat cleanup left %d comments", len(all))
		}
	})
	t.Run("CK-J24-S06_late_write_cannot_reattach_anonymized_account", func(t *testing.T) {
		user, token := w.person(t, 0)
		item := publishedArticle(t, a, "late writer")
		must(t, a.engine.AnonymizeAccount(t.Context(), user.ID))
		_, err := a.client(token).PostContentComments(t.Context(), item.ID, content.CommentInput{Body: "Must never be attached"}, ck.PostContentCommentsQuery{})
		requireError(t, err, 403, "forbidden")
		all, err := a.client("").GetContentComments(t.Context(), item.ID, ck.GetContentCommentsQuery{})
		must(t, err)
		if len(all) != 0 {
			t.Fatal("late writer recreated a comment after erasure")
		}
	})
}

func readComment(t *testing.T, a *application, item, id string) content.Comment {
	t.Helper()
	comments, err := a.client("").GetContentComments(t.Context(), item, ck.GetContentCommentsQuery{})
	must(t, err)
	for _, comment := range comments {
		if comment.ID == id {
			return comment
		}
	}
	t.Fatalf("comment %s disappeared from its public discussion", id)
	return content.Comment{}
}
