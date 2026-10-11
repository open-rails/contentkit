package main

import (
	"context"
	"strings"

	"github.com/microcosm-cc/bluemonday"

	"github.com/open-rails/contentkit/content"
)

// The host's content policies, deterministic for tests: a comment or post
// containing "[hold]" is held for review, "[reject]" is refused; free-text
// poll answers group by their first word.
type moderator struct{}

func (moderator) Screen(_ context.Context, in content.ModerationInput) (content.Verdict, error) {
	switch {
	case strings.Contains(in.Text, "[reject]"):
		return content.Verdict{Decision: content.DecisionReject, Reason: "not allowed here"}, nil
	case strings.Contains(in.Text, "[hold]"):
		return content.Verdict{Decision: content.DecisionReview, Reason: "needs a look"}, nil
	}
	return content.Verdict{Decision: content.DecisionApprove}, nil
}

func (moderator) StatelessPolicy() {}

type classifier struct{}

func (classifier) Classify(_ context.Context, a content.Answer) (content.GroupAssignment, error) {
	word := strings.ToLower(strings.Fields(a.Text + " other")[0])
	return content.GroupAssignment{GroupID: word, Label: strings.ToUpper(word[:1]) + word[1:]}, nil
}

func (classifier) StatelessPolicy() {}

// Post and poll images live in these media kinds (content.Media's defaults).
const (
	postFolder = "post"
	pollFolder = "poll"
)

func isFolder(kind string) bool { return kind == postFolder || kind == pollFolder }

// postHTML is the host's post body sanitizer: user-generated HTML, images by
// image reference (contentkit:i-{uuid}) as well as http(s) URLs.
type postHTML struct{ p *bluemonday.Policy }

func newPostHTML() postHTML {
	p := bluemonday.UGCPolicy()
	p.AllowURLSchemes("http", "https", "mailto", "contentkit")
	return postHTML{p}
}

func (h postHTML) Sanitize(_ context.Context, raw string) (string, error) {
	return strings.TrimSpace(h.p.Sanitize(raw)), nil
}
