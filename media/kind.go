package media

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"slices"
	"strings"

	"github.com/open-rails/contentkit/media/layout"
)

// kindState is what NewRegistry compiles into a Kind.
type kindState struct {
	ns       string
	patterns []pattern // parallel to Uploads
}

// upload finds the Upload a path (stem plus optional extension) belongs to:
// a literal stem wins over a pattern. It returns the Upload's index, the
// stem, its {name} and the extension.
func (k *Kind) upload(path string) (i int, stem, name, ext string, ok bool) {
	stem, ext = splitExt(path)
	for _, literal := range []bool{true, false} {
		for j, p := range k.patterns {
			if (p.literal != "") != literal {
				continue
			}
			if n, ok := p.match(stem); ok {
				return j, stem, n, ext, true
			}
		}
	}
	return -1, "", "", "", false
}

// uploadIndex is the index of the Upload whose Path is from, or -1.
func (k *Kind) uploadIndex(from string) int {
	return slices.IndexFunc(k.Uploads, func(u Upload) bool { return u.Path == from })
}

func (k *Kind) private(name string) *Private {
	if i := slices.IndexFunc(k.Private, func(p Private) bool { return p.Name == name }); i >= 0 {
		return &k.Private[i]
	}
	return nil
}

func (k *Kind) public(name string) *Public {
	if i := slices.IndexFunc(k.Public, func(p Public) bool { return p.Name == name }); i >= 0 {
		return &k.Public[i]
	}
	return nil
}

// Presets are the names of the presets an upload at path feeds: the private
// ones, then (unless hidden) the public ones. Zips are not listed; they
// follow their inputs.
func (k *Kind) Presets(path string, hidden bool) []string {
	i, _, _, _, ok := k.upload(path)
	if !ok {
		return nil
	}
	var out []string
	for _, p := range k.Private {
		if p.From != "" && p.From == k.Uploads[i].Path {
			out = append(out, p.Name)
		}
	}
	for _, p := range k.Public {
		if !hidden && p.From == k.Uploads[i].Path {
			out = append(out, p.Name)
		}
	}
	return out
}

// PrivateFor lists the private presets fed by the upload at path.
func (k *Kind) PrivateFor(path string) []*Private {
	i, _, _, _, ok := k.upload(path)
	if !ok {
		return nil
	}
	var out []*Private
	for j := range k.Private {
		if p := &k.Private[j]; p.From != "" && p.From == k.Uploads[i].Path {
			out = append(out, p)
		}
	}
	return out
}

// PublicFor lists the public presets fed by the upload at path.
func (k *Kind) PublicFor(path string) []*Public {
	i, _, _, _, ok := k.upload(path)
	if !ok {
		return nil
	}
	var out []*Public
	for j := range k.Public {
		if p := &k.Public[j]; p.From == k.Uploads[i].Path {
			out = append(out, p)
		}
	}
	return out
}

// NameOf is the {name} of an upload path ("originals/001.png" is "001",
// "cover.png" is "cover").
func (k *Kind) NameOf(path string) string {
	_, _, name, _, _ := k.upload(path)
	return name
}

// OutputPath is p's output path (a file, or a directory ending in "/") for
// the upload at path.
func (k *Kind) OutputPath(p *Private, path string) string {
	return fill(p.To, map[string]string{"name": k.NameOf(path)})
}

// PublicNames are p's public names for the upload at path, one per width.
func (k *Kind) PublicNames(p *Public, path string) []string {
	vars := map[string]string{"name": k.NameOf(path)}
	if len(p.Widths) == 0 {
		return []string{fill(p.To, vars)}
	}
	out := make([]string, len(p.Widths))
	for i, w := range p.Widths {
		vars["w"] = fmt.Sprint(w)
		out[i] = fill(p.To, vars)
	}
	return out
}

// EditBounds is the image that bounds an edit of the upload at path: its
// first public preset's, else a native one.
func (k *Kind) EditBounds(path string) Image {
	if ps := k.PublicFor(path); len(ps) > 0 {
		return ps[0].Image
	}
	return Image{}
}

