package avatar_test

import (
	"bytes"
	"encoding/json"
	"image/png"
	"io/fs"
	"testing"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/avatar"
)

func TestUserAcrossHosts(t *testing.T) {
	const userID = "01920000-0000-7000-8000-000000000001"
	for _, host := range []string{"first", "second"} {
		t.Run(host, func(t *testing.T) {
			origin := "https://media." + host + ".test"
			reg, err := media.NewRegistry(media.Config{
				Namespace: host, BaseURL: origin, Kinds: []media.Kind{avatar.User()},
				Avatars: media.Avatars{Kind: "user", Preset: "avatar"},
			})
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(reg)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := media.ParseConfig(data)
			if err != nil {
				t.Fatal(err)
			}
			worker, err := media.NewRegistry(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := worker.Ref("user", userID)
			if err != nil {
				t.Fatal(err)
			}
			want := origin + "/v1/accounts/user/" + userID + "/public/avatar-128.webp"
			if got := worker.PublicURL(ref, "avatar-128.webp"); got != want {
				t.Fatalf("avatar URL = %q, want %q", got, want)
			}
			for _, rule := range worker.PresetRules() {
				if rule.Kind == "user" && rule.Name == "avatar" && rule.Default {
					return
				}
			}
			t.Fatal("serialized avatar lost its default image rule")
		})
	}
}

func TestUserDefaultImage(t *testing.T) {
	kind := avatar.User()
	src, err := fs.ReadFile(kind.Defaults, kind.Public[0].Default)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if bounds := img.Bounds(); bounds.Dx() != 512 || bounds.Dy() != 512 {
		t.Fatalf("default dimensions = %v, want 512x512", bounds)
	}
}
