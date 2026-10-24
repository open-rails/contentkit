package video

import "github.com/open-rails/contentkit/media"

// SetBeforePublish runs fn between the output uploads and the manifest edit.
func SetBeforePublish(fn func()) func() {
	testBeforePublish = fn
	return func() { testBeforePublish = nil }
}

// SetNVENCCQOffset overrides NVENC's CQ offset over the rung CRF of codec c.
func SetNVENCCQOffset(c media.Codec, o int) func() {
	old := nvencCQOffset[c]
	nvencCQOffset[c] = o
	return func() { nvencCQOffset[c] = old }
}

// SetCapScale multiplies every rung's bitrate cap.
func SetCapScale(f float64) func() {
	old := rates
	rates = map[string][5]rungRate{}
	for k, rs := range old {
		for i := range rs {
			rs[i].maxrate = int(float64(rs[i].maxrate) * f)
		}
		rates[k] = rs
	}
	return func() { rates = old }
}

// FailNVENC makes e's codec c encoder an NVENC stand-in that fails on the
// first frame (h264_vaapi without a device), so a pass takes the CPU fallback.
func FailNVENC(e *Encoder, c media.Codec) func() {
	old := nvencEncoders[c]
	nvencEncoders[c] = "h264_vaapi"
	e.encoders[c] = nvencEncoders[c]
	return func() { nvencEncoders[c] = old }
}

// SetMaxSubtitleBytes lowers the subtitle size cap.
func SetMaxSubtitleBytes(n int64) func() {
	old := maxSubtitleBytes
	maxSubtitleBytes = n
	return func() { maxSubtitleBytes = old }
}
