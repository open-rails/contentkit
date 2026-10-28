package discovery

// KnownCardsOptions selects a bounded set of already-known IDs, not a listing.
type KnownCardsOptions struct {
	Reader
	IDs          []string
	Kind         string
	CardEntities []string
	Summary      bool
}
