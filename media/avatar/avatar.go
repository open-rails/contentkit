// Package avatar provides a user-avatar kind for ContentKit media registries.
package avatar

import (
	"embed"

	"github.com/open-rails/contentkit/media"
)

//go:embed avatar.png
var defaults embed.FS

// User returns an avatar kind keyed by the auth subject. Its shared accounts
// namespace lets every site's media host serve the same user's avatar, at
// a Stable path built from the id. Upload authorization stays with the
// host's CanUpload hook or ContentKit's AuthKit adapter.
//
// The returned kind can be customized independently before registration.
func User() media.Kind {
	return media.Kind{
		Name:          "user",
		Namespace:     "accounts",
		KeepOriginals: true,
		Uploads: []media.Upload{{
			Path:     "avatar",
			Types:    []string{"image/jpeg", "image/png", "image/webp"},
			MaxBytes: 10 << 20,
		}},
		Public: []media.Public{{
			Name:    "avatar",
			From:    "avatar",
			To:      "avatar-{w}.webp",
			Widths:  []int{64, 128, 256, 512},
			Image:   media.Image{Aspect: media.Square, Quality: 85, Animation: media.AnimationReject},
			Default: "avatar.png",
			Stable:  true,
		}},
		Defaults: defaults,
	}
}
