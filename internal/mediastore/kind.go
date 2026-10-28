package mediastore

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path"
	"slices"
	"strconv"
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
	if p.Previews != "" {
		pv := k.public(p.Previews)
		if pv == nil || pv.First == 0 || !slices.Contains(k.Uploads[k.uploadIndex(pv.From)].Types, "image/webp") {
			return fmt.Errorf("Previews %q is no preview preset (First) of a WebP upload", p.Previews)
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
		if z := k.private(p.Download); z == nil || z.Zip == "" || !z.HostOnly {
			return fmt.Errorf("Download %q is no HostOnly Zip preset", p.Download)
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

// PublicNames are p's public names in m for the upload at path, one per
// width: none for an upload outside a preview's First. m may be nil for a
// preset without First.
func (k *RegisteredKind) PublicNames(m *Manifest, p *media.Public, path string) []string {
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
func (k *RegisteredKind) firsts(m *Manifest, p *media.Public) []string {
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
func rendered(f media.File, p *media.Public) bool {
	return f.Blob != "" && f.Fail() == nil && !slices.Contains(f.Pending, p.Name)
}

// previewNames lists m's published preview generations in order.
func (k *RegisteredKind) previewNames(m *Manifest) []string {
	var out []string
	for i := range k.Public {
		p := &k.Public[i]
		for _, path := range k.firsts(m, p) {
			if f, _ := m.Get(path); rendered(f, p) {
				if pub, ok := k.Current(m, f, p); ok {
					if k.Versions {
						for _, blob := range pub.Blobs {
							if area, name, valid := layout.SplitBlobRef(blob); valid && area == layout.AreaPublic {
								out = append(out, name)
							}
						}
						continue
					}
					out = append(out, pub.NamesOnDisk()...)
				}
			}
		}
	}
	return out
}

// Publication returns the latest generation still owned by the source and
// position: a reservation (cleanup must protect it while PUTs run), else
// the current generation.
func (k *RegisteredKind) Publication(m *Manifest, f media.File, p *media.Public) (media.Publication, bool) {
	pub, ok := f.Publication(p.Name)
	return pub, ok && k.owned(m, f, p, pub)
}

// Current returns the ready generation still owned by the source and
// position: what readers are shown.
func (k *RegisteredKind) Current(m *Manifest, f media.File, p *media.Public) (media.Publication, bool) {
	pub, ok := f.Current(p.Name)
	return pub, ok && k.owned(m, f, p, pub)
}

func (k *RegisteredKind) owned(m *Manifest, f media.File, p *media.Public, pub media.Publication) bool {
	return !f.Unattached && f.Fail() == nil && pub.Source == f.Key() &&
		slices.Equal(pub.Names, k.PublicNames(m, p, f.Path))
}

// PublicKept protects every owned generation: the current files, legacy
// ones included, and an active reservation, plus a Stable preset's logical
// names while one is current. Any other generation's name is permanently
// retired; no later worker may reuse it.
func (k *RegisteredKind) PublicKept(m *Manifest) []string {
	if k.Versions {
		var out []string
		for _, blob := range m.Blobs() {
			if area, name, ok := layout.SplitBlobRef(blob); ok && area == layout.AreaPublic {
				out = append(out, name)
			}
		}
		return out
	}
	var out []string
	for _, f := range m.Files {
		if !f.IsUpload() || f.Unattached {
			continue
		}
		for _, p := range k.PublicFor(f.Path) {
			for _, pub := range f.Public {
				if pub.Preset == p.Name && k.owned(m, f, p, pub) {
					out = append(out, pub.NamesOnDisk()...)
					if p.Stable && pub.Ready() && pub.Generation != "" {
						out = append(out, pub.Names...)
					}
				}
			}
		}
	}
	return out
}

// Mirror copies a Stable preset's current file to its logical name.
type Mirror struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// mirrors are the copies an edit from cur to next makes: each Stable
// preset's current generation that next newly makes current, to its logical
// names. Cleanup (PublicKept) removes them once none is current.
func (k *RegisteredKind) mirrors(item Item, cur, next *Manifest) []Mirror {
	if k.Versions {
		return nil
	}
	var out []Mirror
	for _, f := range next.Files {
		if !f.IsUpload() {
			continue
		}
		for _, p := range k.PublicFor(f.Path) {
			pub, ok := k.Current(next, f, p)
			if !p.Stable || !ok || pub.Generation == "" {
				continue
			}
			if was, ok := cur.Get(f.Path); ok {
				if prev, ok := k.Current(cur, was, p); ok && prev.Generation == pub.Generation {
					continue
				}
			}
			for i, name := range pub.NamesOnDisk() {
				from, _ := item.Public(name)
				to, _ := item.Public(pub.Names[i])
				out = append(out, Mirror{From: from, To: to})
			}
		}
	}
	return out
}

// syncPreviews keeps each preview preset pending on exactly the uploads
// whose position's file is not the one before did (a reorder, an insert, a
// removal, a new source or edit); uploads past First carry none.
func (k *RegisteredKind) syncPreviews(before, m *Manifest) {
	for i := range k.Public {
		p := &k.Public[i]
		if p.First == 0 {
			continue
		}
		was := map[int]string{}
		if before != nil {
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
			case n < 0 || f.Fail() != nil:
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

// UploadRules are the kind's upload paths with the rules a client checks.
func (k *RegisteredKind) UploadRules() []media.UploadRule {
	out := make([]media.UploadRule, 0, len(k.Uploads))
	for _, u := range k.Uploads {
		r := media.UploadRule{Path: u.Path, Types: slices.Clone(u.Types), MaxBytes: u.MaxBytes, Max: u.Max, Named: u.Named, Body: u.Body, Frames: u.Frames}
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
		if i := k.uploadIndex(d); p == nil && (i < 0 || strings.Contains(d, nameVar)) ||
			p != nil && p.MP4 == nil && (p.Zip == "" || k.Publish == nil || k.Publish.Download != d) {
			return fmt.Errorf("download %q is no literal upload, MP4 preset or Publish's Zip", d)
		}
	}
	return nil
}

// Area is the area of an upload's allocation (preset "") or of a derived
// file's: a version's originals/ or internal/, else private/.
func (k *RegisteredKind) Area(preset string) string {
	if k.Versions {
		if preset == "" {
			return layout.AreaOriginals
		}
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
	for _, set := range []bool{p.Image != nil, p.HLS != nil, p.MP4 != nil, p.Zip != "", p.Subtitles != nil} {
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
	dir := p.HLS != nil
	sourceFormat := p.Image != nil && p.Image.Format == media.ImageFormatSource
	if !validTemplate(p.To, dir) || slices.ContainsFunc(placeholders(p.To), func(s string) bool { return s != "name" && !(sourceFormat && s == "ext") }) {
		return fmt.Errorf("invalid To %q (a path with {name}; source-format images may also use {ext})", p.To)
	}
	if sourceFormat && !strings.HasSuffix(p.To, "{ext}") {
		return fmt.Errorf("source-format image To %q must retain the source extension with {ext}", p.To)
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
	switch {
	case p.Image != nil:
		if err := validateImage(*p.Image); err != nil {
			return err
		}
		return validateShapes(*p.Image, p.Shapes)
	case p.HLS != nil:
		return validateHLS(p.HLS)
	case p.MP4 != nil:
		if p.MP4.Rung < 2 || p.MP4.Rung > 4320 || p.MP4.Rung%2 != 0 {
			return fmt.Errorf("invalid MP4 rung %d", p.MP4.Rung)
		}

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
	if p.First < 0 || p.First > maxFirst || p.First > 0 && (k.patterns[i].literal != "" || slices.Contains(vars, "name") || p.Default != "") {
		return fmt.Errorf("First (at most %d) needs a {name} upload, {n} in To for {name}, and no Default", maxFirst)
	}
	if slices.ContainsFunc(vars, func(s string) bool { return s != "name" && s != "w" && s != "n" }) ||
		slices.Contains(vars, "w") != (len(p.Widths) > 0) || slices.Contains(vars, "n") != (p.First > 0) ||
		!layout.ValidSegment(fill(p.To, map[string]string{"name": "n", "w": "1", "n": "1"})) {
		return fmt.Errorf("invalid To %q ([A-Za-z0-9._-] with {w} exactly when Widths are set and {n} exactly with First)", p.To)
	}
	if p.Stable && (slices.Contains(vars, "name") || p.First > 0) {
		return errors.New("Stable needs one image per item: no {name} in To, no First")
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
		if s.Image.Format != base.Format || s.Image.Animation != base.Animation {
			return fmt.Errorf("shape %d: keep the preset's format and animation", i)
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
			names := k.PublicNames(m, p, f.Path)
			if len(names) == 0 || f.Unattached {
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
	for i := range k.Private {
		if z := &k.Private[i]; z.Zip != "" && written[[2]string{z.Zip, z.Name}] == 0 {
			n += outputEntryBytes + maxNameBytes + 16 + int64(jsonLen(z.To)+jsonLen(z.Zip)+len(z.Name))
		}
	}
	return n
}

// DownloadFile is the file k.Download serves in m: its literal upload, or
// its MP4 preset's output of the attached source. A Zip's is its own route's.
func (k *RegisteredKind) DownloadFile(m *Manifest) (media.File, media.DownloadState) {
	from, p := k.Download, k.private(k.Download)
	if p != nil {
		if p.MP4 == nil {
			return media.File{}, media.DownloadNone
		}
		from = p.From
	}
	g := k.uploadIndex(from)
	for _, f := range m.Files {
		if i, _, _, _, ok := k.upload(f.Path); !f.IsUpload() || f.Unattached || !ok || i != g || g < 0 {
			continue
		}
		switch {
		case f.Fail() != nil, f.Gone:
			return f, media.DownloadNone
		case f.Blob == "":
			return f, media.DownloadPending
		case p == nil:
			return f, media.DownloadReady
		case slices.Contains(f.Pending, p.Name):
			return f, media.DownloadPending
		}
		for _, o := range m.Outputs(f.Path, p.Name) {
			if o.Blob != "" && !o.Gone {
				return o, media.DownloadReady
			}
		}
		return f, media.DownloadNone
	}
	return media.File{}, media.DownloadNone
}
