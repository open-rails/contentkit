package openrails

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/open-rails/helpers/notify"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
)

// Jobs routes one shared worker to any number of explicitly configured sources.
// Each source has a distinct queue and durable consumer, and contributes to the
// same host fleet. A worker never acknowledges an unconfigured source's job.
type Jobs struct {
	mu         sync.RWMutex
	sources    map[string]billingSource
	registered map[*river.Workers]bool
}
type billingSource struct {
	source      *Source
	destination notify.Publisher
	mapper      RecipientMapper
}

// Add contributes one source's queue, registration and periodic import. Call
// before starting the host fleet. Repeating a source is a configuration error.
func (j *Jobs) Add(s *Source, destination notify.Publisher, mapper RecipientMapper, namespace string) (riverhelpers.Contribution, error) {
	if s == nil || destination == nil || mapper == nil || namespace == "" {
		return riverhelpers.Contribution{}, fmt.Errorf("openrails notifications: source, destination, mapper and namespace required")
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(namespace+"\x00"+s.name+"\x00"+s.merchant+"\x00"+s.consumer)))
	queue := "contentkit_notifications_" + key[:16]
	j.mu.Lock()
	if j.sources == nil {
		j.sources = map[string]billingSource{}
		j.registered = map[*river.Workers]bool{}
	}
	if _, exists := j.sources[key]; exists {
		j.mu.Unlock()
		return riverhelpers.Contribution{}, fmt.Errorf("openrails notifications: source already attached")
	}
	j.sources[key] = billingSource{source: s, destination: destination, mapper: mapper}
	j.mu.Unlock()
	return riverhelpers.NewContribution(queue, func(ctx context.Context, cfg *river.Config) error {
		if err := s.Register(ctx); err != nil {
			return err
		}
		j.mu.Lock()
		if !j.registered[cfg.Workers] {
			if err := river.AddWorkerSafely(cfg.Workers, &billingWorker{jobs: j}); err != nil {
				j.mu.Unlock()
				return err
			}
			j.registered[cfg.Workers] = true
		}
		j.mu.Unlock()
		if cfg.Queues == nil {
			cfg.Queues = map[string]river.QueueConfig{}
		}
		cfg.Queues[queue] = river.QueueConfig{MaxWorkers: 1}
		cfg.PeriodicJobs = append(cfg.PeriodicJobs, river.NewPeriodicJob(river.PeriodicInterval(30*time.Second), func() (river.JobArgs, *river.InsertOpts) {
			return billingArgs{Key: key}, &river.InsertOpts{Queue: queue, MaxAttempts: 10, UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: 30 * time.Second}}
		}, &river.PeriodicJobOpts{ID: queue, RunOnStart: true}))
		return nil
	}, nil, nil), nil
}

type billingArgs struct {
	Key string `json:"key"`
}

func (billingArgs) Kind() string { return "contentkit_openrails_notifications" }

type billingWorker struct {
	river.WorkerDefaults[billingArgs]
	jobs *Jobs
}

func (w *billingWorker) Work(ctx context.Context, job *river.Job[billingArgs]) error {
	w.jobs.mu.RLock()
	source, ok := w.jobs.sources[job.Args.Key]
	w.jobs.mu.RUnlock()
	if !ok {
		return fmt.Errorf("openrails notifications: worker source mismatch")
	}
	_, err := source.source.Consume(ctx, source.destination, source.mapper, ConsumeOptions{Filter: BillingLifecycle})
	return err
}
