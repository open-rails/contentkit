package discovery

import (
	"fmt"
	"time"
)

// FeedbackReaction is the reaction a prompt follows.
type FeedbackReaction string

const (
	FeedbackLike     FeedbackReaction = "like"
	FeedbackFavorite FeedbackReaction = "favorite"
	FeedbackDislike  FeedbackReaction = "dislike"
)

// FeedbackPolarity is whether a prompt asks what was liked or disliked.
type FeedbackPolarity string

const (
	PolarityLike    FeedbackPolarity = "like"
	PolarityDislike FeedbackPolarity = "dislike"
)

// FeedbackReason is an answer. A dislike prompt offers BadArt, BadStory,
// NotInto (one of its likely culprit tags), Misleading (its cover, title or
// tags) and Other; a like or favorite prompt Artwork, Artist, Story, Series,
// Tag and Other. Other is "Something else…", with optional text.
type FeedbackReason string

const (
	ReasonBadArt     FeedbackReason = "bad_art"
	ReasonBadStory   FeedbackReason = "bad_story"
	ReasonNotInto    FeedbackReason = "not_into"
	ReasonMisleading FeedbackReason = "misleading"
	ReasonArtwork    FeedbackReason = "artwork"
	ReasonArtist     FeedbackReason = "artist"
	ReasonStory      FeedbackReason = "story"
	ReasonSeries     FeedbackReason = "series"
	ReasonTag        FeedbackReason = "tag"
	ReasonOther      FeedbackReason = "other"
)

// MaxFeedbackText is the longest free-text answer, in characters.
const MaxFeedbackText = 280

// FeedbackConfig is when readers are asked why they liked, favorited or
// disliked content. A kind not in Kinds is never asked about.
type FeedbackConfig struct {
	// Kinds are the content kinds that ask, by name.
	Kinds map[string]FeedbackKind `yaml:"kinds" json:"kinds"`
	// Favorite, Like and Dislike are the share of each reaction that is
	// asked about, from 0 to 1; nil is 0.5, 0.2 and 0.3.
	Favorite *float64 `yaml:"favorite,omitempty" json:"favorite,omitempty"`
	Like     *float64 `yaml:"like,omitempty" json:"like,omitempty"`
	Dislike  *float64 `yaml:"dislike,omitempty" json:"dislike,omitempty"`
	// PerDay is how many prompts one person is offered in 24 hours, across
	// every kind; 0 is 1.
	PerDay int `yaml:"per_day,omitempty" json:"per_day,omitempty"`
}

// FeedbackKind names the entity kinds a kind's answers pick from.
type FeedbackKind struct {
	// Tags are "a tag" answers and the tags a dislike may suggest hiding.
	Tags []string `yaml:"tags" json:"tags"`
	// Artists are "the artist's style" answers.
	Artists []string `yaml:"artists" json:"artists"`
	// Series are "the series or characters" answers.
	Series []string `yaml:"series" json:"series"`
}

// Rate is the share of reaction asked about.
func (c FeedbackConfig) Rate(reaction FeedbackReaction) float64 {
	pick := func(v *float64, def float64) float64 {
		if v == nil {
			return def
		}
		return *v
	}
	switch reaction {
	case FeedbackFavorite:
		return pick(c.Favorite, 0.5)
	case FeedbackLike:
		return pick(c.Like, 0.2)
	case FeedbackDislike:
		return pick(c.Dislike, 0.3)
	}
	return 0
}

// Budget is the prompts one person is offered in 24 hours.
func (c FeedbackConfig) Budget() int {
	if c.PerDay <= 0 {
		return 1
	}
	return c.PerDay
}

