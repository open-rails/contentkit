package catalog

// AuthError is a permission check the auth could not answer: 401 when the
// credential ended, else 503. It never grants.
type AuthError struct{ Err error }

func (e *AuthError) Error() string { return "catalog: permission check failed: " + e.Err.Error() }

func (e *AuthError) Unwrap() error { return e.Err }
