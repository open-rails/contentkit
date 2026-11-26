package rankings

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// fillLock bounds one replica's fill of a closed range.
const fillLock = 3 * time.Minute

// store holds encoded rankings and similar lists, shared through Redis or per
// process.
type store interface {
	get(ctx context.Context, key string) ([]byte, bool, error)
	// set stores value for ttl; 0 keeps it without expiry.
	set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	expire(ctx context.Context, key string, ttl time.Duration) error
	// lock takes key for ttl; false when another holds it.
	lock(ctx context.Context, key string, ttl time.Duration) (bool, error)
	unlock(ctx context.Context, key string)
}

type redisStore struct {
	rdb    redis.UniversalClient
	prefix string
}

func (r *redisStore) get(ctx context.Context, key string) ([]byte, bool, error) {
	b, err := r.rdb.Get(ctx, r.prefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	return b, err == nil, err
}

func (r *redisStore) set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return r.rdb.Set(ctx, r.prefix+key, value, ttl).Err()
}

func (r *redisStore) expire(ctx context.Context, key string, ttl time.Duration) error {
	return r.rdb.Expire(ctx, r.prefix+key, ttl).Err()
}

func (r *redisStore) lock(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return r.rdb.SetNX(ctx, r.prefix+"lock:"+key, 1, ttl).Result()
}

func (r *redisStore) unlock(ctx context.Context, key string) { r.rdb.Del(ctx, r.prefix+"lock:"+key) }

type memoryEntry struct {
	value   []byte
	expires time.Time
}

type memoryStore struct {
	mu   sync.Mutex
	now  func() time.Time
	data map[string]memoryEntry
}

func newMemoryStore(now func() time.Time) *memoryStore {
	return &memoryStore{now: now, data: map[string]memoryEntry{}}
}

func (m *memoryStore) get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.data[key]
	if !ok || !e.expires.IsZero() && !m.now().Before(e.expires) {
		delete(m.data, key)
		return nil, false, nil
	}
	return e.value, true, nil
}

func (m *memoryStore) set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for k, e := range m.data {
		if !e.expires.IsZero() && !now.Before(e.expires) {
			delete(m.data, k)
		}
	}
	e := memoryEntry{value: value}
	if ttl > 0 {
		e.expires = now.Add(ttl)
	}
	m.data[key] = e
	return nil
}

func (m *memoryStore) expire(_ context.Context, key string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.data[key]; ok {
		e.expires = m.now().Add(ttl)
		m.data[key] = e
	}
	return nil
}

func (m *memoryStore) lock(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if _, held, _ := m.get(ctx, "lock:"+key); held {
		return false, nil
	}
	return true, m.set(ctx, "lock:"+key, nil, ttl)
}

func (m *memoryStore) unlock(_ context.Context, key string) {
	m.mu.Lock()
	delete(m.data, "lock:"+key)
	m.mu.Unlock()
}

// Ranking is one ranked list, best first: the contents or entities of a
// kind with activity in its window. A list shows them, the ones with a
// positive or zero score first, then everything without activity (newest
// first), then the negative ones.
type Ranking struct {
	// Version names this ranking: a cursor that carries it keeps paging it.
	// "" for a ranking that is not cached.
	Version string
	// From and Until are the UTC days [From, Until) it ranks; Computed is
	// when.
	From, Until, Computed time.Time
	// Ready is false for a featured window not yet precomputed: it ranks
	// nothing.
	Ready bool
	// Stale is a featured ranking whose hourly precompute has since failed.
	Stale bool
	IDs   []string
	Stats []Stat

	positions *positions
}

type positions struct {
	once  sync.Once
	index map[string]int
}

// Stat is one entry's measures over the ranking's window.
type Stat struct {
	// Score is the entry's popularity: Σ q, gated days scaled; per live day
	// for trending; the sum of its galleries' for an entity.
	Score float64
	// Opens are its person-days with a read.
	Opens uint32
	// Polarization, from 0 to 1, is how split an entity's readers are
	// between strong positives and strong negatives.
	Polarization float32
}

// Position is id's place in r; false when r does not rank it.
func (r *Ranking) Position(id string) (int, bool) {
	if r.positions == nil {
		return 0, false
	}
	p := r.positions
	p.once.Do(func() {
		p.index = make(map[string]int, len(r.IDs))
		for i, id := range r.IDs {
			p.index[id] = i
		}
	})
	i, ok := p.index[id]
	return i, ok
}

const rankingMagic = "CKR1"

