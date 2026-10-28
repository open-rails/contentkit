package media

const (
	HLSContentType = "application/vnd.apple.mpegurl"
)

const (
	VTTContentType = "text/vtt; charset=utf-8"
)

// StartRung is the short side of the variant listed first in each codec:
// native HLS players (Safari, iOS) start there before measuring.
const StartRung = 1080
