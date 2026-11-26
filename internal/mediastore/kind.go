package mediastore

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path"
	"slices"
	"strings"

	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// RegisteredKind is a Kind NewRegistry validated and placed in its
// namespace and scope.
type RegisteredKind struct {
	media.Kind
	ns       string
	scope    layout.Scope
	patterns []pattern // parallel to Uploads
}

// levels validates a Versions kind's downloads and body images and derives
// its reader publication configuration.
func (k *RegisteredKind) levels() error {
	if !k.Versions {
		if slices.ContainsFunc(k.Uploads, func(u media.Upload) bool { return u.Body }) || k.Publish != nil {
			return errors.New("Body uploads and Publish need Versions")
		}
		return nil
	}
	if err := k.validateDownload(); err != nil {
		return err
	}
	for _, u := range k.Uploads {
		if !u.Body {
			continue
		}
		// Body images remain private and are selected by the reader grant.
		image := slices.ContainsFunc(k.Private, func(p media.Private) bool { return p.From == u.Path && p.Image != nil && !p.HostOnly })
		switch {
		case !u.Named || !image:
			return fmt.Errorf("body upload %q needs Named and a private image preset", u.Path)
		case slices.ContainsFunc(k.Public, func(p media.Public) bool { return p.From == u.Path }):
			return fmt.Errorf("body upload %q: body images have no public files", u.Path)
		}
	}
	if k.Publish == nil {
		return nil
	}
	return k.validatePublish()
}

// validatePublish checks the existing version renditions; it creates no media
// kind or storage folder. Chapter boundaries are reader metadata only.
func (k *RegisteredKind) validatePublish() error {
	p := k.Publish
	pages := k.uploadIndex(p.Pages)
	if pages < 0 || k.patterns[pages].literal != "" || k.Uploads[pages].Named || !k.feedsImages(p.Pages) {
		return fmt.Errorf("Pages %q is no {name} image upload of versions", p.Pages)
	}
	if len(p.Renditions) == 0 {
		return errors.New("needs Renditions")
	}
	for _, name := range p.Renditions {
		r := k.private(name)
		if r == nil || r.Image == nil || r.From != p.Pages || !strings.HasSuffix(r.To, nameVar+".webp") {
			return fmt.Errorf("rendition %q is no image preset from Pages to {dir}{name}.webp", name)
		}
	}
	if p.Playback != "" {
		i := k.uploadIndex(p.Playback)
		if i < 0 || k.patterns[i].literal != "" || slices.ContainsFunc(k.Uploads[i].Types, func(t string) bool { return !isAudioType(t) }) {
			return fmt.Errorf("Playback %q is no audio upload", p.Playback)
		}
	}
	if p.Import != "" {
		i := k.uploadIndex(p.Import)
		if i < 0 || k.patterns[i].literal != "" || !slices.Equal(k.Uploads[i].Types, []string{"application/zip"}) {
			return fmt.Errorf("Import %q is no {name} upload of application/zip", p.Import)
		}
	}
	if p.Download != "" {
		if z := k.private(p.Download); z == nil || z.Image == nil || z.Image.Format != media.ImageFormatSource || !z.HostOnly || z.From != p.Pages {
			return fmt.Errorf("Download %q is no HostOnly source-format Image preset from Pages", p.Download)
		}
	}
	return nil
}

// upload finds the Upload a path (stem plus optional extension) belongs to:
// a literal stem wins over a pattern. It returns the Upload's index, the
// stem, its {name} and the extension.
func (k *RegisteredKind) upload(path string) (i int, stem, name, ext string, ok bool) {
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
func (k *RegisteredKind) uploadIndex(from string) int { return findUpload(k.Uploads, from) }

func (k *RegisteredKind) private(name string) *media.Private { return findPrivate(k.Private, name) }

func (k *RegisteredKind) public(name string) *media.Public {
	if i := slices.IndexFunc(k.Public, func(p media.Public) bool { return p.Name == name }); i >= 0 {
		return &k.Public[i]
	}
	return nil
}

// Presets are the names of the presets an upload at path feeds: the private
// ones, then (unless unattached) the public ones. Zips are not listed; they
// follow their inputs.
func (k *RegisteredKind) Presets(path string, unattached bool) []string {
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
		if !unattached && p.From == k.Uploads[i].Path {
			out = append(out, p.Name)
		}
	}
	return out
}