func encodeRanking(r *Ranking) []byte {
	var b bytes.Buffer
	b.WriteString(rankingMagic)
	ready := byte(0)
	if r.Ready {
		ready = 1
	}
	b.WriteByte(ready)
	_ = binary.Write(&b, binary.LittleEndian, r.From.Unix())
	_ = binary.Write(&b, binary.LittleEndian, r.Until.Unix())
	_ = binary.Write(&b, binary.LittleEndian, r.Computed.UnixMilli())
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(r.IDs)))
	for i, id := range r.IDs {
		u := uuid.MustParse(id)
		b.Write(u[:])
		s := r.Stats[i]
		_ = binary.Write(&b, binary.LittleEndian, s.Score)
		_ = binary.Write(&b, binary.LittleEndian, s.Opens)
		_ = binary.Write(&b, binary.LittleEndian, s.Polarization)
	}
	return b.Bytes()
}

const entryBytes = 16 + 8 + 4 + 4

func decodeRanking(data []byte, version string) (*Ranking, error) {
	bad := fmt.Errorf("rankings: cached ranking %s is malformed", version)
	head := len(rankingMagic) + 1 + 8 + 8 + 8 + 4
	if len(data) < head || string(data[:len(rankingMagic)]) != rankingMagic {
		return nil, bad
	}
	r := &Ranking{Version: version, Ready: data[len(rankingMagic)] == 1, positions: &positions{}}
	p := len(rankingMagic) + 1
	r.From = time.Unix(int64(binary.LittleEndian.Uint64(data[p:])), 0).UTC()
	r.Until = time.Unix(int64(binary.LittleEndian.Uint64(data[p+8:])), 0).UTC()
	r.Computed = time.UnixMilli(int64(binary.LittleEndian.Uint64(data[p+16:]))).UTC()
	n := int(binary.LittleEndian.Uint32(data[p+24:]))
	p = head
	if len(data) != head+n*entryBytes {
		return nil, bad
	}
	r.IDs, r.Stats = make([]string, n), make([]Stat, n)
	for i := range n {
		u, _ := uuid.FromBytes(data[p : p+16])
		r.IDs[i] = u.String()
		r.Stats[i] = Stat{Score: math.Float64frombits(binary.LittleEndian.Uint64(data[p+16:])), Opens: binary.LittleEndian.Uint32(data[p+24:]),
			Polarization: math.Float32frombits(binary.LittleEndian.Uint32(data[p+28:]))}
		p += entryBytes
	}
	return r, nil
}

// localCache keeps decoded rankings in process: a page reads its ranking's
// slice without fetching it again.
type localCache struct {
	mu      sync.Mutex
	entries map[string]*Ranking
	order   []string
}

const localEntries = 64

func (c *localCache) get(key string) *Ranking {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[key]
}

func (c *localCache) put(key string, r *Ranking) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]*Ranking{}
	}
	if _, ok := c.entries[key]; !ok {
		c.order = append(c.order, key)
	}
	c.entries[key] = r
	for len(c.order) > localEntries {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
}

// flights shares one in-process fill of a key among its callers.
type flights struct {
	mu    sync.Mutex
	calls map[string]*flight
}

type flight struct {
	done chan struct{}
	err  error
}

// do runs fn once per key at a time; a caller arriving meanwhile waits for
// that run's error.
func (f *flights) do(ctx context.Context, key string, fn func() error) error {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]*flight{}
	}
	if c, ok := f.calls[key]; ok {
		f.mu.Unlock()
		select {
		case <-c.done:
			return c.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c := &flight{done: make(chan struct{})}
	f.calls[key] = c
	f.mu.Unlock()
	c.err = fn()
	f.mu.Lock()
	delete(f.calls, key)
	f.mu.Unlock()
	close(c.done)
	return c.err
}

func encodeReadThrough(r ReadThrough, ok bool) []byte {
	if !ok {
		return []byte{0}
	}
	b := make([]byte, 13)
	b[0] = 1
	binary.LittleEndian.PutUint64(b[1:], math.Float64bits(r.Rate))
	binary.LittleEndian.PutUint32(b[9:], uint32(r.Readers))
	return b
}

func decodeReadThrough(b []byte) (ReadThrough, bool) {
	if len(b) != 13 || b[0] != 1 {
		return ReadThrough{}, false
	}
	return ReadThrough{Rate: math.Float64frombits(binary.LittleEndian.Uint64(b[1:])), Readers: int(binary.LittleEndian.Uint32(b[9:]))}, true
}
