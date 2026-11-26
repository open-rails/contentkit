package mediastore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/contentref"
	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/helpers/eventlog"
	eventpg "github.com/open-rails/helpers/eventlog/postgres"
)

// PGCache is the catalog's cache of item manifests: one content_media_items
// row per item (its revision, ETag, processing status and public images),
// written forward-only by Rev after every manifest write. Everything in it
// is rebuilt from the bucket (Rebuild); nothing about files is kept
// elsewhere in Postgres.
type PGCache struct {
	pool   *pgxpool.Pool
	schema string
	table  string
	reg    *Registry
	routes map[string]*PGCache
	o      CacheOptions
}

// CacheOptions are what a recorded write sets off.
type CacheOptions struct {
	// Lock runs first in every recording transaction, before the item's row
	// is locked: the catalog's rows are always locked before the cache's, as
	// catalog writers lock them. Optional.
	Lock func(ctx context.Context, tx pgx.Tx, item Item, m *Manifest) error
	// Recorded runs in the transaction that advanced an item's row, with the
	// manifest just recorded: the catalog's own facts (a version's pages,
	// cover and release) follow it there.
	Recorded func(ctx context.Context, tx pgx.Tx, item Item, m *Manifest) error
	// Processes is told about items with work to do: a rebuild's, as commits
	// and processing jobs enqueue their own. Optional.
	Processes ProcessQueue
	// GC schedules collection of items with retired files; optional.
	GC GCQueue
	// Settle is the manifests' settle period (ManifestOptions.Settle).
	Settle time.Duration
	Now    func() time.Time
}

// CatalogHook is the catalog's part of recording a manifest
// (CacheOptions.Lock and Recorded).
type CatalogHook struct {
	Lock     func(ctx context.Context, tx pgx.Tx, item Item, m *Manifest) error
	Recorded func(ctx context.Context, tx pgx.Tx, item Item, m *Manifest) error
}