// PrivateFor lists the private presets fed by the upload at path.
func (k *RegisteredKind) PrivateFor(path string) []*media.Private {
	i, _, _, _, ok := k.upload(path)
	if !ok {
		return nil
	}
	var out []*media.Private
	for j := range k.Private {
		if p := &k.Private[j]; p.From != "" && p.From == k.Uploads[i].Path {
			out = append(out, p)
		}
	}
	return out
}

// PublicFor lists the public presets fed by the upload at path.
func (k *RegisteredKind) PublicFor(path string) []*media.Public {
	i, _, _, _, ok := k.upload(path)
	if !ok {
		return nil
	}
	var out []*media.Public
	for j := range k.Public {
		if p := &k.Public[j]; p.From == k.Uploads[i].Path {
			out = append(out, p)
		}
	}
	return out
}

// NameOf is the {name} of an upload path ("originals/001.png" is "001",
// "cover.png" is "cover").
func (k *RegisteredKind) NameOf(path string) string {
	_, _, name, _, _ := k.upload(path)
	return name
}

// AudioUpload is an attached audio asset declared by this kind. It may be
// played with an ordinary media grant without exposing other original uploads.
func (k *RegisteredKind) AudioUpload(f media.File) bool {
	return !f.Unattached && k.audioOriginal(f)
}

func (k *RegisteredKind) audioOriginal(f media.File) bool {
	if !f.IsUpload() || f.Gone || f.Blob == "" || !isAudioType(f.Type) {
		return false
	}
	i, _, _, _, ok := k.upload(f.Path)
	return ok && slices.Contains(k.Uploads[i].Types, f.Type)
}

// OutputPath is p's output path (a file, or a directory ending in "/") for
// the upload at path.
func (k *RegisteredKind) OutputPath(p *media.Private, uploadPath string) string {
	return fill(p.To, map[string]string{"name": k.NameOf(uploadPath), "ext": path.Ext(uploadPath)})
}

