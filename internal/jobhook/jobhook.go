// Package jobhook lets ContentKit's own white-box tests work the engine's
// jobs on a River client they configure (a queue subset, one worker). Hosts
// cannot import it; they call Engine.Start and Engine.StartJobs.
package jobhook

import (
	"context"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
)

// Fleet starts engine (a *contentkit.Engine) on an unstarted River client
// built from cfg with the engine's jobs and extra. The caller starts and
// stops the client; Engine.Close leaves it running.
var Fleet func(ctx context.Context, engine any, cfg *river.Config, extra ...riverhelpers.Contribution) (*river.Client[pgx.Tx], error)

// SearchExpansion sizes engine's search sync: items per rename-expansion
// batch and batches per run (zero is the default).
var SearchExpansion func(engine any, batch, pages int)

// SearchSync runs one run of engine's search sync, as its job does without
// queueing the next, and reports whether work remains.
var SearchSync func(ctx context.Context, engine any) (more bool, err error)