// NewPGCache caches items of reg in schema.
func NewPGCache(pool *pgxpool.Pool, schema string, reg *Registry, o CacheOptions) (*PGCache, error) {
	if pool == nil || schema == "" || reg == nil {
		return nil, errors.New("media: the item cache needs a pool, a schema and a registry")
	}
	if o.Settle <= 0 {
		o.Settle = DefaultSettle
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &PGCache{pool: pool, schema: schema, table: pgx.Identifier{schema, "content_media_items"}.Sanitize(), reg: reg, o: o}, nil
}

// RouteCache sends the account partition's items to account.
func RouteCache(app, account *PGCache, partition string) (*PGCache, error) {
	if app == nil || account == nil || app.pool != account.pool || partition == "" {
		return nil, errors.New("media: cache route needs one database and account partition")
	}
	copy := *app
	copy.routes = map[string]*PGCache{partition: account}
	return &copy, nil
}

func (c *PGCache) forTenant(tenant string) *PGCache {
	if q := c.routes[tenant]; q != nil {
		return q
	}
	return c
}

// Pool is the cache's database.
func (c *PGCache) Pool() *pgxpool.Pool { return c.pool }

// cacheRow is an item's cached facts.
type cacheRow struct {
	Rev     int64
	ETag    string
	State   string
	Deleted bool
	Images  []cachedImage
}

// Record caches m, ref's manifest as stored with etag, unless the row
// already holds this revision or a newer one. A row that moves forward runs
// Recorded in the same transaction, appends a processing event when the
// status changed, and then schedules processing and collection.
func (c *PGCache) Record(ctx context.Context, item Item, m *Manifest, etag string) error {
	if item.Ref().ContentID == layout.DefaultID {
		return nil // a kind's defaults: no catalog item
	}
	c = c.forTenant(item.Ref().TenantID)
	k := item.Kind()
	status := statusOf(k, m, media.EncodeStatus{})
	images, err := json.Marshal(k.publicImages(m))
	if err != nil {
		return err
	}
	ref := item.Ref()
	// Most writes find the row current (an unchanged edit, a retry): no
	// transaction and no lock then.
	var rev int64
	err = c.pool.QueryRow(ctx, `SELECT rev FROM `+c.table+` WHERE tenant_id=$1 AND content_kind=$2 AND content_id=$3 AND content_version_id=$4`,
		ref.TenantID, ref.ContentKind, ref.ContentID, ref.Version()).Scan(&rev)
	if err == nil && rev >= m.Rev {
		return nil
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	fact := media.ProcessingEvent{Kind: ref.ContentKind, ContentID: ref.ContentID, VersionID: ref.Version(), State: status.State,
		Uploads: status.Uploads, Archives: status.Archives, Presets: status.Presets, FailureCount: status.FailureCount}
	statusJSON, err := json.Marshal(fact)
	if err != nil {
		return err
	}
	var advanced bool
	err = pgx.BeginFunc(ctx, c.pool, func(tx pgx.Tx) error {
		if c.o.Lock != nil {
			if err := c.o.Lock(ctx, tx, item, m); err != nil {
				return err
			}
		}
		var prev []byte
		err := tx.QueryRow(ctx, `SELECT status FROM `+c.table+` WHERE tenant_id=$1 AND content_kind=$2 AND content_id=$3 AND content_version_id=$4 FOR UPDATE`,
			ref.TenantID, ref.ContentKind, ref.ContentID, ref.Version()).Scan(&prev)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO `+c.table+` AS t
(tenant_id,content_kind,content_id,content_version_id,rev,etag,state,status,images,deleted,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now())
ON CONFLICT(tenant_id,content_kind,content_id,content_version_id) DO UPDATE
SET rev=EXCLUDED.rev,etag=EXCLUDED.etag,state=EXCLUDED.state,status=EXCLUDED.status,images=EXCLUDED.images,deleted=EXCLUDED.deleted,updated_at=now()
WHERE t.rev < EXCLUDED.rev`, ref.TenantID, ref.ContentKind, ref.ContentID, ref.Version(), m.Rev, etag, status.State, statusJSON, images, m.Deleted != nil)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		advanced = true
		if c.o.Recorded != nil {
			if err := c.o.Recorded(ctx, tx, item, m); err != nil {
				return err
			}
		}
		if string(prev) == string(statusJSON) || prev == nil && len(m.Files) == 0 {
			return nil
		}
		return c.appendProcessing(ctx, tx, ref, fact, statusJSON)
	})
	if err != nil || !advanced {
		return err
	}
	return c.followUp(ctx, item, m)
}

// followUp schedules what m still needs: processing and collection.
func (c *PGCache) followUp(ctx context.Context, item Item, m *Manifest) error {
	if c.o.Processes != nil && m.Deleted == nil && item.Kind().NeedsProcessing(m) {
		if err := c.o.Processes.Enqueue(ctx, ProcessJob{Ref: item.Ref(), Place: len(m.StagedNames()) > 0}); err != nil {
			return err
		}
	}
	if c.o.GC != nil {
		if at := nextCollection(m, c.o.Settle, c.o.Now().UTC()); !at.IsZero() {
			return c.o.GC.ScheduleGC(ctx, item.Ref(), at)
		}
	}
	return nil
}

func (c *PGCache) appendProcessing(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef, fact media.ProcessingEvent, payload []byte) error {
	eventType := media.EventProcessingProgressed
	if fact.State != media.StateProcessing {
		eventType = media.EventProcessingCompleted
	}
	scope, err := processingEventScope(ctx, tx, c.schema, ref.TenantID)
	if err != nil {
		return err
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT nextval($1::regclass)::text`, pgx.Identifier{c.schema, "content_event_ids"}.Sanitize()).Scan(&id); err != nil {
		return err
	}
	log, err := eventpg.New(c.pool, eventpg.Config{Schema: c.schema, Source: "contentkit", Scope: scope})
	if err != nil {
		return err
	}
	return log.AppendTx(ctx, tx, eventlog.Event{ID: id, Source: "contentkit", Scope: scope, Type: eventType, Version: 1, OccurredAt: time.Now().UTC(),
		Subject: eventlog.Subject{Type: ref.ContentKind, ID: ref.ContentID}, Data: payload})
}

func processingEventScope(ctx context.Context, tx pgx.Tx, schema, namespace string) (string, error) {
	scope, scoped := layout.ScopeFromPartition(namespace)
	if !scoped || scope.AppID == "" {
		return namespace, nil
	}
	var catalog string
	err := tx.QueryRow(ctx, `SELECT catalog_key FROM `+pgx.Identifier{schema, "content_app_bindings"}.Sanitize()+` WHERE tenant_id=$1::uuid AND app_id=$2::uuid`, scope.TenantID, scope.AppID).Scan(&catalog)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("media: processing event has no catalog binding for %s", namespace)
	}
	return catalog, err
}

// read loads the cached rows of refs, keyed by ref.
func (c *PGCache) read(ctx context.Context, refs []contentref.ContentRef) (map[contentref.ContentKey]cacheRow, error) {
	out := map[contentref.ContentKey]cacheRow{}
	groups := map[*PGCache][]contentref.ContentRef{}
	for _, ref := range refs {
		q := c.forTenant(ref.TenantID)
		groups[q] = append(groups[q], ref)
	}
	for q, refs := range groups {
		cols := make([][]string, 4)
		for _, ref := range refs {
			for i, v := range []string{ref.TenantID, ref.ContentKind, ref.ContentID, ref.Version()} {
				cols[i] = append(cols[i], v)
			}
		}
		rows, err := q.pool.Query(ctx, `SELECT t.tenant_id,t.content_kind,t.content_id,t.content_version_id,t.rev,t.etag,t.state,t.deleted,t.images
FROM unnest($1::text[],$2::text[],$3::text[],$4::text[]) AS k(tenant_id,content_kind,content_id,content_version_id)
JOIN `+q.table+` t USING (tenant_id,content_kind,content_id,content_version_id)`, cols[0], cols[1], cols[2], cols[3])
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var tenant, kind, id, version string
			var row cacheRow
			var images []byte
			if err := rows.Scan(&tenant, &kind, &id, &version, &row.Rev, &row.ETag, &row.State, &row.Deleted, &images); err != nil {
				rows.Close()
				return nil, err
			}
			if row.Images, err = decodeImages(images); err != nil {
				rows.Close()
				return nil, err
			}
			out[contentref.New(tenant, kind, id).WithVersion(version).Key()] = row
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ETag is the manifest ETag recorded for item, "" when it has no row
// (ManifestOptions.Current).
func (c *PGCache) ETag(ctx context.Context, item Item) (string, error) {
	ref := item.Ref()
	if ref.ContentID == layout.DefaultID {
		return "", nil
	}
	q := c.forTenant(ref.TenantID)
	var etag string
	err := q.pool.QueryRow(ctx, `SELECT etag FROM `+q.table+` WHERE tenant_id=$1 AND content_kind=$2 AND content_id=$3 AND content_version_id=$4`,
		ref.TenantID, ref.ContentKind, ref.ContentID, ref.Version()).Scan(&etag)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return etag, err
}

// Forget drops ref's cached row: its item is gone from the catalog.
func (c *PGCache) ForgetTx(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef) error {
	q := c.forTenant(ref.TenantID)
	_, err := tx.Exec(ctx, `DELETE FROM `+q.table+` WHERE tenant_id=$1 AND content_kind=$2 AND content_id=$3 AND content_version_id=$4`,
		ref.TenantID, ref.ContentKind, ref.ContentID, ref.Version())
	return err
}

// nextCollection is when m next needs collecting: its earliest due or
// settling entry, or a day after its last write for the orphan pass.
func nextCollection(m *Manifest, settle time.Duration, now time.Time) time.Time {
	var next time.Time
	earlier := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	for _, r := range m.Retired {
		if r.Deleting == nil {
			earlier(r.Due)
		} else {
			earlier(r.Deleting.Add(settle))
		}
	}
	if orphanPass := m.WrittenAt.Add(OrphanAge + time.Hour); orphanPass.After(now) {
		earlier(orphanPass)
	}
	return next
}
