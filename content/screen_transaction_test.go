package content

import (
	"context"
	"testing"

	"github.com/open-rails/contentkit/access"
)

type pausedScreen struct{ entered, release chan struct{} }

func (*pausedScreen) StatelessPolicy() {}
func (p *pausedScreen) Screen(_ context.Context, in ModerationInput) (Verdict, error) {
	if in.Text == "paused edit" {
		close(p.entered)
		<-p.release
	}
	return Verdict{Decision: DecisionApprove}, nil
}
func TestPausedCommentScreeningCannotOverwriteNewerEdit(t *testing.T) {
	mod := &pausedScreen{make(chan struct{}), make(chan struct{})}
	rt := moderatedRuntime(t, mod)
	author := access.Actor{ID: "author"}
	ctx := context.Background()
	cm := mustComment(t, rt, author, "gallery", cid(1), CommentInput{Body: "public"})
	done := make(chan error, 1)
	go func() { _, err := rt.comments.edit(ctx, author, cm.ID, "paused edit"); done <- err }()
	<-mod.entered
	_, err := rt.comments.edit(ctx, author, cm.ID, "newer edit")
	close(mod.release)
	older := <-done
	if err != nil || older != errContentChanged {
		t.Fatalf("new=%v old=%v", err, older)
	}
}
