package critical_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/internal/subject"
	"github.com/open-rails/contentkit/kinds"
)

// feedbackWorld is an app whose articles carry tags, an artist and a series,
// whose domain lets readers hide the furry and gore tags, and which asks
// about every reaction unless a case lowers a rate.
func feedbackWorld(t *testing.T) *world {
	t.Helper()
	return newWorld(t, func(c *ck.Config) {
		c.Interactions.Limits.Disabled = true
		c.Interactions.Anonymous.Reactions = true
		scope := []kinds.MembershipScope{kinds.ContentScope}
		c.Content.EntityKinds["tag"] = kinds.EntityKind{Route: "tags", MembershipScopes: scope}
		c.Content.EntityKinds["artist"] = kinds.EntityKind{Route: "artists", MembershipScopes: scope}
		c.Content.EntityKinds["series"] = kinds.EntityKind{Route: "series", MembershipScopes: scope}
		article := c.Content.Kinds["article"]
		article.Entities = []string{"tag", "artist", "series"}
		c.Content.Kinds["article"] = article
		c.Content.ContentFilters = kinds.ContentFilters{AppliesTo: []string{"article"}, Domains: []kinds.Domain{{Name: "main", Filters: []kinds.ContentFilter{
			{Entity: kinds.EntityRef{Kind: "tag", Slug: "furry"}, Visible: true},
			{Entity: kinds.EntityRef{Kind: "tag", Slug: "gore"}, Visible: true},
			{Entity: kinds.EntityRef{Kind: "tag", Slug: "tentacle"}, Visible: true},
			{Entity: kinds.EntityRef{Kind: "tag", Slug: "horror"}, Visible: true},
		}}}}
		always := 1.0
		c.Interactions.Feedback = discovery.FeedbackConfig{Kinds: map[string]discovery.FeedbackKind{"article": {Tags: []string{"tag"}, Artists: []string{"artist"}, Series: []string{"series"}}},
			Favorite: &always, Like: &always, Dislike: &always}
	})
}

type feedbackFixture struct {
	t                                                      *testing.T
	w                                                      *world
	a                                                      *application
	furry, gore, plain, alice, bob, saga, tentacle, horror entities.Entity
}