// PublicNames are p's public names for the upload at path, one per width.
func (k *RegisteredKind) PublicNames(p *media.Public, path string) []string {
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

// PublicOutputs are the current outputs of public preset p from upload f:
// its widths in order. A failed or unattached upload shows none.
func (k *RegisteredKind) PublicOutputs(m *Manifest, f media.File, p *media.Public) []media.File {
	if f.Unattached || f.Fail() != nil || f.Gone {
		return nil
	}
	return m.Outputs(f.Path, p.Name)
}

// UploadRules are the kind's upload paths with the rules a client checks.
func (k *RegisteredKind) UploadRules() []media.UploadRule {
	out := make([]media.UploadRule, 0, len(k.Uploads))
	for _, u := range k.Uploads {
		r := media.UploadRule{Path: u.Path, Types: slices.Clone(u.Types), MaxBytes: u.MaxBytes, Max: u.Max, MaxPixels: u.MaxPixels, Named: u.Named, Body: u.Body, Frames: u.Frames}
		r.Page = k.Publish != nil && u.Path == k.Publish.Pages
		if i := slices.IndexFunc(k.Public, func(p media.Public) bool { return p.From == u.Path }); i >= 0 {
			r.Aspect, r.MinWidth = k.Public[i].Image.Aspect, k.Public[i].Image.MinWidth
		}
		if slices.ContainsFunc(u.Types, isVideoType) {
			lim := u.Video.Limits()
			r.Video = &lim
		}
		for _, p := range k.Private {
			if p.From != u.Path || p.HLS == nil {
				continue
			}
			lo, hi := p.HLS.Aspects()
			if r.MinAspect == 0 || lo > r.MinAspect {
				r.MinAspect = lo
			}
			if r.MaxAspect == 0 || hi < r.MaxAspect {
				r.MaxAspect = hi
			}
		}
		out = append(out, r)
	}
	return out
}

// EditBounds is the image that bounds an edit of the upload at path: its
// first public preset's, else a native one.
func (k *RegisteredKind) EditBounds(path string) media.Image {
	if ps := k.PublicFor(path); len(ps) > 0 {
		return ps[0].Image
	}
	return media.Image{}
}

func (k *RegisteredKind) validate() error {
	if !layout.ValidSegment(k.Name) || strings.HasPrefix(k.Name, "_") {
		return errors.New("invalid name")
	}
	if len(k.Uploads) == 0 {
		return errors.New("no uploads")
	}
	k.patterns = make([]pattern, len(k.Uploads))
	for i, u := range k.Uploads {
		p, ok := parsePattern(u.Path)
		if !ok || slices.ContainsFunc(k.Uploads[:i], func(o media.Upload) bool { return o.Path == u.Path }) {
			return fmt.Errorf("upload %q: invalid or duplicate path", u.Path)
		}
		if len(u.Types) == 0 || u.MaxBytes <= 0 || u.Max < 0 || u.MaxPixels < 0 {
			return fmt.Errorf("upload %q: needs Types and MaxBytes; Max and MaxPixels are not negative", u.Path)
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
		if err := validateVideo(u.Video, u.Types); err != nil {
			return fmt.Errorf("upload %q: %w", u.Path, err)
		}
	}
	for _, u := range k.Uploads {
		if k.feedsImages(u.Path) {
			if i := slices.IndexFunc(u.Types, func(t string) bool { return !slices.Contains(media.ImageTypes, t) }); i >= 0 {
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
		if !slices.Contains(u.Types, "image/png") {
			return fmt.Errorf("upload %q: Frames needs image/png in Types (a grabbed frame is a PNG)", u.Path)
		}
	}
	names := map[string]bool{}
	for _, p := range k.Private {
		if err := k.validatePrivate(p, names); err != nil {
			return fmt.Errorf("private %q: %w", p.Name, err)
		}
	}
	if err := k.validateDownload(); err != nil {
		return err
	}
	for i := range k.Public {
		if err := k.validatePublic(&k.Public[i], names); err != nil {
			return fmt.Errorf("public %q: %w", k.Public[i].Name, err)
		}
	}
	return nil
}

// validateDownload checks Download.
func (k *RegisteredKind) validateDownload() error {
	if d := k.Download; d != "" {
		p := k.private(d)
		pages := k.Publish != nil && d == k.Publish.Pages
		if i := k.uploadIndex(d); p != nil || !pages && (i < 0 || strings.Contains(d, nameVar)) {
			return fmt.Errorf("download %q is no literal upload or Publish's Pages", d)
		}
	}
	return nil
}

// ReaderOutput reports a derived file readers may get as a file: of a
// page, only the kind's reader renditions (Publish.Renditions); never a
// HostOnly preset's or the download rendition's (Publish.Download).
func (k *RegisteredKind) ReaderOutput(m *Manifest, f media.File) bool {
	if f.IsUpload() || k.downloadOnly(f.Preset) {
		return false
	}
	src, ok := m.Get(f.From)
	if !ok || k.Publish == nil || !k.IsPageUpload(src) {
		return true
	}
	return slices.Contains(k.Publish.Renditions, f.Preset)
}

// downloadOnly reports a preset readers never get as a file: a HostOnly
// one, or the reader kind's download rendition (Publish.Download), which
// only the ZIP carries.
func (k *RegisteredKind) downloadOnly(preset string) bool {
	if p := k.private(preset); p != nil && p.HostOnly {
		return true
	}
	return preset != "" && k.Publish != nil && k.Publish.Download == preset
}

// Area is where an upload (preset "") or a derived file is first written:
// a public preset's in public/; a version's in internal/, until its release
// moves it; another kind's in private/, but internal/ for a HostOnly
// preset's, which no item token opens.
func (k *RegisteredKind) Area(preset string) string {
	switch {
	case k.public(preset) != nil:
		return layout.AreaPublic
	case k.Versions:
		return layout.AreaInternal // released by moving it (Release)
	}
	if p := k.private(preset); p != nil && p.HostOnly {
		return layout.AreaInternal
	}
	return layout.AreaPrivate
}

func validateVideo(l *media.VideoLimits, types []string) error {
	if l == nil {
		return nil
	}
	if !slices.ContainsFunc(types, isVideoType) {
		return errors.New("video limits on an upload without video")
	}
	finite := func(v float64) bool { return v >= 0 && !math.IsInf(v, 0) && !math.IsNaN(v) }
	if !finite(l.MaxSeconds) || !finite(l.MaxFPS) || l.MaxFPS > 60 || l.MaxPixels < 0 || !finite(l.MaxWork) {
		return fmt.Errorf("invalid Video limits %+v (non-negative, MaxFPS at most 60)", *l)
	}
	return nil
}

// feedsImages reports an upload some image preset (private or public) decodes.
func (k *RegisteredKind) feedsImages(path string) bool {
	return slices.ContainsFunc(k.Private, func(p media.Private) bool { return p.Image != nil && p.From == path }) ||
		slices.ContainsFunc(k.Public, func(p media.Public) bool { return p.From == path })
}

func (k *RegisteredKind) validatePrivate(p media.Private, names map[string]bool) error {
	if !layout.ValidSegment(p.Name) || names[p.Name] {
		return errors.New("invalid or duplicate name")
	}
	names[p.Name] = true
	producers := 0
	for _, set := range []bool{p.Image != nil, p.HLS != nil, p.Subtitles != nil} {
		if set {
			producers++
		}
	}
	if producers != 1 {
		return errors.New("needs exactly one producer")
	}
	if k.uploadIndex(p.From) < 0 {
		return fmt.Errorf("From %q is no upload path", p.From)
	}
	dir := p.HLS != nil
	sourceFormat := p.Image != nil && p.Image.Format == media.ImageFormatSource
	if !validTemplate(p.To, dir) || slices.ContainsFunc(placeholders(p.To), func(s string) bool { return s != "name" && !(sourceFormat && s == "ext") }) {
		return fmt.Errorf("invalid To %q (a path with {name}; source-format images may also use {ext})", p.To)
	}
	if sourceFormat && !strings.HasSuffix(p.To, "{ext}") {
		return fmt.Errorf("source-format image To %q must retain the source extension with {ext}", p.To)
	}
	if sourceFormat && !p.Image.KeepMetadata {
		if i := slices.IndexFunc(k.Uploads[k.uploadIndex(p.From)].Types, func(t string) bool { return !slices.Contains(media.StrippableTypes, t) }); i >= 0 {
			return fmt.Errorf("source-format copies of %s cannot drop their metadata losslessly: set Image.KeepMetadata or drop the type", k.Uploads[k.uploadIndex(p.From)].Types[i])
		}
	}
	if slices.Contains(placeholders(p.Download), "") {
		return fmt.Errorf("invalid Download %q", p.Download)
	}
	// Outputs never land on upload paths: "subs/{name}" uploads cannot
	// derive "subs/{name}.vtt" (a .vtt sidecar would be its own output).
	sample := fill(p.To, map[string]string{"name": "x", "ext": ".png"})
	if dir {
		sample += "x"
	}
	if _, _, _, _, ok := k.upload(sample); ok {
		return fmt.Errorf("To %q overlaps an upload path", p.To)
	}
	if len(p.Shapes) > 0 && p.Image == nil {
		return errors.New("Shapes need an Image producer")
	}
	if p.HostOnly && p.Image == nil {
		return errors.New("HostOnly is for Image presets: an HLS ladder or caption track left out of playlists plays nowhere")
	}
	switch {
	case p.Image != nil:
		if err := validateImage(*p.Image); err != nil {
			return err
		}
		return validateShapes(*p.Image, p.Shapes)
	case p.HLS != nil:
		return validateHLS(p.HLS)
	}
	return nil
}

func (k *RegisteredKind) validatePublic(p *media.Public, names map[string]bool) error {
	if p.Image.Format != media.ImageFormatWebP {
		return errors.New("public image presets use WebP; source format is private-only")
	}
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
	return validateImage(p.Image)
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

func validateImage(im media.Image) error {
	if im.Format != media.ImageFormatWebP && im.Format != media.ImageFormatSource {
		return fmt.Errorf("invalid image format %q", im.Format)
	}
	if im.KeepMetadata && im.Format != media.ImageFormatSource {
		return errors.New("KeepMetadata is for source-format images; re-encoded ones carry no metadata")
	}
	if im.Width < 0 || im.Height < 0 || im.Width > 16384 || im.Height > 16384 || im.Quality < 0 || im.Quality > 100 ||
		im.MinWidth < 0 || !im.Aspect.Valid() ||
		im.Fit != media.FitInside && im.Fit != media.FitCover || im.Animation != media.AnimationAllow && im.Animation != media.AnimationReject {
		return fmt.Errorf("invalid image spec %+v", im)
	}
	return nil
}

// validateShapes requires bounded, nonempty, disjoint shapes whose images
// keep base's format and animation.
func validateShapes(base media.Image, shapes []media.Shape) error {
	for i, s := range shapes {
		if s.WiderThan.Native() && s.NoWiderThan.Native() || !s.WiderThan.Valid() || !s.NoWiderThan.Valid() ||
			!spans(s.WiderThan, s.NoWiderThan) {
			return fmt.Errorf("shape %d: set WiderThan, NoWiderThan or both (reduced W:H), the first narrower", i)
		}
		if s.Image.Format != base.Format || s.Image.Animation != base.Animation || s.Image.KeepMetadata != base.KeepMetadata {
			return fmt.Errorf("shape %d: keep the preset's format, animation and metadata policy", i)
		}
		if err := validateImage(s.Image); err != nil {
			return fmt.Errorf("shape %d: %w", i, err)
		}
		for j, o := range shapes[:i] {
			if spans(s.WiderThan, o.NoWiderThan) && spans(o.WiderThan, s.NoWiderThan) {
				return fmt.Errorf("shapes %d and %d overlap", j, i)
			}
		}
	}
	return nil
}

func validateHLS(h *media.HLS) error {
	for i, n := range h.Ladder {
		if n < 2 || n > 4320 || n%2 != 0 || i > 0 && n >= h.Ladder[i-1] {
			return fmt.Errorf("invalid ladder %v", h.Ladder)
		}
	}
	if lo, hi := h.Aspects(); h.MinAspect < 0 || h.MaxAspect < 0 || lo > 1 || hi < 1 {
		return fmt.Errorf("invalid aspect bounds %g-%g", lo, hi)
	}
	return nil
}

func isVideoType(t string) bool { return strings.HasPrefix(t, "video/") }

func isImageType(t string) bool { return strings.HasPrefix(t, "image/") }

func isAudioType(t string) bool { return strings.HasPrefix(t, "audio/") }

const (
	outputEntryBytes = 357 // includes the private allocation UUID
)

const (
	trackEntryBytes = 640 + 24 + 74 // fresh allocation UUIDs in both Blob and Track.Index
)

const (
	blobFieldsBytes = 256 // what a staged upload or a frame gains: its blob, size and dimensions
)

const (
	measuredBytes = 48 // an upload's w and h, or dur, once measured
)

const (
	failureEntryBytes = 768 // a Failure: its key, a message of maxFailureBytes, code and details
)

// Unwritten estimates the JSON m gains as the worker processes it. Per
// upload it is the larger of a failure record and what success writes: an
// entry for every output its presets have yet to write (an HLS ladder's
// tracks one by one: a rendition per rung and codec, the sprite, and as many
// audio and subtitle tracks as a source may carry), its measured fields,
// and a staged upload's or frame's blob.
// Then the zips and the public presets' pending names.
// Commits bound the manifest with it; an item whose outputs still overrun
// it is marked Full by the worker.
func (k *RegisteredKind) Unwritten(m *Manifest) int64 {
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
		if isImageType(f.Type) && f.Editor == nil {
			out += 256 // an owned editor image and its source/spec fingerprint
		}
		from := jsonLen(f.Path)
		public := k.PublicFor(f.Path)
		for _, p := range public {
			if f.Unattached {
				continue
			}
			if len(m.Outputs(f.Path, p.Name)) == 0 {
				// One output entry per width: its name, hash and dimensions.
				for _, name := range k.PublicNames(p, f.Path) {
					out += int64(jsonLen(name) + outputEntryBytes)
				}
			}
		}
		for _, p := range k.PrivateFor(f.Path) {
			entries, size := int64(1), int64(outputEntryBytes)
			switch {
			case p.HLS != nil:
				ladder := cmp.Or(len(p.HLS.Ladder), len(media.DefaultLadder))
				entries, size = int64(3*ladder+1+media.MaxAudioTracks+media.MaxSubtitleTracks), trackEntryBytes
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
	return n
}
