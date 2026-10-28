package access

// Item is one independently accessible version. Content identifies its
// catalog grouping; Version is required and is the entitlement boundary.
type Item struct {
	Content string
	Version string
	// Rule is the version's effective rule (Effective).
	Rule Rule
}