func newFeedbackFixture(t *testing.T, w *world, a *application) *feedbackFixture {
	t.Helper()
	made, err := a.engine.Client().PostEntities(t.Context(), []entities.EntityInput{
		{Kind: "tag", Slug: "furry", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Furry"}}},
		{Kind: "tag", Slug: "gore", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Gore"}}},
		{Kind: "tag", Slug: "plain", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Plain"}}},
		{Kind: "artist", Slug: "alice", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Alice"}}},
		{Kind: "artist", Slug: "bob", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Bob"}}},
		{Kind: "series", Slug: "saga", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Saga"}}},
		{Kind: "tag", Slug: "tentacle", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Tentacle"}}},
		{Kind: "tag", Slug: "horror", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Horror"}}},
	})
	must(t, err)
	if len(made) != 8 {
		t.Fatalf("entity fixture incomplete: %+v", made)
	}
	return &feedbackFixture{t: t, w: w, a: a, furry: made[0], gore: made[1], plain: made[2], alice: made[3], bob: made[4], saga: made[5], tentacle: made[6], horror: made[7]}
}

// item publishes an article carrying the entities.
func (f *feedbackFixture) item(title string, members ...entities.Entity) catalog.Content {
	f.t.Helper()
	item := publishedArticle(f.t, f.a, title)
	links := []entities.Membership{}
	for _, e := range members {
		links = append(links, entities.Membership{EntityID: e.ID})
	}
	_, err := f.a.engine.Client().PutContentEntitiesShared(f.t.Context(), item.ID, entities.ContentEntities{Memberships: links}, ck.PutContentEntitiesSharedQuery{})
	must(f.t, err)
	return item
}

func (f *feedbackFixture) ask(c *ck.Client, id string, reaction discovery.FeedbackReaction) *discovery.ReactionFeedback {
	f.t.Helper()
	out, err := c.PostContentFeedback(f.t.Context(), id, discovery.FeedbackRequest{Reaction: reaction}, ck.PostContentFeedbackQuery{})
	must(f.t, err)
	return out.Prompt
}

// browser is a signed-out browser presenting its anonymous id, as the SDK does.
func (f *feedbackFixture) browser(id string) *ck.Client {
	return ck.NewClient(f.a.server.URL+"/content", &http.Client{Transport: readerTransport{test: f.t, w: f.w, anonymous: id}})
}

func (f *feedbackFixture) answer(c *ck.Client, id string, in discovery.FeedbackInput) (discovery.ReactionFeedback, error) {
	return c.PatchContentFeedback(f.t.Context(), id, in, ck.PatchContentFeedbackQuery{})
}

// rows counts the stored prompts matching where (over columns of the table).
func (f *feedbackFixture) rows(where string, args ...any) int {
	f.t.Helper()
	var n int
	must(f.t, f.w.pool.QueryRow(f.t.Context(), `SELECT count(*) FROM `+pgx.Identifier{f.w.schema, "content_reaction_feedback"}.Sanitize()+
		` WHERE app_id = $1::uuid AND `+where, append([]any{f.a.cfg.Scope.AppID.String()}, args...)...).Scan(&n))
	return n
}

func refusedWith(t *testing.T, err error, status int, code string) {
	t.Helper()
	var refused *ck.Error
	if !errors.As(err, &refused) || refused.Status != status || refused.Code != code {
		t.Fatalf("got %v, want %d %s", err, status, code)
	}
}

func reasonPtr(r discovery.FeedbackReason) *discovery.FeedbackReason { return &r }
func strPtr(s string) *string                                        { return &s }
func boolPtr(b bool) *bool                                           { return &b }

func TestReactionFeedback(t *testing.T) {
	w := feedbackWorld(t)
	a := w.apps[0]
	f := newFeedbackFixture(t, w, a)
	// Every item is published before any case moves the clock forward.
	full := f.item("Feedback full item", f.furry, f.plain, f.alice, f.saga)
	bare := f.item("Feedback bare item")
	once := f.item("Feedback asked once", f.plain)
	first, second := f.item("Feedback budget first", f.plain), f.item("Feedback budget second", f.plain)
	likeAnswers := f.item("Feedback like answers", f.furry, f.plain, f.alice, f.saga)
	neverAsked := f.item("Feedback never asked", f.plain)
	dislikeAnswers := f.item("Feedback dislike answers", f.plain, f.bob)
	hideItem := f.item("Feedback hide", f.plain, f.furry)
	earlier, both := f.item("Feedback earlier gore", f.gore), f.item("Feedback both tags", f.furry, f.gore)
	many := f.item("Feedback many tags", f.horror, f.tentacle, f.plain, f.gore, f.furry)
	plainOne, plainTwo := f.item("Feedback plain one", f.plain), f.item("Feedback plain two", f.plain)
	signedOut := f.item("Feedback signed-out hide", f.furry)
	erased := f.item("Feedback erased", f.plain)

	t.Run("CK-J40-S01_offered_only_after_the_readers_own_reaction", func(t *testing.T) {
		_, token := w.person(t, 0)
		reader := a.client(token)
		if p := f.ask(reader, full.ID, discovery.FeedbackLike); p != nil {
			t.Fatalf("a prompt without a like: %+v", p)
		}
		if p := f.ask(reader, full.ID, discovery.FeedbackFavorite); p != nil {
			t.Fatalf("a favorite prompt without a favorite: %+v", p)
		}
		_, err := reader.PostContentLike(t.Context(), full.ID, ck.PostContentLikeQuery{})
		must(t, err)
		if p := f.ask(reader, full.ID, discovery.FeedbackDislike); p != nil {
			t.Fatalf("a dislike prompt while the reader likes it: %+v", p)
		}
		_, err = reader.PostContentFeedback(t.Context(), full.ID, discovery.FeedbackRequest{Reaction: "love"}, ck.PostContentFeedbackQuery{})
		refusedWith(t, err, http.StatusBadRequest, "invalid_request")
		p := f.ask(reader, full.ID, discovery.FeedbackLike)
		if p == nil || p.Polarity != discovery.PolarityLike || p.Reaction != discovery.FeedbackLike || p.Answer != nil || p.Hide != nil || p.FollowUp != nil {
			t.Fatalf("like prompt: %+v", p)
		}
		want := []discovery.FeedbackReason{discovery.ReasonArtwork, discovery.ReasonArtist, discovery.ReasonStory, discovery.ReasonSeries, discovery.ReasonTag, discovery.ReasonOther}
		if !slices.Equal(p.Reasons, want) {
			t.Fatalf("like reasons %v, want %v", p.Reasons, want)
		}
		if len(p.Tags) != 2 || p.Tags[0].ID != string(f.furry.ID) || p.Tags[1].ID != string(f.plain.ID) || len(p.Artists) != 1 || p.Artists[0].Name != "Alice" || len(p.Series) != 1 || p.Series[0].ID != string(f.saga.ID) {
			t.Fatalf("like prompt choices: %+v", p)
		}
		_, err = reader.PostContentLike(t.Context(), bare.ID, ck.PostContentLikeQuery{})
		must(t, err)
		w.clock.set(w.clock.now().Add(25 * time.Hour))
		p = f.ask(reader, bare.ID, discovery.FeedbackLike)
		if p == nil || !slices.Equal(p.Reasons, []discovery.FeedbackReason{discovery.ReasonArtwork, discovery.ReasonStory, discovery.ReasonOther}) {
			t.Fatalf("a like names only what the item has: %+v", p)
		}
		if p := f.ask(f.browser(uuid.NewString()), full.ID, discovery.FeedbackLike); p != nil {
			t.Fatalf("a signed-out prompt without the browser's like: %+v", p)
		}
		_, err = a.client("").PostContentFeedback(t.Context(), full.ID, discovery.FeedbackRequest{Reaction: discovery.FeedbackLike}, ck.PostContentFeedbackQuery{})
		refusedWith(t, err, http.StatusBadRequest, "invalid_request")
		_, err = reader.PostContentFeedback(t.Context(), uuid.NewString(), discovery.FeedbackRequest{Reaction: discovery.FeedbackLike}, ck.PostContentFeedbackQuery{})
		refusedWith(t, err, http.StatusNotFound, "not_found")
	})

	t.Run("CK-J40-S02_never_twice_for_one_item", func(t *testing.T) {
		user, token := w.person(t, 0)
		reader := a.client(token)
		item := once
		_, err := reader.PostContentLike(t.Context(), item.ID, ck.PostContentLikeQuery{})
		must(t, err)
		if f.ask(reader, item.ID, discovery.FeedbackLike) == nil {
			t.Fatal("first like was not asked about")
		}
		w.clock.set(w.clock.now().Add(25 * time.Hour))
		_, err = reader.PostContentFavorite(t.Context(), item.ID, ck.PostContentFavoriteQuery{})
		must(t, err)
		if p := f.ask(reader, item.ID, discovery.FeedbackFavorite); p != nil {
			t.Fatalf("asked twice about one item: %+v", p)
		}
		if p := f.ask(reader, item.ID, discovery.FeedbackLike); p != nil {
			t.Fatalf("a repeated ask offered again: %+v", p)
		}
		if n := f.rows("user_id = $2::uuid AND content_id = $3::uuid", user.ID, item.ID); n != 1 {
			t.Fatalf("%d prompts stored for one item, want 1", n)
		}
	})

	t.Run("CK-J40-S03_one_prompt_a_day_across_likes_and_dislikes", func(t *testing.T) {
		user, token := w.person(t, 0)
		reader := a.client(token)
		_, err := reader.PostContentLike(t.Context(), first.ID, ck.PostContentLikeQuery{})
		must(t, err)
		_, err = reader.PostContentDislike(t.Context(), second.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		if f.ask(reader, first.ID, discovery.FeedbackLike) == nil {
			t.Fatal("the day's first reaction was not asked about")
		}
		w.clock.set(w.clock.now().Add(23 * time.Hour))
		if p := f.ask(reader, second.ID, discovery.FeedbackDislike); p != nil {
			t.Fatalf("a second prompt within 24 hours: %+v", p)
		}
		w.clock.set(w.clock.now().Add(2 * time.Hour))
		if p := f.ask(reader, second.ID, discovery.FeedbackDislike); p == nil || p.Polarity != discovery.PolarityDislike {
			t.Fatalf("the next day's dislike was not asked about: %+v", p)
		}
		if n := f.rows("user_id = $2::uuid", user.ID); n != 2 {
			t.Fatalf("%d prompts recorded, want 2", n)
		}
		// Each signed-out browser has its own budget, keyed by its anonymous id.
		// The rig's browsers share an address, whose reaction names the last.
		one, two := f.browser(uuid.NewString()), f.browser(uuid.NewString())
		for _, b := range []*ck.Client{one, two} {
			_, err = b.PostContentLike(t.Context(), first.ID, ck.PostContentLikeQuery{})
			must(t, err)
			if f.ask(b, first.ID, discovery.FeedbackLike) == nil {
				t.Fatal("a signed-out browser was not asked")
			}
		}
		_, err = one.PostContentLike(t.Context(), first.ID, ck.PostContentLikeQuery{})
		must(t, err)
		if p := f.ask(one, first.ID, discovery.FeedbackLike); p != nil {
			t.Fatalf("one browser asked twice: %+v", p)
		}
	})

	t.Run("CK-J40-S04_like_answers_name_their_entity_and_follow_up", func(t *testing.T) {
		user, token := w.person(t, 0)
		reader := a.client(token)
		item := likeAnswers
		_, err := reader.PostContentLike(t.Context(), item.ID, ck.PostContentLikeQuery{})
		must(t, err)
		if f.ask(reader, item.ID, discovery.FeedbackLike) == nil {
			t.Fatal("like was not asked about")
		}
		_, err = f.answer(reader, item.ID, discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonTag)})
		refusedWith(t, err, http.StatusBadRequest, "invalid_request")
		_, err = f.answer(reader, item.ID, discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonTag), EntityID: strPtr(string(f.alice.ID))})
		refusedWith(t, err, http.StatusBadRequest, "invalid_request")
		_, err = f.answer(reader, item.ID, discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonBadArt)})
		refusedWith(t, err, http.StatusBadRequest, "invalid_request")
		_, err = f.answer(reader, item.ID, discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonArtwork), Text: strPtr("text belongs to other")})
		refusedWith(t, err, http.StatusBadRequest, "invalid_request")
		_, err = f.answer(reader, item.ID, discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonOther), Text: strPtr(strings.Repeat("é", discovery.MaxFeedbackText+1))})
		refusedWith(t, err, http.StatusBadRequest, "invalid_request")
		_, err = f.answer(reader, item.ID, discovery.FeedbackInput{Hide: boolPtr(true)})
		refusedWith(t, err, http.StatusBadRequest, "invalid_request")

		got, err := f.answer(reader, item.ID, discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonTag), EntityID: strPtr(string(f.plain.ID))})
		must(t, err)
		if got.Answer == nil || got.Answer.Reason != discovery.ReasonTag || *got.Answer.EntityID != string(f.plain.ID) ||
			got.FollowUp == nil || got.FollowUp.Action != discovery.FollowUpBrowse || got.FollowUp.Entity.Slug != "plain" {
			t.Fatalf("tag answer: %+v", got)
		}
		first := got.Answer.AnsweredAt
		for _, c := range []struct {
			reason discovery.FeedbackReason
			entity entities.Entity
		}{{discovery.ReasonArtist, f.alice}, {discovery.ReasonSeries, f.saga}} {
			got, err = f.answer(reader, item.ID, discovery.FeedbackInput{Reason: reasonPtr(c.reason)})
			must(t, err)
			if got.Answer.EntityID == nil || *got.Answer.EntityID != string(c.entity.ID) || got.FollowUp == nil || got.FollowUp.Action != discovery.FollowUpFollow || got.FollowUp.Entity.ID != string(c.entity.ID) {
				t.Fatalf("%s answer did not name its only entity: %+v", c.reason, got)
			}
		}
		for _, r := range []discovery.FeedbackReason{discovery.ReasonArtwork, discovery.ReasonStory} {
			got, err = f.answer(reader, item.ID, discovery.FeedbackInput{Reason: reasonPtr(r)})
			must(t, err)
			if got.Answer.Reason != r || got.Answer.EntityID != nil || got.FollowUp != nil {
				t.Fatalf("%s answer: %+v", r, got)
			}
		}
		text := strings.Repeat("é", discovery.MaxFeedbackText)
		got, err = f.answer(reader, item.ID, discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonOther), Text: strPtr("  " + text + " ")})
		must(t, err)
		if got.Answer.Text == nil || *got.Answer.Text != text || !got.Answer.AnsweredAt.Equal(first) {
			t.Fatalf("other answer: %+v", got.Answer)
		}
		if n := f.rows("user_id = $2::uuid AND content_id = $3::uuid AND reason = 'other' AND text = $4 AND answered_at IS NOT NULL", user.ID, item.ID, text); n != 1 {
			t.Fatal("the latest answer and its text were not stored")
		}
		_, err = f.answer(reader, neverAsked.ID, discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonArtwork)})
		refusedWith(t, err, http.StatusNotFound, "not_found")
	})

	t.Run("CK-J40-S05_dislike_answers_and_culprit_tags", func(t *testing.T) {
		user, token := w.person(t, 0)
		reader := a.client(token)
		item := dislikeAnswers
		_, err := reader.PostContentDislike(t.Context(), item.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		p := f.ask(reader, item.ID, discovery.FeedbackDislike)
		if p == nil || !slices.Equal(p.Reasons, []discovery.FeedbackReason{discovery.ReasonBadArt, discovery.ReasonBadStory, discovery.ReasonMisleading, discovery.ReasonOther}) ||
			len(p.Tags) != 0 || len(p.Artists) != 0 || p.Hide != nil || p.HideTaken != nil {
			t.Fatalf("a dislike without culprit tags: %+v", p)
		}
		for _, in := range []discovery.FeedbackInput{
			{Reason: reasonPtr(discovery.ReasonNotInto), EntityID: strPtr(string(f.plain.ID))},
			{Reason: reasonPtr(discovery.ReasonArtwork)},
			{Reason: reasonPtr(discovery.ReasonMisleading), EntityID: strPtr(string(f.plain.ID))},
		} {
			_, err = f.answer(reader, item.ID, in)
			refusedWith(t, err, http.StatusBadRequest, "invalid_request")
		}
		for _, r := range []discovery.FeedbackReason{discovery.ReasonBadArt, discovery.ReasonBadStory, discovery.ReasonMisleading, discovery.ReasonOther} {
			got, err := f.answer(reader, item.ID, discovery.FeedbackInput{Reason: reasonPtr(r)})
			must(t, err)
			if got.Answer == nil || got.Answer.Reason != r || got.FollowUp != nil || f.rows("user_id = $2::uuid AND content_id = $3::uuid AND reason = $4", user.ID, item.ID, string(r)) != 1 {
				t.Fatalf("%s answer: %+v", r, got)
			}
		}

		// Culprits are the tags the filters offer, in their order, at most three.
		_, token = w.person(t, 0)
		second := a.client(token)
		_, err = second.PostContentDislike(t.Context(), many.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		p = f.ask(second, many.ID, discovery.FeedbackDislike)
		if p == nil || !slices.Equal(p.Reasons, []discovery.FeedbackReason{discovery.ReasonBadArt, discovery.ReasonBadStory, discovery.ReasonNotInto, discovery.ReasonMisleading, discovery.ReasonOther}) {
			t.Fatalf("dislike reasons with culprits: %+v", p)
		}
		if got := feedbackIDs(p.Tags); !slices.Equal(got, []string{string(f.furry.ID), string(f.gore.ID), string(f.tentacle.ID)}) || !p.Tags[0].Hideable || !p.Tags[2].Hideable {
			t.Fatalf("culprit tags %v: %+v", got, p.Tags)
		}
		for _, e := range []*string{nil, strPtr(string(f.horror.ID)), strPtr(string(f.plain.ID))} {
			_, err = f.answer(second, many.ID, discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonNotInto), EntityID: e})
			refusedWith(t, err, http.StatusBadRequest, "invalid_request")
		}

		// A tag the reader already dislikes is a culprit though no filter offers it.
		_, token = w.person(t, 0)
		third := a.client(token)
		_, err = third.PostContentDislike(t.Context(), plainOne.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		_, err = third.PostContentDislike(t.Context(), plainTwo.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		p = f.ask(third, plainTwo.ID, discovery.FeedbackDislike)
		if p == nil || len(p.Tags) != 1 || p.Tags[0].ID != string(f.plain.ID) || p.Tags[0].Hideable {
			t.Fatalf("the already disliked tag: %+v", p)
		}
		_, err = f.answer(third, plainTwo.ID, discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonNotInto), EntityID: strPtr(string(f.plain.ID)), Hide: boolPtr(true)})
		refusedWith(t, err, http.StatusBadRequest, "invalid_request")
		got, err := f.answer(third, plainTwo.ID, discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonNotInto), EntityID: strPtr(string(f.plain.ID))})
		must(t, err)
		if got.Answer.EntityID == nil || *got.Answer.EntityID != string(f.plain.ID) || got.Hide != nil || got.HideTaken != nil || got.FollowUp != nil {
			t.Fatalf("not_into a tag no filter hides: %+v", got)
		}
	})

	t.Run("CK-J40-S06_not_into_offers_hiding_the_tag_in_account_filters", func(t *testing.T) {
		user, token := w.person(t, 0)
		reader := a.client(token)
		item := hideItem
		_, err := reader.PostContentDislike(t.Context(), item.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		p := f.ask(reader, item.ID, discovery.FeedbackDislike)
		if p == nil || len(p.Tags) != 1 || p.Tags[0].ID != string(f.furry.ID) || !p.Tags[0].Hideable || p.Hide != nil || p.HideTaken != nil {
			t.Fatalf("a filterable culprit, nothing preselected: %+v", p)
		}
		_, err = f.answer(reader, item.ID, discovery.FeedbackInput{Hide: boolPtr(true)})
		refusedWith(t, err, http.StatusBadRequest, "invalid_request")
		got, err := f.answer(reader, item.ID, discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonNotInto), EntityID: strPtr(string(f.furry.ID))})
		must(t, err)
		if got.Hide == nil || got.Hide.ID != string(f.furry.ID) || got.HideTaken != nil {
			t.Fatalf("not_into did not suggest hiding its tag: %+v", got)
		}
		if n := f.rows("user_id = $2::uuid AND reason = 'not_into' AND entity_id = $3 AND hide_entity_id = $3 AND hide_taken IS NULL", user.ID, string(f.furry.ID)); n != 1 {
			t.Fatal("the suggestion was not measured as offered")
		}
		got, err = f.answer(reader, item.ID, discovery.FeedbackInput{Hide: boolPtr(true)})
		must(t, err)
		if got.HideTaken == nil || !*got.HideTaken || got.Answer.Reason != discovery.ReasonNotInto {
			t.Fatalf("taking the suggestion: %+v", got)
		}
		filters, err := reader.GetContentFilters(t.Context(), ck.GetContentFiltersQuery{})
		must(t, err)
		if state := filterState(filters, string(f.furry.ID)); state == nil || state.Visible || !state.Overridden {
			t.Fatalf("the account's filters do not hide the tag: %+v", filters)
		}
		var raw string
		must(t, w.pool.QueryRow(t.Context(), `SELECT overrides->>$3 FROM `+pgx.Identifier{w.schema, "content_filter_prefs"}.Sanitize()+` WHERE tenant_id = $1 AND user_id = $2`,
			a.cfg.Scope.AppID.String(), user.ID, "entity:"+string(f.furry.ID)).Scan(&raw))
		if raw != "hide" {
			t.Fatalf("content_filter_prefs holds %q for the tag", raw)
		}
		if n := f.rows("user_id = $2::uuid AND hide_entity_id = $3 AND hide_taken", user.ID, string(f.furry.ID)); n != 1 {
			t.Fatal("the taken suggestion was not measured")
		}

		// A tag the reader already dislikes comes before the filters' order; declining keeps the filters.
		_, token = w.person(t, 0)
		picky := a.client(token)
		_, err = picky.PostContentDislike(t.Context(), earlier.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		_, err = picky.PostContentDislike(t.Context(), both.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		if p := f.ask(picky, both.ID, discovery.FeedbackDislike); p == nil || !slices.Equal(feedbackIDs(p.Tags), []string{string(f.gore.ID), string(f.furry.ID)}) {
			t.Fatalf("the already disliked tag was not first: %+v", p)
		}
		got, err = f.answer(picky, both.ID, discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonNotInto), EntityID: strPtr(string(f.gore.ID)), Hide: boolPtr(false)})
		must(t, err)
		if got.Hide == nil || got.Hide.ID != string(f.gore.ID) || got.HideTaken == nil || *got.HideTaken {
			t.Fatalf("declining: %+v", got)
		}
		filters, err = picky.GetContentFilters(t.Context(), ck.GetContentFiltersQuery{})
		must(t, err)
		if state := filterState(filters, string(f.gore.ID)); state == nil || !state.Visible || state.Overridden {
			t.Fatalf("declining changed the filters: %+v", filters)
		}

		// A tag the reader already hides stays a culprit but is not offered for hiding.
		_, token = w.person(t, 0)
		hider := a.client(token)
		_, err = hider.PatchMeContentFilters(t.Context(), discovery.ContentFiltersUpdate{Filters: []discovery.GuestFilterSetting{{EntityID: string(f.furry.ID), Visible: boolPtr(false)}}}, ck.PatchMeContentFiltersQuery{})
		must(t, err)
		_, err = hider.PostContentDislike(t.Context(), item.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		if p := f.ask(hider, item.ID, discovery.FeedbackDislike); p == nil || len(p.Tags) != 1 || p.Tags[0].Hideable {
			t.Fatalf("offered to hide a tag the reader hides already: %+v", p)
		}
		_, err = f.answer(hider, item.ID, discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonNotInto), EntityID: strPtr(string(f.furry.ID)), Hide: boolPtr(true)})
		refusedWith(t, err, http.StatusBadRequest, "invalid_request")
	})

	t.Run("CK-J40-S07_signed_out_hide_is_recorded_not_stored_for_an_account", func(t *testing.T) {
		item := signedOut
		id := uuid.NewString()
		anonymous := f.browser(id)
		_, err := anonymous.PostContentDislike(t.Context(), item.ID, ck.PostContentDislikeQuery{})
		must(t, err)
		p := f.ask(anonymous, item.ID, discovery.FeedbackDislike)
		if p == nil || len(p.Tags) != 1 || p.Tags[0].ID != string(f.furry.ID) || !p.Tags[0].Hideable {
			t.Fatalf("signed-out dislike prompt: %+v", p)
		}
		var before int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{w.schema, "content_filter_prefs"}.Sanitize()+` WHERE tenant_id = $1`, a.cfg.Scope.AppID.String()).Scan(&before))
		notInto := discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonNotInto), EntityID: strPtr(string(f.furry.ID)), Hide: boolPtr(true)}
		_, err = f.answer(a.client(""), item.ID, notInto)
		refusedWith(t, err, http.StatusBadRequest, "invalid_request")
		_, err = f.answer(f.browser(uuid.NewString()), item.ID, notInto)
		refusedWith(t, err, http.StatusNotFound, "not_found")
		got, err := f.answer(anonymous, item.ID, notInto)
		must(t, err)
		if got.Hide == nil || got.HideTaken == nil || !*got.HideTaken {
			t.Fatalf("signed-out take: %+v", got)
		}
		_, err = f.answer(anonymous, item.ID, discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonOther), Text: strPtr("signed-out words")})
		must(t, err)
		var after int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{w.schema, "content_filter_prefs"}.Sanitize()+` WHERE tenant_id = $1`, a.cfg.Scope.AppID.String()).Scan(&after))
		if after != before {
			t.Fatal("a signed-out choice was stored as account filters")
		}
		who, _ := subject.Anonymous(uuid.UUID(a.cfg.Scope.AppID), id)
		if n := f.rows("anonymous_actor_id = $2 AND user_id IS NULL AND content_id = $3::uuid AND reason = 'other' AND text = 'signed-out words' AND hide_entity_id = $4 AND hide_taken",
			who, item.ID, string(f.furry.ID)); n != 1 {
			t.Fatal("the signed-out answer and its hide decision were not recorded under its browser")
		}
		var active int
		must(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+pgx.Identifier{w.schema, "anonymous_activity"}.Sanitize()+` WHERE app_id = $1 AND subject = $2`,
			a.cfg.Scope.AppID.String(), who).Scan(&active))
		if active != 1 {
			t.Fatal("the signed-out prompt was not recorded as the browser's activity")
		}
	})

	t.Run("CK-J40-S08_signin_merges_browser_filters", func(t *testing.T) {
		user, token := w.person(t, 0)
		reader := a.client(token)
		_, err := reader.PatchMeContentFilters(t.Context(), discovery.ContentFiltersUpdate{Filters: []discovery.GuestFilterSetting{{EntityID: string(f.furry.ID), Visible: boolPtr(true)}}}, ck.PatchMeContentFiltersQuery{})
		must(t, err)
		browser := []discovery.GuestFilterSetting{
			{EntityID: string(f.furry.ID), Visible: boolPtr(false)},
			{EntityID: string(f.gore.ID), Visible: boolPtr(false)},
			{EntityID: string(f.plain.ID), Visible: boolPtr(false)},
			{EntityID: uuid.NewString(), Default: boolPtr(true)},
		}
		for range 2 {
			got, err := reader.PostMeContentFiltersMerge(t.Context(), discovery.ContentFiltersMerge{AccountID: user.ID, Filters: browser}, ck.PostMeContentFiltersMergeQuery{})
			must(t, err)
			for _, id := range []string{string(f.furry.ID), string(f.gore.ID)} {
				if state := filterState(got, id); state == nil || state.Visible || !state.Overridden {
					t.Fatalf("merged filters for %s: %+v", id, got)
				}
			}
		}
		var overrides map[string]string
		must(t, w.pool.QueryRow(t.Context(), `SELECT overrides FROM `+pgx.Identifier{w.schema, "content_filter_prefs"}.Sanitize()+` WHERE tenant_id = $1 AND user_id = $2`,
			a.cfg.Scope.AppID.String(), user.ID).Scan(&overrides))
		if len(overrides) != 2 || overrides["entity:"+string(f.furry.ID)] != "hide" || overrides["entity:"+string(f.gore.ID)] != "hide" {
			t.Fatalf("merge stored %v", overrides)
		}

		// A browser's show fills an unset entity but never overrides a hide.
		other, token := w.person(t, 0)
		second := a.client(token)
		_, err = second.PatchMeContentFilters(t.Context(), discovery.ContentFiltersUpdate{Filters: []discovery.GuestFilterSetting{{EntityID: string(f.gore.ID), Visible: boolPtr(false)}}}, ck.PatchMeContentFiltersQuery{})
		must(t, err)
		got, err := second.PostMeContentFiltersMerge(t.Context(), discovery.ContentFiltersMerge{AccountID: other.ID, Filters: []discovery.GuestFilterSetting{
			{EntityID: string(f.gore.ID), Visible: boolPtr(true)}, {EntityID: string(f.furry.ID), Visible: boolPtr(true)}}}, ck.PostMeContentFiltersMergeQuery{})
		must(t, err)
		if gore, furry := filterState(got, string(f.gore.ID)), filterState(got, string(f.furry.ID)); gore == nil || gore.Visible || furry == nil || !furry.Visible || !furry.Overridden {
			t.Fatalf("show merge: %+v", got)
		}
		_, err = second.PostMeContentFiltersMerge(t.Context(), discovery.ContentFiltersMerge{AccountID: user.ID, Filters: browser}, ck.PostMeContentFiltersMergeQuery{})
		refusedWith(t, err, http.StatusForbidden, "forbidden")
		_, err = a.client("").PostMeContentFiltersMerge(t.Context(), discovery.ContentFiltersMerge{AccountID: user.ID, Filters: browser}, ck.PostMeContentFiltersMergeQuery{})
		refusedWith(t, err, http.StatusUnauthorized, "unauthorized")
	})

	t.Run("CK-J40-S09_erasure_deletes_answers_and_their_text", func(t *testing.T) {
		user, token := w.person(t, 0)
		reader := a.client(token)
		item := erased
		_, err := reader.PostContentLike(t.Context(), item.ID, ck.PostContentLikeQuery{})
		must(t, err)
		if f.ask(reader, item.ID, discovery.FeedbackLike) == nil {
			t.Fatal("like was not asked about")
		}
		_, err = f.answer(reader, item.ID, discovery.FeedbackInput{Reason: reasonPtr(discovery.ReasonOther), Text: strPtr("private words to erase")})
		must(t, err)
		kept := f.rows("user_id IS NOT NULL AND user_id <> $2::uuid", user.ID)
		if kept == 0 || f.rows("user_id = $2::uuid AND text IS NOT NULL", user.ID) != 1 {
			t.Fatal("erasure fixture incomplete")
		}
		must(t, a.engine.AnonymizeAccount(t.Context(), user.ID))
		if n := f.rows("user_id = $2::uuid", user.ID); n != 0 {
			t.Fatalf("%d feedback rows survived erasure", n)
		}
		if n := f.rows("text = 'private words to erase'"); n != 0 {
			t.Fatal("erased free text survived without its account")
		}
		if n := f.rows("user_id IS NOT NULL AND user_id <> $2::uuid", user.ID); n != kept {
			t.Fatalf("erasure removed other people's feedback: %d of %d", n, kept)
		}
	})
}

