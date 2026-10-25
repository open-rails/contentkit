package critical_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/media"
)

// Use the existing upload-path policies to express independent attachment
// ceilings. Five images and one video are fixture policy, not a kit default.
func TestPostAttachmentPolicies(t *testing.T) {
	w := newWorld(t)
	a := w.apps[0]
	attachMedia(t, w, a, func(cfg *ck.Config) {
		cfg.Media.Kinds = []media.Kind{{Name: "article", KeepOriginals: true,
			Uploads: []media.Upload{
				{Path: "images/{name}", Types: []string{"image/png"}, MaxBytes: 1 << 20, Max: 5},
				{Path: "videos/{name}", Types: []string{"video/mp4"}, MaxBytes: 2 << 20, Max: 1},
			},
			Private: []media.Private{{Name: "reader", From: "images/{name}", To: "reader/{name}.webp", Image: &media.Image{Width: 48, Height: 48}}},
		}}
	})
	persona, err := w.auth.Persona("app")
	must(t, err)
	key, err := w.tenant[0].CreateAPIKey(t.Context(), iam.SystemIdentity(), a.group, iam.NewAPIKey{Name: "Attachment policy editor", Role: persona.OwnerRole()})
	must(t, err)
	client := a.client(key.Secret)
	item, err := client.PostContents(t.Context(), catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: "Independent attachment ceilings", Body: "A mixed-media post"}}})
	must(t, err)
	ref := media.RefBody{Kind: "article", ID: item.ID, Version: item.Versions[0].ID}
	payload := imageBytes(t)
	ops := make([]media.Op, 6)
	for i := range ops {
		ops[i] = stageFile(t, client, ref, fmt.Sprintf("images/%d.png", i), payload)
	}
	_, err = client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: ops[:4]})
	must(t, err)
	awaitMediaReady(t, client, ref)
	// A second application instance shares storage and the admission journal.
	cfg := a.cfg
	cfg.Database.SkipMigrations = true
	reopened, err := ck.New(t.Context(), cfg, a.deps)
	must(t, err)
	t.Cleanup(func() { must(t, reopened.Close(context.Background())) })
	server := mountEngine(t, reopened)
	second := ck.NewClient(server.URL+"/content", &http.Client{Transport: bearerTransport{token: key.Secret, base: http.DefaultTransport}})
	type result struct {
		op  media.Op
		err error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for i, c := range []*ck.Client{client, second} {
		go func(i int, c *ck.Client) {
			<-start
			_, err := c.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{ops[4+i]}})
			results <- result{ops[4+i], err}
		}(i, c)
	}
	close(start)
	successes := 0
	for range 2 {
		r := <-results
		if r.err == nil {
			successes++
			continue
		}
		// A competing admission may ask for a retry while the winner owns the
		// journal. On retry the actual policy must still reject the sixth image.
		_, err = client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{r.op}})
		requireError(t, err, 409, "too_many_files")
	}
	if successes != 1 {
		t.Fatalf("concurrent fifth-image admissions succeeded %d times; want one", successes)
	}
	read := awaitMediaReady(t, client, ref)
	count := 0
	for _, f := range read.Files {
		if f.Upload && strings.HasPrefix(f.Path, "images/") {
			count++
		}
	}
	if count != 5 {
		t.Fatalf("stored %d images after concurrent commits; want5", count)
	}
	// Hitting the image ceiling does not consume the independent video slot.
	source := filepath.Join(t.TempDir(), "clip.mp4")
	command := exec.CommandContext(t.Context(), "ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=5", "-t", "0.4", "-an", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-movflags", "+faststart", source)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("video fixture: %v\n%s", err, output)
	}
	video, err := os.ReadFile(source)
	must(t, err)
	stageVideo := func(name string) media.Op {
		plan := uploadPlan(t, client, ref, name, "video/mp4", video)
		if !plan.Exists {
			if plan.Put == nil {
				t.Fatal("small video needs direct PUT")
			}
			if code, body := transfer(t, *plan.Put, video); code != 200 {
				t.Fatalf("video PUT:%d %s", code, body)
			}
		}
		return media.Op{Op: media.OpPut, Path: plan.Path, Blob: plan.Blob}
	}
	video1, video2 := stageVideo("videos/one.mp4"), stageVideo("videos/two.mp4")
	_, err = client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{video1}})
	must(t, err)
	_, err = client.PostMediaUploadCommit(t.Context(), media.CommitBody{Ref: ref, OperationID: uuid.NewString(), Ops: []media.Op{video2}})
	requireError(t, err, 409, "too_many_files")
	_, err = client.PostMediaUploadPresign(t.Context(), media.PresignBody{Ref: ref, Path: "images/not-a-video.mp4", Type: "video/mp4", Size: int64(len(video)), SHA256: digestBytes(video)})
	requireError(t, err, 415, "type_not_allowed")
	read = awaitMediaReady(t, client, ref)
	images, videos := 0, 0
	for _, f := range read.Files {
		if !f.Upload {
			continue
		}
		if strings.HasPrefix(f.Path, "images/") {
			images++
		}
		if strings.HasPrefix(f.Path, "videos/") {
			videos++
		}
	}
	if images != 5 || videos != 1 {
		t.Fatalf("attachment limits changed: images=%d videos=%d", images, videos)
	}
}
