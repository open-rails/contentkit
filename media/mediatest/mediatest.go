// Package mediatest opens real media storage for tests outside media/: the
// S3 test bucket (CONTENTKIT_TEST_S3_*, unset skips) and its Postgres
// journal and locker (CONTENTKIT_TEST_URL). See media/internal/s3test.
package mediatest

import (
	"testing"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

// Env is one test's bucket prefix, store and journal database.
type Env = s3test.Env

// Open probes the test bucket and returns a store under a fresh prefix,
// removed on cleanup.
func Open(t testing.TB) *Env { return s3test.Open(t) }

// Locker is a PGLocker on CONTENTKIT_TEST_URL, as a host wires one.
func Locker(t testing.TB, store media.Store) media.Locker { return s3test.Locker(t, store) }
