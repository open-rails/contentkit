package main

import (
	"context"
	"strings"

	"github.com/microcosm-cc/bluemonday"

	"github.com/open-rails/contentkit/content"
)

// The host's content policies, deterministic for tests: a comment or text
// containing "[hold]" is held for review and "[reject]" is refused.
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

// blogKind is the content kind of blog posts.
const blogKind = "blog"

// postHTML is the host's rich-text body sanitizer: user-generated HTML, images by
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
