package media

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

// ImageTypes are the image types the image presets decode (media/image); an
// upload feeding one accepts no other. SVG, BMP and JPEG XL are refused:
// their libvips loaders are blocked as untrusted.
var ImageTypes = []string{"image/jpeg", "image/png", "image/webp", "image/gif", "image/avif", "image/heic", "image/heif", "image/tiff"}

const (
	MetaLang = "lang" // subtitles and audio: BCP 47
)

const (
	MetaLabel = "label" // a track's name
)

const (
	MetaForced = "forced" // bool: a forced-narrative subtitle track
)

const (
	MetaFor = "for" // a subtitle's video upload path; default every video of the item
)

const (
	MetaCharset = "charset" // a subtitle's IANA charset, overriding detection
)

const (
	MetaAutoCover = "auto_cover" // automatic cover ownership; explicit uploads clear it
)

const (
	MaxAudioTracks = 8
)

const (
	MaxSubtitleTracks = 16
)

// DownloadState is how far an item's Download file is.
type DownloadState int

const (
	DownloadNone    DownloadState = iota // nothing to serve: no source, a failed or dropped one
	DownloadPending                      // still being placed or produced
	DownloadReady
)
