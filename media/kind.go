package media

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"slices"
	"strconv"
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

// PublicNames are p's public names in m for the upload at path, one per
// width: none for an upload outside a preview's First. m may be nil for a
// preset without First.
func (k *Kind) PublicNames(m *Manifest, p *Public, path string) []string {
	vars := map[string]string{"name": k.NameOf(path)}
	if p.First > 0 {
		n := slices.Index(k.firsts(m, p), path)
		if n < 0 {
			return nil
		}
		vars["n"] = strconv.Itoa(n + 1)
	}
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

// firsts are the paths of the uploads preview p renders, in order: the
// first p.First attached uploads of its From.
func (k *Kind) firsts(m *Manifest, p *Public) []string {
	var out []string
	if m == nil {
		return nil
	}
	g := k.uploadIndex(p.From)
	for _, f := range m.Files {
		if len(out) == p.First {
			break
		}
		if i, _, _, _, ok := k.upload(f.Path); ok && i == g && f.IsUpload() && !f.Unattached {
			out = append(out, f.Path)
		}
	}
	return out
}

// rendered reports preview p rendered for the upload now at f's position:
// until then the name there holds another upload's image, or none.
func rendered(f File, p *Public) bool {
	return f.Blob != "" && f.Fail() == nil && !slices.Contains(f.Pending, p.Name)
}

// previewNames lists m's published preview generations in order.
func (k *Kind) previewNames(m *Manifest) []string {
	var out []string
	if m.Hidden {
		return nil
	}
	for i := range k.Public {
		p := &k.Public[i]
		for _, path := range k.firsts(m, p) {
			if f, _ := m.Get(path); rendered(f, p) {
				if pub, ok := k.Publication(m, f, p); ok && pub.Ready() {
					out = append(out, pub.NamesOnDisk()...)
				}
			}
		}
	}
	return out
}

// Publication returns the generation still owned by the source and position.
// A reservation need not be Ready: cleanup must protect it while PUTs run.
func (k *Kind) Publication(m *Manifest, f File, p *Public) (Publication, bool) {
	pub, ok := f.Publication(p.Name)
	return pub, ok && !m.Hidden && !f.Unattached && f.Fail() == nil && pub.Source == f.Key() &&
		slices.Equal(pub.Names, k.PublicNames(m, p, f.Path))
}

// PublicKept protects every active reservation and published generation.
// An unowned name is permanently retired; no later worker may reuse it.
func (k *Kind) PublicKept(m *Manifest) []string {
	var out []string
	if m.Hidden {
		return nil
	}
	for _, f := range m.Files {
		if !f.IsUpload() || f.Unattached {
			continue
		}
		for _, p := range k.PublicFor(f.Path) {
			if pub, ok := k.Publication(m, f, p); ok {
				out = append(out, pub.NamesOnDisk()...)
			}
		}
	}
	return out
}

// syncPreviews keeps each preview preset pending on exactly the uploads
// whose position's file is not the one before did (a reorder, an insert, a
// removal, a new source or edit); uploads past First carry none.
func (k *Kind) syncPreviews(before, m *Manifest) {
	for i := range k.Public {
		p := &k.Public[i]
		if p.First == 0 {
			continue
		}
		was := map[int]string{}
		if before != nil && !before.Hidden {
			for n, path := range k.firsts(before, p) {
				if f, ok := before.Get(path); ok && !slices.Contains(f.Pending, p.Name) {
					was[n] = f.Key()
				}
			}
		}
		now := k.firsts(m, p)
		g := k.uploadIndex(p.From)
		for j := range m.Files {
			f := &m.Files[j]
			if u, _, _, _, ok := k.upload(f.Path); !ok || u != g || !f.IsUpload() {
				continue
			}
			n := slices.Index(now, f.Path)
			pending := slices.Contains(f.Pending, p.Name)
			switch {
			case m.Hidden || n < 0 || f.Fail() != nil:
				if pending {
					f.Pending = slices.DeleteFunc(slices.Clone(f.Pending), func(s string) bool { return s == p.Name })
					if len(f.Pending) == 0 {
						f.Pending = nil
					}
				}
			case !pending && was[n] != f.Key():
				f.Pending = append(slices.Clone(f.Pending), p.Name)
			}
		}
	}
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
		if u.Named && u.Max == 0 {
			return fmt.Errorf("upload %q: a Named upload needs Max", u.Path)
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
	if p.First < 0 || p.First > maxFirst || p.First > 0 && (k.patterns[i].literal != "" || slices.Contains(vars, "name") || p.Default != "") {
		return fmt.Errorf("First (at most %d) needs a {name} upload, {n} in To for {name}, and no Default", maxFirst)
	}
	if slices.ContainsFunc(vars, func(s string) bool { return s != "name" && s != "w" && s != "n" }) ||
		slices.Contains(vars, "w") != (len(p.Widths) > 0) || slices.Contains(vars, "n") != (p.First > 0) ||
		!layout.ValidSegment(fill(p.To, map[string]string{"name": "n", "w": "1", "n": "1"})) {
		return fmt.Errorf("invalid To %q ([A-Za-z0-9._-] with {w} exactly when Widths are set and {n} exactly with First)", p.To)
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
	MetaLang    = "lang"    // subtitles and audio: BCP 47
	MetaLabel   = "label"   // a track's name
	MetaForced  = "forced"  // bool: a forced-narrative subtitle track
	MetaFor     = "for"     // a subtitle's video upload path; default every video of the item
	MetaCharset = "charset" // a subtitle's IANA charset, overriding detection
)

// Output entry sizes in manifest JSON, beyond their paths and preset name:
// a file's blob, type, size, dimensions and fingerprint, and a track's
// fields too (its label is at most 120 bytes of JSON) with the file name
// under its directory.
const (
	outputEntryBytes  = 320
	trackEntryBytes   = 640 + 24
	blobFieldsBytes   = 256 // what a staged upload or a frame gains: its blob, size and dimensions
	measuredBytes     = 48  // an upload's w and h, or dur, once measured
	failureEntryBytes = 768 // a Failure: its key, a message of maxFailureBytes, code and details
)

// Tracks per source a video's HLS ladder carries beyond its renditions: the
// probe keeps at most this many audio and text subtitle streams.
const (
	MaxAudioTracks    = 8
	MaxSubtitleTracks = 16
)

// Unwritten estimates the JSON m gains as the worker processes it. Per
// upload it is the larger of a failure record and what success writes: an
// entry for every output its presets have yet to write (an HLS ladder's
// tracks one by one: a rendition per rung and codec, the sprite, and as many
// audio and subtitle tracks as a source may carry; an Audio preset's track
// and M4A), its measured fields, and a staged upload's or frame's blob.
// Then the zips, and the public presets' pending names an unhide adds.
// Commits bound the manifest with it; an item whose outputs still overrun
// it is marked Full by the worker.
func (k *Kind) Unwritten(m *Manifest) int64 {
	written := map[[2]string]int64{}
	for _, f := range m.Files {
		if !f.IsUpload() {
			written[[2]string{f.From, f.Preset}]++
		}
	}
	var n int64
	for _, f := range m.Files {
		if !f.IsUpload() || f.Gone || f.Fail() != nil {
			continue
		}
		var out int64
		if f.Blob == "" {
			out += blobFieldsBytes
		}
		from := jsonLen(f.Path)
		public := k.PublicFor(f.Path)
		for _, p := range public {
			names := k.PublicNames(m, p, f.Path)
			if len(names) == 0 || m.Hidden || f.Unattached {
				continue
			}
			if _, ok := k.Publication(m, f, p); !ok {
				// Source, fingerprint and UUID; one logical name and measured
				// dimension pair per rendition. Replacement does not accumulate
				// old generations in the root.
				out += 256 + int64(len(p.Name))
				for _, name := range names {
					out += int64(jsonLen(name) + 64)
				}
			}
		}
		for _, p := range k.PrivateFor(f.Path) {
			entries, size := int64(1), int64(outputEntryBytes)
			switch {
			case p.HLS != nil:
				ladder := cmp.Or(len(p.HLS.Ladder), len(DefaultLadder))
				entries, size = int64(3*ladder+1+MaxAudioTracks+MaxSubtitleTracks), trackEntryBytes
			case p.Audio != nil:
				entries, size = 2, trackEntryBytes
			}
			if p.Download != "" {
				size += maxNameBytes + 16
			}
			if left := entries - written[[2]string{f.Path, p.Name}]; left > 0 {
				out += left * (size + int64(jsonLen(k.OutputPath(p, f.Path))+from+len(p.Name)))
			}
		}
		if out > 0 || len(f.Pending) > 0 { // work is left: it may measure the upload, or fail
			if f.W == 0 && f.Dur == 0 {
				out += measuredBytes
			}
			out = max(out, failureEntryBytes)
		}
		n += out
		if len(public) > 0 {
			if len(f.Pending) == 0 {
				n += int64(len(`,"pending":[]`))
			}
			for _, p := range public {
				n += int64(len(p.Name)) + 3
			}
		}
	}
	for i := range k.Private {
		if z := &k.Private[i]; z.Zip != "" && written[[2]string{z.Zip, z.Name}] == 0 {
			n += outputEntryBytes + maxNameBytes + 16 + int64(jsonLen(z.To)+jsonLen(z.Zip)+len(z.Name))
		}
	}
	return n
}
