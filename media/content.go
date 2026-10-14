package media

import (
	"errors"
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
// version's body images (its Body uploads), one item per version ({kind}.body,
// content, version), which shows only to viewers who open the version. A
// locked version's body images never reach a viewer who has not opened it; an
// open version's keep public files. Versions share no files: each holds its
// own copy of its content's body images.
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
		if content {
			if err := downloads(mk, k.Kinds[mk.Name]); err != nil {
				return Config{}, fmt.Errorf("media: kind %q: %w", mk.Name, err)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(k.GroupKinds)) {
		if !slices.ContainsFunc(out.Kinds, func(mk Kind) bool { return mk.Name == name }) {
			out.Kinds = append(out.Kinds, GroupKind(name, k.GroupKinds[name]))
		}
	}
	return out, nil
}

// downloads puts a content kind's originals and the files leaving only as
// downloads in download/ (Kind.Downloads), and names what its download
// serves (Kind.Download): its page uploads and imports, its zip and what
// the zip packs, its videos' sources, and its served MP4. A default (k sets
// no Serve) the media cannot serve serves none; a configured one is an error.
func downloads(mk *Kind, k kinds.Kind) error {
	mk.Downloads = slices.Clone(mk.Downloads)
	add := func(d string) {
		if d != "" && !slices.Contains(mk.Downloads, d) {
			mk.Downloads = append(mk.Downloads, d)
		}
	}
	var zip, original string
	if p := mk.Publish; p != nil {
		add(p.Pages)
		add(p.Import)
		if z := mk.private(p.Download); z != nil && z.Zip != "" {
			zip = z.Name
			add(z.Name)
			for _, pr := range mk.Private {
				if pr.From == p.Pages && strings.HasPrefix(pr.To, z.Zip) {
					add(pr.Name)
				}
			}
		}
	}
	for _, pr := range mk.Private {
		if pr.HLS != nil && !strings.Contains(pr.From, nameVar) && mk.uploadIndex(pr.From) >= 0 {
			add(pr.From)
			if original == "" && mk.KeepOriginals {
				original = pr.From
			}
		}
	}
	explicit := k.Download != nil && k.Download.Serve != ""
	switch serve := k.Serves(); serve {
	case "":
		mk.Download = ""
	case kinds.DownloadPages:
		if mk.Download = zip; zip == "" && explicit {
			return errors.New("download serves pages, but Publish names no Zip Download")
		}
	case kinds.DownloadOriginal:
		if mk.Download = original; original == "" && explicit {
			return errors.New("download serves the original: it needs KeepOriginals and an HLS preset from a literal upload")
		}
	default:
		if pr := mk.private(serve); pr == nil || pr.MP4 == nil {
			return fmt.Errorf("download serves %q, which is no MP4 preset", serve)
		}
		add(serve)
		mk.Download = serve
	}
	return nil
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

// bodyKind is a content kind's body kind: its Body uploads, their public
// presets, and for each a private image the item token serves to a viewer
// who opened the version while it has no public files.
func bodyKind(k Kind) (Kind, bool, error) {
	body := Kind{Name: k.Name + BodySuffix, KeepOriginals: k.KeepOriginals}
	for _, u := range k.Uploads {
		if !u.Body {
			continue
		}
		if !u.Named || !slices.ContainsFunc(k.Public, func(p Public) bool { return p.From == u.Path && p.First == 0 }) {
			return Kind{}, false, fmt.Errorf("body upload %q needs Named and a public preset", u.Path)
		}
		u.Body = false
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
	return body, len(body.Uploads) > 0, nil
}
