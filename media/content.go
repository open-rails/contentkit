package media

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/open-rails/contentkit/kinds"
)

// CoverPreset is the public preset a content's or a group's cover shows.
const CoverPreset = "cover"

// AvatarPreset is the public preset a user-owned group's avatar shows.
const AvatarPreset = "avatar"

// BodySuffix names a content kind's body kind: {Name}.body holds each
// content item's body images, one item per content ({kind}.body, id), which
// shows only to viewers who open the content. A locked item's body images
// never reach a viewer who has not opened it; an open item's keep public files.
const BodySuffix = ".body"

// BodyOf is the content kind of a body kind.
func BodyOf(kind string) (string, bool) { return strings.CutSuffix(kind, BodySuffix) }

// ForContent derives c from the site's one kinds list: a media kind named
// after a content kind is that kind's, of Versions, in the app's namespace;
// every group kind without one declared gets GroupKind's. No other kind has
// versions. NewRegistry adds the content kinds' body kinds.
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

// bodyKind is a content kind's body kind (Upload.Named and Content images
// with a public preset): its named content images, their
// public presets, and for each a private image the item token serves to a
// viewer who opened the content while it has no public files.
func bodyKind(k Kind) (Kind, bool) {
	body := Kind{Name: k.Name + BodySuffix, KeepOriginals: k.KeepOriginals}
	for _, u := range k.Uploads {
		if !u.Content || !u.Named || !slices.ContainsFunc(k.Public, func(p Public) bool { return p.From == u.Path }) {
			continue
		}
		u.Content = false
		body.Uploads = append(body.Uploads, u)
		for _, p := range k.Public {
			if p.From != u.Path || p.First > 0 {
				continue
			}
			body.Public = append(body.Public, p)
			img := p.Image
			if len(p.Widths) > 0 {
				img.Width = slices.Max(p.Widths)
			}
			body.Private = append(body.Private, Private{Name: p.Name + "-open", From: p.From, To: p.Name + "-open/{name}.webp", Image: &img})
		}
	}
	return body, len(body.Uploads) > 0
}
