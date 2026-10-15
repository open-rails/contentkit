package content

import (
	"context"
	"math"
	"net/http"
	"slices"

	"github.com/open-rails/contentkit/media"
)

// Poll images store the image's name ("i-{uuid}"), never its URL: a
// published file's name carries a generation that changes whenever it is
// published again, so reads resolve names to current public files.

// ImageInput names an inline image of the item's media folder ("i-{uuid}");
// "" clears where the route allows it.
type ImageInput struct {
	Image *string `json:"image"`
}

func decodeImage(req *http.Request) (string, error) {
	var in ImageInput
	if err := decodeJSON(req, &in); err != nil {
		return "", err
	}
	if in.Image == nil {
		return "", badRequest("image is required")
	}
	if *in.Image != "" && !media.ValidNamed(*in.Image) {
		return "", badRequest("image must be an inline image name (i-{uuid})")
	}
	return *in.Image, nil
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
		qs[i] = media.ImageQuery{Ref: s.rt.mediaRef(m.kind(k.f), k.id), Name: k.name}
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
