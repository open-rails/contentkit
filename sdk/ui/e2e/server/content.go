package main

import (
	"context"
	"path"
	"strings"

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
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

// inlineURLs is content.MediaURLs: an inline image's published file, or the
// name its preset template gives until the worker has published it. A
// workaround: published names carry a generation, so a stored URL goes
// stale (ContentKit tracker #111, references resolved at render time).
type inlineURLs struct{ manifests *media.Manifests }

func (u inlineURLs) InlineURL(ctx context.Context, ref contentref.ContentRef, name string) (string, error) {
	images, err := u.manifests.PublicImages(ctx, ref)
	if err != nil {
		return "", err
	}
	for _, im := range images {
		if strings.TrimSuffix(im.From, path.Ext(im.From)) == name && len(im.Renditions) > 0 {
			return im.Renditions[0].URL, nil
		}
	}
	return u.manifests.Registry().PublicURL(ref, name+".webp"), nil
}
