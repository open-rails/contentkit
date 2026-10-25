package entities

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
)

// DefaultQueue holds the current entity state work.
const DefaultQueue = "contentkit_entities"

type syncArgs struct {
	Tenant   string   `json:"tenant"`
	Entities []string `json:"groups"`
}

func (syncArgs) Kind() string { return "contentkit_entity_sync" }

type syncWorker struct {
	river.WorkerDefaults[syncArgs]
	s *Store
}

func (w *syncWorker) Work(ctx context.Context, job *river.Job[syncArgs]) error {
	if job.Args.Tenant != w.s.tenant {
		return river.JobCancel(fmt.Errorf("entities: job of tenant %q", job.Args.Tenant))
	}
	ids := make([]EntityID, len(job.Args.Entities))
	for i, id := range job.Args.Entities {
		ids[i] = EntityID(id)
	}
	return w.s.Sync(ctx, ids)
}

// riverBinding is the River client a store's composition bound.
type riverBinding struct {
	mu       sync.Mutex
	composed bool
	client   *river.Client[pgx.Tx]
}

func (b *riverBinding) bound() *river.Client[pgx.Tx] {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.client
}

// RiverJobs contributes the entity sync worker and queue. Writes enqueue
// exact lifecycle work transactionally even before this worker is bound.
// Ordinary staff-managed entities contribute no worker or queue.
func (s *Store) RiverJobs() riverhelpers.Contribution {
	b := s.river
	claimed := false
	return riverhelpers.NewContribution("contentkit-entities", func(_ context.Context, cfg *river.Config) error {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.composed {
			return errors.New("entities: River jobs already composed")
		}
		b.composed, claimed = true, true
		if !s.needsSync() {
			return nil
		}
		if _, ok := cfg.Queues[s.queue]; !ok {
			cfg.Queues[s.queue] = river.QueueConfig{MaxWorkers: 1}
		}
		if err := river.AddWorkerSafely(cfg.Workers, &syncWorker{s: s}); err != nil {
			return err
		}
		return nil
	}, func(_ context.Context, bd riverhelpers.Binding) error {
		if bd.Pool != s.pool || bd.Client.Schema() != s.riverSchema {
			return errors.New("entities: the River fleet must use the configured pool and schema")
		}
		b.mu.Lock()
		b.client = bd.Client
		b.mu.Unlock()
		return nil
	}, func() error {
		if claimed {
			b.mu.Lock()
			b.client = nil
			b.mu.Unlock()
		}
		return nil
	})
}
