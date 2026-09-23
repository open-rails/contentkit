package video

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
