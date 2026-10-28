package access

// Mode is a listing's access mode.
type Mode string

const (
	// Hide lists only what the viewer opens (a purchased library).
	Hide Mode = "hide"
)

const (
	// Lock lists everything; Decide marks what stays locked.
	Lock Mode = "lock"
)
