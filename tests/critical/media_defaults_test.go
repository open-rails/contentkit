package critical_test

import (
	"bytes"
	"errors"
	"image"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/avatar"
	"github.com/open-rails/contentkit/media/layout"
	_ "golang.org/x/image/webp"
)

// The stock worker binary publishes the registry's default renditions from
// its JSON (no database, no host code): ContentKit's avatar silhouette and a
// host default from MEDIA_DEFAULTS_DIR. Hosts link no libvips for this. An
// account kind's defaults are the avatar primary's (TestSecondaryDefaultAvatar).
func TestWorkerPublishesDefaults(t *testing.T) {
	stockWorker(t)
	m := newMediaStorage(t, ck.Scope{TenantID: ck.NewTenantID(), AppID: ck.NewAppID()})
	dir := t.TempDir()
	images := filepath.Join(dir, "defaults")
	must(t, os.Mkdir(images, 0o700))
	must(t, os.WriteFile(filepath.Join(images, "series.png"), imageBytes(t), 0o600))
	series := media.Kind{Name: "series", Uploads: []media.Upload{{Path: "cover", Types: []string{"image/png"}, MaxBytes: 1 << 20}},
		Public: []media.Public{{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{32}, Image: media.Image{Aspect: media.Square}, Default: "series.png"}}}
	creator := avatar.User() // an app kind showing ContentKit's silhouette
	creator.Name, creator.Account = "creator", false
	reg, err := mediastore.NewRegistry(media.Config{Scope: newMediaScope(), Kinds: []media.Kind{series, creator}}, mediastore.Hooks{})
	must(t, err)
	kinds, err := reg.MarshalJSON()
	must(t, err)
	kindsFile := filepath.Join(dir, "kinds.json")
	must(t, os.WriteFile(kindsFile, kinds, 0o600))
	publish := func(t *testing.T, env ...string) (string, error) {
		return publishDefaults(t, kindsFile, m.bucket, env...)
	}

	t.Run("without_the_host_images_it_refuses", func(t *testing.T) {
		logs, err := publish(t)
		if err == nil || !strings.Contains(logs, "series.png") {
			t.Fatalf("a default missing from every source must fail: %v\n%s", err, logs)
		}
	})

	manifestKeys := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, name := range []string{"series", "creator"} {
			k, err := reg.Kind(name)
			must(t, err)
			obj, err := m.store.Head(t.Context(), k.DefaultsItem().ManifestKey())
			must(t, err)
			out[name] = obj.ETag
		}
		return out
	}
	var first map[string]string
	t.Run("renders_every_default_once", func(t *testing.T) {
		out, err := publish(t, "MEDIA_DEFAULTS_DIR="+images)
		if err != nil {
			t.Fatalf("publish-defaults: %v\n%s", err, out)
		}
		if got := strings.Fields(out); len(got) != 2 || got[0] != "series" || got[1] != "creator" {
			t.Fatalf("one line per kind written:\n%s", out)
		}
		// Each kind's defaults are its _default item, named in its manifest:
		// the engine reads them from there (Registry.DefaultImage).
		mustManifests(t, m.store, reg)
		widths := map[string]int{}
		for _, c := range []struct{ kind, preset string }{{"series", "cover"}, {"creator", "avatar"}} {
			id := uuid.Must(uuid.NewV7()).String()
			ref, err := reg.Ref(c.kind, id)
			must(t, err)
			image, ok := reg.DefaultImage(t.Context(), ref, c.preset)
			if !ok {
				t.Fatalf("%s has no default %s", c.kind, c.preset)
			}
			for _, r := range image.Renditions {
				key := strings.TrimPrefix(r.URL, layout.URLPrefix)
				body, obj, err := m.store.Get(t.Context(), key, media.GetOptions{})
				must(t, err)
				data, err := io.ReadAll(body)
				must(t, errors.Join(err, body.Close()))
				config, format, err := decodeImageConfig(data)
				must(t, err)
				if obj.ContentType != "image/webp" || format != "webp" || config.Width != config.Height || config.Width != r.W {
					t.Fatalf("%s is %s %s %dx%d, want a square WebP %d wide", key, obj.ContentType, format, config.Width, config.Height, r.W)
				}
				widths[c.kind+"/"+c.preset+"-"+strconv.Itoa(r.W)] = config.Width
			}
		}
		want := map[string]int{"series/cover-32": 32, "creator/avatar-64": 64, "creator/avatar-128": 128, "creator/avatar-256": 256, "creator/avatar-512": 512}
		if len(widths) != len(want) {
			t.Fatalf("published defaults %v, want %v", widths, want)
		}
		for name, width := range want {
			if widths[name] != width {
				t.Fatalf("published defaults %v, want %v", widths, want)
			}
		}
		first = manifestKeys()
	})

	t.Run("repeating_writes_nothing", func(t *testing.T) {
		out, err := publish(t, "MEDIA_DEFAULTS_DIR="+images)
		if err != nil {
			t.Fatalf("publish-defaults again: %v\n%s", err, out)
		}
		if again := manifestKeys(); again["series"] != first["series"] || again["creator"] != first["creator"] {
			t.Fatalf("defaults changed on repeat: %v -> %v", first, again)
		}
	})
}

// stockWorker is the built cmd/media-worker.
func stockWorker(t *testing.T) string {
	t.Helper()
	worker, err := filepath.Abs(".bin/media-worker")
	must(t, err)
	if info, err := os.Stat(worker); err != nil || info.Mode()&0o111 == 0 {
		t.Fatal("build the stock worker before this selected test: GOWORK=off CGO_ENABLED=1 go build -o .bin/media-worker github.com/open-rails/contentkit/cmd/media-worker")
	}
	return worker
}

// publishDefaults runs the stock worker's publish-defaults over bucket with
// the registry in kindsFile: its output, or its log when it fails.
func publishDefaults(t *testing.T, kindsFile, bucket string, env ...string) (string, error) {
	t.Helper()
	command := exec.CommandContext(t.Context(), stockWorker(t), "publish-defaults")
	command.Env = append([]string{"PATH=" + os.Getenv("PATH"), "MEDIA_KINDS_FILE=" + kindsFile, "MEDIA_S3_BUCKET=" + bucket,
		"MEDIA_S3_ENDPOINT=" + os.Getenv("CONTENTKIT_CRITICAL_S3_ENDPOINT"), "MEDIA_S3_ACCESS_KEY_ID=" + os.Getenv("CONTENTKIT_CRITICAL_S3_USER"),
		"MEDIA_S3_SECRET_ACCESS_KEY=" + os.Getenv("CONTENTKIT_CRITICAL_S3_PASSWORD")}, env...)
	var out, logs bytes.Buffer
	command.Stdout, command.Stderr = &out, &logs
	if err := command.Run(); err != nil {
		return logs.String(), err
	}
	return out.String(), nil
}

func decodeImageConfig(data []byte) (image.Config, string, error) {
	return image.DecodeConfig(bytes.NewReader(data))
}