func (k *Kind) validate() error {
	if !layout.ValidSegment(k.Name) || strings.HasPrefix(k.Name, "_") {
		return errors.New("invalid name")
	}
	if len(k.Uploads) == 0 {
		return errors.New("no uploads")
	}
	k.patterns = make([]pattern, len(k.Uploads))
	for i, u := range k.Uploads {
		p, ok := parsePattern(u.Path)
		if !ok || slices.ContainsFunc(k.Uploads[:i], func(o Upload) bool { return o.Path == u.Path }) {
			return fmt.Errorf("upload %q: invalid or duplicate path", u.Path)
		}
		if len(u.Types) == 0 || u.MaxBytes <= 0 || u.Max < 0 {
			return fmt.Errorf("upload %q: needs Types and MaxBytes", u.Path)
		}
		if u.Named && p.literal != "" {
			return fmt.Errorf("upload %q: only a pattern is Named", u.Path)
		}
		k.patterns[i] = p
	}
	for _, u := range k.Uploads {
		if err := u.Video.validate(u.Types); err != nil {
			return fmt.Errorf("upload %q: %w", u.Path, err)
		}
	}
	for _, u := range k.Uploads {
		if k.feedsImages(u.Path) {
			if i := slices.IndexFunc(u.Types, func(t string) bool { return !slices.Contains(ImageTypes, t) }); i >= 0 {
				return fmt.Errorf("upload %q: its image presets cannot decode %s (ImageTypes)", u.Path, u.Types[i])
			}
		}
		if u.Frames == "" {
			continue
		}
		j := k.uploadIndex(u.Frames)
		if j < 0 || k.patterns[j].literal == "" || !slices.ContainsFunc(k.Uploads[j].Types, isVideoType) {
			return fmt.Errorf("upload %q: Frames %q is not a literal video upload", u.Path, u.Frames)
		}
	}
	names := map[string]bool{}
	for _, p := range k.Private {
		if err := k.validatePrivate(p, names); err != nil {
			return fmt.Errorf("private %q: %w", p.Name, err)
		}
	}
	for i := range k.Public {
		if err := k.validatePublic(&k.Public[i], names); err != nil {
			return fmt.Errorf("public %q: %w", k.Public[i].Name, err)
		}
	}
	return nil
}

func (l *VideoLimits) validate(types []string) error {
	if l == nil {
		return nil
	}
	if !slices.ContainsFunc(types, func(t string) bool { return isVideoType(t) || isAudioType(t) }) {
		return errors.New("video limits on an upload of neither video nor audio")
	}
	finite := func(v float64) bool { return v >= 0 && !math.IsInf(v, 0) && !math.IsNaN(v) }
	if !finite(l.MaxSeconds) || !finite(l.MaxFPS) || l.MaxFPS > 60 || l.MaxPixels < 0 || !finite(l.MaxWork) {
		return fmt.Errorf("invalid Video limits %+v (non-negative, MaxFPS at most 60)", *l)
	}
	return nil
}

// feedsImages reports an upload some image preset (private or public) decodes.
func (k *Kind) feedsImages(path string) bool {
	return slices.ContainsFunc(k.Private, func(p Private) bool { return p.Image != nil && p.From == path }) ||
		slices.ContainsFunc(k.Public, func(p Public) bool { return p.From == path })
}

func (k *Kind) validatePrivate(p Private, names map[string]bool) error {
	if !layout.ValidSegment(p.Name) || names[p.Name] {
		return errors.New("invalid or duplicate name")
	}
	names[p.Name] = true
	producers := 0
	for _, set := range []bool{p.Image != nil, p.HLS != nil, p.MP4 != nil, p.Zip != "", p.Audio != nil, p.Subtitles != nil} {
		if set {
			producers++
		}
	}
	if producers != 1 {
		return errors.New("needs exactly one producer")
	}
	if p.Zip != "" {
		if p.From != "" || !validDir(p.Zip) {
			return errors.New("a Zip takes no From and a directory prefix")
		}
	} else if k.uploadIndex(p.From) < 0 {
		return fmt.Errorf("From %q is no upload path", p.From)
	}
	dir := p.HLS != nil || p.Audio != nil
	if !validTemplate(p.To, dir) || slices.ContainsFunc(placeholders(p.To), func(s string) bool { return s != "name" }) {
		return fmt.Errorf("invalid To %q (a path with {name} only; a directory ending in / for HLS and Audio)", p.To)
	}
	if slices.Contains(placeholders(p.Download), "") {
		return fmt.Errorf("invalid Download %q", p.Download)
	}
	// Outputs never land on upload paths: "subs/{name}" uploads cannot
	// derive "subs/{name}.vtt" (a .vtt sidecar would be its own output).
	sample := fill(p.To, map[string]string{"name": "x"})
	if dir {
		sample += "x"
	}
	if _, _, _, _, ok := k.upload(sample); ok {
		return fmt.Errorf("To %q overlaps an upload path", p.To)
	}
	switch {
	case p.Image != nil:
		return p.Image.validate()
	case p.HLS != nil:
		return p.HLS.validate()
	case p.MP4 != nil:
		if p.MP4.Rung < 2 || p.MP4.Rung > 4320 || p.MP4.Rung%2 != 0 || !validProfile(p.MP4.Profile) {
			return fmt.Errorf("invalid MP4 rung %d or profile", p.MP4.Rung)
		}
	case p.Audio != nil:
		if l := p.Audio.Loudness; l != 0 && (l < -70 || l > -5) {
			return fmt.Errorf("invalid loudness %g LUFS", l)
		}
	}
	return nil
}