// Validate checks the rates and that each kind's entity kinds are its own.
func (c FeedbackConfig) Validate(kindEntities map[string][]string) error {
	for _, r := range []*float64{c.Favorite, c.Like, c.Dislike} {
		if r != nil && (*r < 0 || *r > 1) {
			return fmt.Errorf("feedback: a rate is between 0 and 1, got %v", *r)
		}
	}
	if c.PerDay < 0 {
		return fmt.Errorf("feedback: per_day is not negative")
	}
	for kind, k := range c.Kinds {
		allowed, ok := kindEntities[kind]
		if !ok {
			return fmt.Errorf("feedback: content kind %q is not configured", kind)
		}
		seen := map[string]bool{}
		for _, e := range append(append(append([]string{}, k.Tags...), k.Artists...), k.Series...) {
			if seen[e] {
				return fmt.Errorf("feedback: kind %q names entity kind %q twice", kind, e)
			}
			seen[e] = true
			found := false
			for _, a := range allowed {
				found = found || a == e
			}
			if !found {
				return fmt.Errorf("feedback: kind %q has no entity kind %q", kind, e)
			}
		}
	}
	return nil
}

// FeedbackRequest asks for a prompt after the reader's reaction. A
// signed-out reader sends its anonymous id in ContentKit-Anonymous-Id.
type FeedbackRequest struct {
	// Reaction is the one just made: like, favorite or dislike.
	Reaction FeedbackReaction `json:"reaction"`
}

// FeedbackOffer is the prompt to show, or null when there is none.
type FeedbackOffer struct {
	Prompt *ReactionFeedback `json:"prompt"`
}

// FeedbackEntity is an entity an answer can name.
type FeedbackEntity struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
	// Hideable is a dislike's tag the reader's content filters can hide and
	// still show: answering not_into it suggests hiding it.
	Hideable bool `json:"hideable"`
}

// ReactionFeedback is one prompt and its answer.
type ReactionFeedback struct {
	ContentID string           `json:"content_id"`
	Reaction  FeedbackReaction `json:"reaction"`
	Polarity  FeedbackPolarity `json:"polarity"`
	// Reasons are the answers offered, in order.
	Reasons []FeedbackReason `json:"reasons"`
	// Tags, Artists and Series are the content's entities a like's answer
	// names: a tag, the artist's style, the series or characters. A
	// dislike's Tags are its likely culprits, at most three, that not_into
	// names: tags the content filters offer (polarizing) or the reader
	// already dislikes.
	Tags    []FeedbackEntity `json:"tags"`
	Artists []FeedbackEntity `json:"artists"`
	Series  []FeedbackEntity `json:"series"`
	// Hide is the tag suggested for hiding, once a not_into answer names a
	// hideable one, and HideTaken whether it was hidden; null until then.
	Hide      *FeedbackEntity `json:"hide"`
	HideTaken *bool           `json:"hide_taken"`
	OfferedAt time.Time       `json:"offered_at"`
	Answer    *FeedbackAnswer `json:"answer"`
	// FollowUp is what the answer leads to, or null.
	FollowUp *FeedbackFollowUp `json:"follow_up"`
}

// FeedbackAnswer is the reader's answer.
type FeedbackAnswer struct {
	Reason     FeedbackReason `json:"reason"`
	EntityID   *string        `json:"entity_id"`
	Text       *string        `json:"text"`
	AnsweredAt time.Time      `json:"answered_at"`
}

// FeedbackFollowUp is the action an answer suggests: follow the artist or
// series (where the host has follows) or browse the tag.
type FeedbackFollowUp struct {
	Action FollowUpAction `json:"action"`
	Entity FeedbackEntity `json:"entity"`
}

// FollowUpAction is what a follow-up offers.
type FollowUpAction string

const (
	FollowUpFollow FollowUpAction = "follow"
	FollowUpBrowse FollowUpAction = "browse"
)

// FeedbackInput answers a prompt, decides its hide suggestion, or both.
type FeedbackInput struct {
	// Reason is one of the prompt's Reasons; a later answer replaces it.
	Reason *FeedbackReason `json:"reason,omitempty"`
	// EntityID is the tag (required for tag and not_into), artist or
	// series the answer names; with one artist or series it may be left out.
	EntityID *string `json:"entity_id,omitempty"`
	// Text goes with other, at most MaxFeedbackText characters.
	Text *string `json:"text,omitempty"`
	// Hide takes (true) or declines hiding the tag a not_into answer names,
	// when it is hideable. Taken by an account it hides the tag in the
	// account's content filters; a signed-out browser keeps that choice
	// itself.
	Hide *bool `json:"hide,omitempty"`
}
