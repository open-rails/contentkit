package versionstore

import (
	"github.com/open-rails/contentkit/internal/jobqueue"
	versions "github.com/open-rails/contentkit/media/versions"
)

// QueueFor is the edition-worker queue for one immutable catalog key. The
// media partition (layout.Scope.Partition) is not necessarily this catalog key.
func QueueFor(catalog string) string { return jobqueue.For(versions.Queue, catalog) }