func (k *Kind) validatePublic(p *Public, names map[string]bool) error {
	if !layout.ValidSegment(p.Name) || names[p.Name] {
		return errors.New("invalid or duplicate name")
	}
	names[p.Name] = true
	i := k.uploadIndex(p.From)
	if i < 0 {
		return fmt.Errorf("From %q is no upload path", p.From)
	}
	vars := placeholders(p.To)
	if slices.Contains(vars, "name") && (k.patterns[i].literal == "" && !k.Uploads[i].Named || p.Default != "") {
		return errors.New("{name} in To needs a literal or Named upload, and no Default")
	}
	if slices.ContainsFunc(vars, func(s string) bool { return s != "name" && s != "w" }) ||
		slices.Contains(vars, "w") != (len(p.Widths) > 0) ||
		!layout.ValidSegment(fill(p.To, map[string]string{"name": "n", "w": "1"})) {
		return fmt.Errorf("invalid To %q ([A-Za-z0-9._-] with {w} exactly when Widths are set)", p.To)
	}
	if p.Default != "" && k.Defaults != nil {
		if _, err := fs.Stat(k.Defaults, p.Default); err != nil {
			return fmt.Errorf("Default %q: %w", p.Default, err)
		}
	}
	if len(p.Widths) > 0 && (p.Image.Width > 0) != (p.Image.Height > 0) {
		return errors.New("with Widths, Image.Width and Height are the names' shape: set both or neither")
	}
	p.Widths = slices.Sorted(slices.Values(p.Widths))
	for j, w := range p.Widths {
		if w <= 0 || w > maxWidth || j > 0 && w == p.Widths[j-1] {
			return fmt.Errorf("invalid width %d", w)
		}
	}
	return p.Image.validate()
}

// validTemplate accepts a relative app path: no empty, "." or ".." segment;
// a directory ends in "/".
func validTemplate(t string, dir bool) bool {
	if t == "" || strings.HasSuffix(t, "/") != dir || strings.HasPrefix(t, "/") {
		return false
	}
	for _, s := range strings.Split(strings.TrimSuffix(t, "/"), "/") {
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "\\") {
			return false
		}
	}
	return true
}

func (im Image) validate() error {
	if im.Width < 0 || im.Height < 0 || im.Width > 16384 || im.Height > 16384 || im.Quality < 0 || im.Quality > 100 ||
		im.Blur < 0 || im.MinWidth < 0 || !im.Aspect.Valid() ||
		im.Fit != FitInside && im.Fit != FitCover || im.Animation != AnimationAllow && im.Animation != AnimationReject {
		return fmt.Errorf("invalid image spec %+v", im)
	}
	return nil
}

// Rungs is the ladder in effect.
func (h *HLS) Rungs() []int {
	if h == nil || len(h.Ladder) == 0 {
		return DefaultLadder
	}
	return h.Ladder
}

// Aspects are the aspect bounds in effect.
func (h *HLS) Aspects() (lo, hi float64) {
	lo, hi = DefaultMinAspect, DefaultMaxAspect
	if h != nil && h.MinAspect > 0 {
		lo = h.MinAspect
	}
	if h != nil && h.MaxAspect > 0 {
		hi = h.MaxAspect
	}
	return lo, hi
}

func (h *HLS) validate() error {
	for i, n := range h.Ladder {
		if n < 2 || n > 4320 || n%2 != 0 || i > 0 && n >= h.Ladder[i-1] {
			return fmt.Errorf("invalid ladder %v", h.Ladder)
		}
	}
	if lo, hi := h.Aspects(); h.MinAspect < 0 || h.MaxAspect < 0 || lo > 1 || hi < 1 {
		return fmt.Errorf("invalid aspect bounds %g-%g", lo, hi)
	}
	if !validProfile(h.Profile) {
		return fmt.Errorf("unknown profile %q", h.Profile)
	}
	return nil
}

func validProfile(p string) bool { return p == VideoLive || p == VideoAnimation }

func isVideoType(t string) bool    { return strings.HasPrefix(t, "video/") }
func isImageType(t string) bool    { return strings.HasPrefix(t, "image/") }
func isAudioType(t string) bool    { return strings.HasPrefix(t, "audio/") }
func isSubtitleType(t string) bool { return slices.Contains(SubtitleTypes, t) }

// ImageTypes are the image types the image presets decode (media/image); an
// upload feeding one accepts no other. SVG, BMP and JPEG XL are refused:
// their libvips loaders are blocked as untrusted.
var ImageTypes = []string{"image/jpeg", "image/png", "image/webp", "image/gif", "image/avif", "image/heic", "image/heif", "image/tiff"}

// SubtitleTypes are the subtitle types the Subtitles producer converts.
var SubtitleTypes = []string{"text/vtt", "application/x-subrip", "text/x-ssa", "text/x-ass"}

// Upload meta keys ContentKit reads (put's meta).
const (
	MetaTeaser  = "teaser"  // bool: served to every viewer who can see the item
	MetaLang    = "lang"    // subtitles and audio: BCP 47
	MetaLabel   = "label"   // a track's name
	MetaForced  = "forced"  // bool: a forced-narrative subtitle track
	MetaFor     = "for"     // a subtitle's video upload path; default every video of the item
	MetaCharset = "charset" // a subtitle's IANA charset, overriding detection
)