// TestReactionFeedbackSampling runs an app asking half of likes and no
// dislikes. Each signed-out browser is a person; the rig's share one address,
// whose reaction names the browser that made it last.
func TestReactionFeedbackSampling(t *testing.T) {
	w := feedbackWorld(t)
	a := w.apps[0]
	half, never := 0.5, 0.0
	a.server.Close()
	must(t, a.engine.Close(t.Context()))
	a.cfg.Interactions.Feedback.Like, a.cfg.Interactions.Feedback.Dislike = &half, &never
	var err error
	a.engine, err = ck.New(t.Context(), a.cfg, a.deps)
	must(t, err)
	a.server = mountEngine(t, a.engine)
	f := newFeedbackFixture(t, w, a)
	liked, disliked := f.item("Sampled like", f.plain), f.item("Sampled dislike", f.plain)
	react := func(b *ck.Client) {
		t.Helper()
		_, err := b.PostContentLike(t.Context(), liked.ID, ck.PostContentLikeQuery{})
		must(t, err)
		_, err = b.PostContentDislike(t.Context(), disliked.ID, ck.PostContentDislikeQuery{})
		must(t, err)
	}

	t.Run("CK-J40-S10_sampling_follows_the_rates_and_never_redraws", func(t *testing.T) {
		const browsers = 60
		var asked, passed []*ck.Client
		for range browsers {
			b := f.browser(uuid.NewString())
			react(b)
			if p := f.ask(b, disliked.ID, discovery.FeedbackDislike); p != nil {
				t.Fatalf("a dislike was asked about at rate 0: %+v", p)
			}
			if f.ask(b, liked.ID, discovery.FeedbackLike) != nil {
				asked = append(asked, b)
			} else {
				passed = append(passed, b)
			}
		}
		if len(asked) < 12 || len(asked) > 48 {
			t.Fatalf("%d of %d likes asked about at rate 1/2", len(asked), browsers)
		}
		for _, b := range passed {
			for range 3 {
				react(b)
				if p := f.ask(b, liked.ID, discovery.FeedbackLike); p != nil {
					t.Fatalf("asking again drew a new sample: %+v", p)
				}
			}
		}
		if n := f.rows("anonymous_actor_id IS NOT NULL"); n != len(asked) {
			t.Fatalf("%d prompts stored, want %d", n, len(asked))
		}
		if n := f.rows("reaction = 'dislike'"); n != 0 {
			t.Fatal("a dislike prompt was stored at rate 0")
		}
	})
}

func feedbackIDs(list []discovery.FeedbackEntity) []string {
	out := []string{}
	for _, e := range list {
		out = append(out, e.ID)
	}
	return out
}

func filterState(filters discovery.ContentFilters, id string) *discovery.ContentFilterState {
	for i := range filters.Filters {
		if string(filters.Filters[i].Entity.ID) == id {
			return &filters.Filters[i]
		}
	}
	return nil
}
