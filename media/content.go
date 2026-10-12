package media

import (
	"fmt"
	"maps"
	"slices"

	"github.com/open-rails/contentkit/kinds"
)

// CoverPreset is the public preset a content's or a group's cover shows.
const CoverPreset = "cover"

// AvatarPreset is the public preset a user-owned group's avatar shows.
const AvatarPreset = "avatar"

// ForContent derives c from the site's one kinds list: a media kind named
// after a content kind is that kind's, of Versions, in the app's namespace;
// every group kind without one declared gets GroupKind's. No other kind has
// versions.
func ForContent(c Config, k kinds.Config) (Config, error) {
	out := c
	out.Kinds = slices.Clone(c.Kinds)
	for i := range out.Kinds {
		mk := &out.Kinds[i]
		_, content := k.Kinds[mk.Name]
		switch {
		case content && mk.Namespace != "":
			return Config{}, fmt.Errorf("media: content kind %q lives in the app's namespace", mk.Name)
		case !content && mk.Versions:
			return Config{}, fmt.Errorf("media: kind %q has Versions but is no content kind", mk.Name)
		}
		mk.Versions = content
	}
	for _, name := range slices.Sorted(maps.Keys(k.GroupKinds)) {
		if !slices.ContainsFunc(out.Kinds, func(mk Kind) bool { return mk.Name == name }) {
			out.Kinds = append(out.Kinds, GroupKind(name, k.GroupKinds[name]))
		}
	}
	return out, nil
}

// GroupKind is a group kind's media: named cover images ("cover/i-{uuid}"),
// and for user-owned groups named avatars, each the group's selection
// (groups.Cover.Image) among them.
func GroupKind(name string, gk kinds.GroupKind) Kind {
	image := func(path string) Upload {
		return Upload{Path: path + "/{name}", Types: slices.Clone(ImageTypes), MaxBytes: 20 << 20, Named: true, Max: 20}
	}
	k := Kind{Name: name, KeepOriginals: true, Uploads: []Upload{image(CoverPreset)},
		Public: []Public{{Name: CoverPreset, From: CoverPreset + "/{name}", To: "cover-{name}-{w}.webp", Widths: []int{320, 640, 1280},
			Image: Image{Quality: 82}}}}
	if gk.Owner == "user" {
		k.Uploads = append(k.Uploads, image(AvatarPreset))
		k.Public = append(k.Public, Public{Name: AvatarPreset, From: AvatarPreset + "/{name}", To: "avatar-{name}-{w}.webp",
			Widths: []int{64, 128, 256}, Image: Image{Aspect: Square, Quality: 85}})
	}
	return k
}
