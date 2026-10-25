package versions

import "github.com/open-rails/contentkit/internal/jobqueue"

// QueueFor is the edition-worker queue for one immutable catalog key. The
// media tenant/app path partition is not necessarily this catalog key.
func QueueFor(catalog string) string { return jobqueue.For(Queue, catalog) }
