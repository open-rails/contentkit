// Package signalstore is ContentKit's ClickHouse activity log: the batched
// intake, the outcome query over it, the daily rollups and erasure.
package signalstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"

	signal "github.com/open-rails/contentkit/signal"
)

// Store reads and writes one ClickHouse database. Every method takes the
// tenant its rows belong to.
type Store struct {
	conn    signal.Conn
	db      string
	cluster string
}

// NewStore returns a Store over an existing connection. The schema comes from
// migrations.ClickHouse; verify it with CheckSchema.
func NewStore(conn signal.Conn, database string, options ...signal.StoreOptions) (*Store, error) {
	if conn == nil {
		return nil, fmt.Errorf("signal: conn is required")
	}
	db := strings.TrimSpace(database)
	if !identRe.MatchString(db) {
		return nil, fmt.Errorf("signal: invalid database name %q", db)
	}
	if len(options) > 1 {
		return nil, fmt.Errorf("signal: at most one store options value is allowed")
	}
	var cluster string
	if len(options) == 1 {
		cluster = strings.TrimSpace(options[0].Cluster)
		if cluster != "" && !identRe.MatchString(cluster) {
			return nil, fmt.Errorf("signal: invalid cluster name %q", cluster)
		}
	}
	return &Store{conn: conn, db: db, cluster: cluster}, nil
}

// Conn is the store's connection.
func (st *Store) Conn() signal.Conn { return st.conn }

// Database is the store's database.
func (st *Store) Database() string { return st.db }

// Actions an activity row records.
const (
	ActionOpen     = "open"
	ActionProgress = "progress"
	ActionReaction = "reaction"
	ActionFavorite = "favorite"
	ActionHide     = "hide"
	ActionSearch   = "search"
)

// Units of a progress row's points.
const (
	UnitPage   = "page"
	UnitSecond = "second"
)

// Access values of a row besides an account's claimed tiers.
const (
	AccessAnonymous = "anonymous"
	AccessAccount   = "account"
)

// Row is one activity row as the API stamps it.
type Row struct {
	Subject     uuid.UUID
	At          time.Time
	Action      string
	ContentKind string
	ContentID   uuid.UUID
	Domain      string
	Language    string
	Access      string
	Offset      int16
	RequestID   uuid.UUID
	Surface     string
	Position    uint16
	ActiveMS    uint32
	Furthest    uint32
	Current     uint32
	Length      uint32
	Unit        string
	Value       int8
	Reason      string
	Query       string
	Results     []uuid.UUID
}

const activityColumns = "tenant, subject, ts, action, content_kind, content_id, domain, language, access, utc_offset, request_id, surface, position, " +
	"active_ms, furthest, current, length, unit, value, reason, query, results"

const activityPlaceholders = "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

// insertRows bounds one INSERT.
const insertRows = 1000

// tsLayout is how a row's time is sent: text ClickHouse parses in UTC.
const tsLayout = "2006-01-02 15:04:05.000"

// Insert writes rows as INSERT … VALUES with async_insert=1 and
// wait_for_async_insert=1: ClickHouse batches parts across processes and
// the call returns once the rows are stored. It reports how many rows it
// wrote before an error.
func (st *Store) Insert(ctx context.Context, tenant string, rows []Row) (int, error) {
	if strings.TrimSpace(tenant) == "" {
		return 0, fmt.Errorf("signal: tenant is required")
	}
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"async_insert": 1, "wait_for_async_insert": 1}))
	for start := 0; start < len(rows); start += insertRows {
		chunk := rows[start:min(start+insertRows, len(rows))]
		values := make([]string, len(chunk))
		args := make([]any, 0, len(chunk)*22)
		for i, r := range chunk {
			values[i] = activityPlaceholders
			results := make([]string, len(r.Results))
			for j, id := range r.Results {
				results[j] = id.String()
			}
			args = append(args, tenant, r.Subject.String(), r.At.UTC().Format(tsLayout), r.Action, r.ContentKind, r.ContentID.String(),
				r.Domain, r.Language, r.Access, r.Offset, r.RequestID.String(), r.Surface, r.Position,
				r.ActiveMS, r.Furthest, r.Current, r.Length, r.Unit, r.Value, r.Reason, r.Query, results)
		}
		q := "INSERT INTO " + st.db + ".activity (" + activityColumns + ") VALUES " + strings.Join(values, ", ")
		if err := st.conn.Exec(ctx, q, args...); err != nil {
			return start, fmt.Errorf("signal: insert activity: %w", err)
		}
	}
	return len(rows), nil
}

// notErased drops rows of erased subjects; it binds the tenant.
func (st *Store) notErased(column string) string {
	return column + " NOT IN (SELECT subject FROM " + st.db + ".erasures WHERE tenant = ?)"
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}
