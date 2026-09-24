package media

import (
	"math"
	"sync"
	"time"
)

// ViewerLimit is the read API's per-viewer token bucket: every read,
// playlist, download, slot and video-images request takes one token. Zero
// fields take the defaults (2/s sustained, burst 120: a page of reads and
// an HLS session each fit, bulk link harvesting does not).
type ViewerLimit struct {
	PerSecond float64
	Burst     int
	// Disabled turns limiting off, e.g. when the host limits upstream.
	Disabled bool
}

const (
	defaultViewerRate  = 2
	defaultViewerBurst = 120
	limiterShards      = 32
	limiterMaxKeys     = 1 << 16 // per shard; idle full buckets are dropped first
)

// viewerLimiter is an in-process token bucket per viewer key. Replicas each
// keep their own, so the effective limit scales with the replica count.
type viewerLimiter struct {
	rate   float64
	burst  float64
	now    func() time.Time
	shards [limiterShards]limiterShard
}

type limiterShard struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newViewerLimiter(l ViewerLimit, now func() time.Time) *viewerLimiter {
	if l.Disabled {
		return nil
	}
	if l.PerSecond <= 0 {
		l.PerSecond = defaultViewerRate
	}
	if l.Burst <= 0 {
		l.Burst = defaultViewerBurst
	}
	v := &viewerLimiter{rate: l.PerSecond, burst: float64(l.Burst), now: now}
	for i := range v.shards {
		v.shards[i].buckets = map[string]*bucket{}
	}
	return v
}

// allow takes one token for key, or reports how long until one is available.
func (v *viewerLimiter) allow(key string) (bool, time.Duration) {
	if v == nil {
		return true, 0
	}
	s := &v.shards[fnv32(key)%limiterShards]
	now := v.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[key]
	if !ok {
		if len(s.buckets) >= limiterMaxKeys {
			v.prune(s, now)
		}
		b = &bucket{tokens: v.burst, at: now}
		s.buckets[key] = b
	}
	b.tokens = math.Min(v.burst, b.tokens+now.Sub(b.at).Seconds()*v.rate)
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration(math.Ceil((1-b.tokens)/v.rate*1000)) * time.Millisecond
}

// prune drops buckets that have refilled: forgetting them changes nothing.
func (v *viewerLimiter) prune(s *limiterShard, now time.Time) {
	for k, b := range s.buckets {
		if b.tokens+now.Sub(b.at).Seconds()*v.rate >= v.burst {
			delete(s.buckets, k)
		}
	}
	if len(s.buckets) >= limiterMaxKeys { // all active: shed an arbitrary half
		n := 0
		for k := range s.buckets {
			if n++; n > limiterMaxKeys/2 {
				break
			}
			delete(s.buckets, k)
		}
	}
}

func fnv32(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
