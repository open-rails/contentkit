package worker

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/open-rails/contentkit/media/video"
)

// Metrics records work attempts and video encode passes. Register it once per
// worker process; queue backlog metrics belong on an always-on host process.
type Metrics struct {
	river.MiddlewareDefaults
	attempts *prometheus.HistogramVec
	duration *prometheus.HistogramVec
	encode   *prometheus.HistogramVec
	cpu      *prometheus.CounterVec
	output   *prometheus.CounterVec
	coreRate *prometheus.HistogramVec
}

// NewMetrics registers one worker process's metrics with registerer.
func NewMetrics(registerer prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		attempts: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "media_video_attempt", Help: "Attempt number of executed media jobs.",
			Buckets: []float64{1, 2, 3, 4, 5},
		}, []string{"queue", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "media_video_job_duration_seconds", Help: "Elapsed duration of a media job work attempt.",
			Buckets: []float64{1, 5, 15, 30, 60, 300, 900, 1800, 3600, 7200, 14400, 28800, 57600},
		}, []string{"queue", "outcome"}),
		encode: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "media_video_encode_seconds", Help: "Elapsed duration of an ffmpeg video ladder pass.",
			Buckets: []float64{1, 5, 15, 30, 60, 300, 900, 1800, 3600, 7200, 14400, 28800, 57600},
		}, []string{"class", "outcome"}),
		cpu: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "media_video_encode_cpu_seconds_total", Help: "User and system CPU seconds spent in ffmpeg video ladder passes.",
		}, []string{"class", "outcome"}),
		output: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "media_video_encoded_output_seconds_total", Help: "Successful encoded video rendition seconds.",
		}, []string{"class"}),
		coreRate: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "media_video_core_seconds_per_output_second", Help: "FFmpeg CPU seconds per encoded video rendition second.",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 25, 50, 100, 250, 500},
		}, []string{"class"}),
	}
	for _, collector := range []prometheus.Collector{m.attempts, m.duration, m.encode, m.cpu, m.output, m.coreRate} {
		if err := registerer.Register(collector); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Work wraps the full River execution, including WorkBegin and argument
// decoding, which can fail before WorkEnd is called.
func (m *Metrics) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) (err error) {
	started := time.Now()
	defer func() {
		if p := recover(); p != nil {
			m.recordWork(job, "error", time.Since(started))
			panic(p)
		}
		m.recordWork(job, workOutcome(ctx, err), time.Since(started))
	}()
	return doInner(ctx)
}

func workOutcome(ctx context.Context, err error) string {
	var snooze *river.JobSnoozeError
	var cancel *river.JobCancelError
	switch {
	case errors.As(err, &snooze):
		return "snoozed"
	case errors.As(err, &cancel), errors.Is(err, river.ErrJobCancelledRemotely), err != nil && errors.Is(context.Cause(ctx), river.ErrJobCancelledRemotely):
		return "cancelled"
	case err != nil:
		return "error"
	default:
		return "success"
	}
}

func (m *Metrics) recordWork(job *rivertype.JobRow, outcome string, duration time.Duration) {
	m.attempts.WithLabelValues(job.Queue, outcome).Observe(float64(job.Attempt))
	m.duration.WithLabelValues(job.Queue, outcome).Observe(duration.Seconds())
}

func (m *Metrics) ObserveEncode(o video.EncodeObservation) {
	outcome := "error"
	if o.Succeeded {
		outcome = "success"
		m.output.WithLabelValues(o.SourceClass).Add(o.OutputSeconds)
		if o.OutputSeconds > 0 {
			m.coreRate.WithLabelValues(o.SourceClass).Observe(o.CPU.Seconds() / o.OutputSeconds)
		}
	}
	m.encode.WithLabelValues(o.SourceClass, outcome).Observe(o.Duration.Seconds())
	m.cpu.WithLabelValues(o.SourceClass, outcome).Add(o.CPU.Seconds())
}
