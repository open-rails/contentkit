package versions

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
)

// Queue is the River queue of version publication and optional media jobs.
const Queue = "contentkit_versions"

// RiverJobs contributes the version release workers and queue to
// the host's River composition, beside media's own.
func (s *Service) RiverJobs() riverhelpers.Contribution {
	return riverhelpers.NewContribution("contentkit-versions", func(_ context.Context, cfg *river.Config) error {
		if s.o.MediaConfigured && s.m.Manifests == nil {
			return errors.New("versions: configured media must be bound before River jobs")
		}
		if err := s.addReleaseWorker(cfg.Workers); err != nil {
			return err
		}
		if _, ok := cfg.Queues[QueueFor(s.o.Tenant)]; !ok {
			cfg.Queues[QueueFor(s.o.Tenant)] = river.QueueConfig{MaxWorkers: 2}
		}
		return nil
	}, func(_ context.Context, b riverhelpers.Binding) error {
		if b.Pool != s.o.Pool {
			return errors.New("versions: River must use the service's pool")
		}
		s.mu.Lock()
		s.client = b.Client
		s.mu.Unlock()
		return nil
	}, func() error {
		s.mu.Lock()
		s.client = nil
		s.mu.Unlock()
		return nil
	})
}

func (s *Service) bound() (*river.Client[pgx.Tx], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		return nil, errors.New("versions: River jobs are not composed into a client")
	}
	return s.client, nil
}
