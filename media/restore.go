package media

// ObjectVersion identifies immutable bytes in a versioned bucket.
type ObjectVersion struct {
	Key, VersionID string
}
