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
	"strings"
	"testing"

	mediastore "github.com/open-rails/contentkit/internal/mediastore"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/avatar"
	"github.com/open-rails/contentkit/media/layout"
	_ "golang.org/x/image/webp"
)

// The stock worker binary publishes the registry's default renditions from
// its JSON (no database, no host code): ContentKit's avatar silhouette and a
// host default from MEDIA_DEFAULTS_DIR. Hosts link no libvips for this.
func TestWorkerPublishesDefaults(t *testing.T) {
	worker, err := filepath.Abs(".bin/media-worker")
	must(t, err)
	if info, err := os.Stat(worker); err != nil || info.Mode()&0o111 == 0 {
		t.Fatal("build the stock worker before this selected test: GOWORK=off CGO_ENABLED=1 go build -o .bin/media-worker github.com/open-rails/contentkit/cmd/media-worker")
	}
	m := newMediaStorage(t, ck.Scope{TenantID: ck.NewTenantID(), AppID: ck.NewAppID()})
	dir := t.TempDir()
	images := filepath.Join(dir, "defaults")
	must(t, os.Mkdir(images, 0o700))
	must(t, os.WriteFile(filepath.Join(images, "series.png"), imageBytes(t), 0o600))
	series := media.Kind{Name: "series", Uploads: []media.Upload{{Path: "cover", Types: []string{"image/png"}, MaxBytes: 1 << 20}},
		Public: []media.Public{{Name: "cover", From: "cover", To: "cover-{w}.webp", Widths: []int{32}, Image: media.Image{Aspect: media.Square}, Default: "series.png"}}}
	reg, err := mediastore.NewRegistry(media.Config{Namespace: "critical", Kinds: []media.Kind{series, avatar.User()}}, mediastore.Hooks{})
	must(t, err)
	kinds, err := reg.MarshalJSON()
	must(t, err)
	kindsFile := filepath.Join(dir, "kinds.json")
	must(t, os.WriteFile(kindsFile, kinds, 0o600))
	publish := func(t *testing.T, env ...string) (string, error) {
		command := exec.CommandContext(t.Context(), worker, "publish-defaults")
		command.Env = append([]string{"PATH=" + os.Getenv("PATH"), "MEDIA_KINDS_FILE=" + kindsFile, "MEDIA_S3_BUCKET=" + m.bucket,
			"MEDIA_S3_ENDPOINT=" + os.Getenv("CONTENTKIT_CRITICAL_S3_ENDPOINT"), "MEDIA_S3_ACCESS_KEY_ID=" + os.Getenv("CONTENTKIT_CRITICAL_S3_USER"),
			"MEDIA_S3_SECRET_ACCESS_KEY=" + os.Getenv("CONTENTKIT_CRITICAL_S3_PASSWORD")}, env...)
		var out, logs bytes.Buffer
		command.Stdout, command.Stderr = &out, &logs
		err := command.Run()
		if err != nil {
			return logs.String(), err
		}
		return out.String(), nil
	}

	t.Run("without_the_host_images_it_refuses", func(t *testing.T) {
		logs, err := publish(t)
		if err == nil || !strings.Contains(logs, "series.png") {
			t.Fatalf("a default missing from every source must fail: %v\n%s", err, logs)
		}
	})

	var mapping string
	t.Run("renders_every_default_once", func(t *testing.T) {
		out, err := publish(t, "MEDIA_DEFAULTS_DIR="+images)
		if err != nil {
			t.Fatalf("publish-defaults: %v\n%s", err, out)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		written, summary := lines[:len(lines)-2], lines[len(lines)-2:]
		if len(written) != 5 || summary[0] != "Published 5 default rendition(s)." || !strings.HasPrefix(summary[1], "MEDIA_GATEWAY_DEFAULTS=") {
			t.Fatalf("one rendition per default width, then the gateway's value:\n%s", out)
		}
		mapping = strings.TrimPrefix(summary[1], "MEDIA_GATEWAY_DEFAULTS=")
		defaults, err := layout.ParseDefaults(mapping)
		must(t, err)
		widths := map[string]int{}
		for _, d := range defaults {
			for name, file := range d.Files {
				key := strings.Join([]string{d.Namespace, d.Kind, "_default", "public", file}, "/")
				if !strings.Contains(out, key+"\n") {
					t.Fatalf("mapping %s=%s names %s, which this run did not write:\n%s", name, file, key, out)
				}
				body, obj, err := m.store.Get(t.Context(), key, media.GetOptions{})
				must(t, err)
				data, err := io.ReadAll(body)
				must(t, errors.Join(err, body.Close()))
				config, format, err := image.DecodeConfig(bytes.NewReader(data))
				must(t, err)
				if obj.ContentType != "image/webp" || format != "webp" || config.Width != config.Height {
					t.Fatalf("%s is %s %s %dx%d, want a square WebP", key, obj.ContentType, format, config.Width, config.Height)
				}
				widths[d.Kind+"/"+name] = config.Width
			}
		}
		want := map[string]int{"series/cover-32.webp": 32, "user/avatar-64.webp": 64, "user/avatar-128.webp": 128, "user/avatar-256.webp": 256, "user/avatar-512.webp": 512}
		if len(widths) != len(want) {
			t.Fatalf("published defaults %v, want %v", widths, want)
		}
		for name, width := range want {
			if widths[name] != width {
				t.Fatalf("published defaults %v, want %v", widths, want)
			}
		}
	})

	t.Run("repeating_writes_nothing", func(t *testing.T) {
		out, err := publish(t, "MEDIA_DEFAULTS_DIR="+images)
		if err != nil {
			t.Fatalf("publish-defaults again: %v\n%s", err, out)
		}
		if out != "Published 0 default rendition(s).\nMEDIA_GATEWAY_DEFAULTS="+mapping+"\n" {
			t.Fatalf("immutable defaults changed on repeat:\n%s", out)
		}
	})
}
