package video

import "context"

// SetBeforePromote runs fn between the blob uploads and the manifest edit.
func SetBeforePromote(fn func()) func() {
	testBeforePromote = fn
	return func() { testBeforePromote = nil }
}

// SetMultipart lowers the multipart threshold and part size.
func SetMultipart(above, part int64) func() {
	a, p := multipartAbove, partSize
	multipartAbove, partSize = above, part
	return func() { multipartAbove, partSize = a, p }
}

// SetNVENCCQOffset overrides NVENC's CQ offset over the rung CRF.
func SetNVENCCQOffset(o int) func() {
	old := nvencCQOffset
	nvencCQOffset = o
	return func() { nvencCQOffset = old }
}

// SetStageOneMax lowers the first stage's largest rung.
func SetStageOneMax(n int) func() {
	old := stageOneMax
	stageOneMax = n
	return func() { stageOneMax = old }
}

// EncodeStage runs the stale files' next stage and reports whether one remains.
func EncodeStage(ctx context.Context, e *Encoder, job Job, report Report) (bool, error) {
	return e.encode(ctx, job, report, true)
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
