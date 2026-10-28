package contenturl

// VersionKey names a version by its content and version code.
type VersionKey struct {
	ContentID string
	Code      VersionCode
}

// AliasKey names another system's identifier: the system (an import
// batch's source), the identifier's space and the identifier.
type AliasKey struct {
	Source string `json:"source"`
	Kind   string `json:"kind"`
	Key    string `json:"key"`
}

// AliasMatch is what an alias resolves to.
type AliasMatch struct {
	Link
	Locator string
}
