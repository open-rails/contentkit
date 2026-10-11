package content

import (
	"context"
	"math"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/media"
)

// A post body references its images, never their URLs: a published file's
// name carries a generation that changes whenever it is published again
// (regenerated, or hidden and shown), so a stored URL goes stale and a draft
// has no public file at all. "contentkit:i-{uuid}" names an inline image of
// the post's media folder wherever the body's format puts a URL (an <img
// src>, a Quill image embed); reads resolve it to the image's current public
// file. Covers and poll images store the name itself.

// ImageScheme is the URL scheme of an image reference. A host's
// ContentProcessor must keep it where it keeps image URLs.
const ImageScheme = "contentkit:"

var imageRefRe = regexp.MustCompile(ImageScheme + `(i-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

// ImageRef is the body reference to an inline image name ("i-{uuid}").
func ImageRef(name string) string { return ImageScheme + name }

// imageNames lists the names body references, in order, once each.
func imageNames(body string) []string {
	var out []string
	for _, m := range imageRefRe.FindAllStringSubmatch(body, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// resolveBody replaces each reference to a name in urls with its URL; the
// rest stay references.
func resolveBody(body string, urls map[string]string) string {
	if len(urls) == 0 {
		return body
	}
	return imageRefRe.ReplaceAllStringFunc(body, func(ref string) string {
		if u, ok := urls[ref[len(ImageScheme):]]; ok {
			return u
		}
		return ref
	})
}

type imageKey struct {
	f        folder
	id, name string
}

// images collects the images one response shows and resolves them with one
// projection lookup (MediaImages.Images).
type images struct {
	rt   *Runtime
	keys []imageKey
	urls map[imageKey]string
}

func (rt *Runtime) images() *images { return &images{rt: rt, urls: map[imageKey]string{}} }

func (s *images) add(f folder, id, name string) {
	k := imageKey{f, id, name}
	if name != "" && !slices.Contains(s.keys, k) {
		s.keys = append(s.keys, k)
	}
}

// resolve looks every added image up at once. Without Media nothing resolves.
func (s *images) resolve(ctx context.Context) error {
	m := s.rt.media
	if m == nil || len(s.keys) == 0 {
		return nil
	}
	qs := make([]media.ImageQuery, len(s.keys))
	for i, k := range s.keys {
		qs[i] = media.ImageQuery{Ref: s.rt.Ref(m.kind(k.f), k.id), Name: k.name}
	}
	found, err := m.Images.Images(ctx, qs...)
	if err != nil {
		return err
	}
	for i, k := range s.keys {
		if u, ok := widest(found[i]); ok {
			s.urls[k] = u
		}
	}
	return nil
}

// url is an image's current public URL; false while it has none.
func (s *images) url(f folder, id, name string) (string, bool) {
	u, ok := s.urls[imageKey{f, id, name}]
	return u, ok
}

// widest is the widest rendition of an upload's first current image.
func widest(found []media.PublicImage) (string, bool) {
	if len(found) == 0 || len(found[0].Renditions) == 0 {
		return "", false
	}
	return found[0].URL(math.MaxInt), true
}

// ResolvePosts resolves the posts' images with one projection lookup: each
// reference in Body (ImageRef) to an image with a current public file
// becomes its URL, CoverURL is Cover's, and Images maps every resolved name
// to its URL. A reference without a current file (an unpublished post, an
// image still rendering) stays as it is, CoverURL nil. Body is the stored
// body; the post routes answer posts resolved. Hosts reading content_posts
// themselves resolve with it before rendering.
func (rt *Runtime) ResolvePosts(ctx context.Context, posts []Post) error {
	set := rt.images()
	for i := range posts {
		p := &posts[i]
		for _, name := range imageNames(p.Body) {
			set.add(postFolder, p.ID, name)
		}
		if p.Cover != nil {
			set.add(postFolder, p.ID, *p.Cover)
		}
	}
	if err := set.resolve(ctx); err != nil {
		return err
	}
	for i := range posts {
		p := &posts[i]
		p.Images, p.CoverURL = nil, nil
		note := func(name string) (string, bool) {
			u, ok := set.url(postFolder, p.ID, name)
			if ok {
				if p.Images == nil {
					p.Images = map[string]string{}
				}
				p.Images[name] = u
			}
			return u, ok
		}
		for _, name := range imageNames(p.Body) {
			note(name)
		}
		if p.Cover != nil {
			if u, ok := note(*p.Cover); ok {
				p.CoverURL = &u
			}
		}
		p.Body = resolveBody(p.Body, p.Images)
	}
	return nil
}

// previewURL shows a post's image to its editor now: its public file, else
// its editor view once rendered (the read asks for it). "" while neither
// exists or without a Media.Reader.
func (rt *Runtime) previewURL(ctx context.Context, editor access.Actor, id, name string) (string, error) {
	set := rt.images()
	set.add(postFolder, id, name)
	if err := set.resolve(ctx); err != nil {
		return "", err
	}
	if u, ok := set.url(postFolder, id, name); ok {
		return u, nil
	}
	if rt.media.Reader == nil {
		return "", nil
	}
	read, err := rt.media.Reader.Read(ctx, rt.Ref(rt.media.PostKind, id), editor, media.ReadOptions{Editor: true, Limit: math.MaxInt})
	if err != nil {
		return "", err
	}
	for _, f := range read.Files {
		if f.Upload && strings.TrimSuffix(path.Base(f.Path), path.Ext(f.Path)) == name {
			return f.EditorURL, nil
		}
	}
	return "", nil
}
