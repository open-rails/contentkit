package contentkit

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/workqueue"
)

// bindCheckTimeout bounds New's look at each bound namespace's owner.
const bindCheckTimeout = 5 * time.Second

// binding is one bound namespace (MediaConfig.Bind) and its owner's bucket.
type binding struct {
	ns string
	NamespaceOwner
	pool  *pgxpool.Pool
	store media.Store
}

// bindings validates Config.Media.Bind: each namespace is a shared one of
// the registry, bound to another engine's schemas and bucket on Deps.Store's
// backend. A binding New cannot read fails New, as any invalid Config does:
// dropping it would split the namespace's journal (contentkit#115).
func (e *Engine) bindings(mc media.Config, deps Deps) ([]binding, error) {
	bind := e.cfg.Media.Bind
	if len(bind) == 0 {
		return nil, nil
	}
	shared := map[string]bool{}
	for _, k := range mc.Kinds {
		if k.Namespace != "" && k.Namespace != mc.Namespace {
			shared[k.Namespace] = true
		}
	}
	opener, ok := deps.Store.(media.BucketOpener)
	if !ok {
		return nil, fmt.Errorf("contentkit: Config.Media.Bind needs a Deps.Store that opens buckets (media.BucketOpener), not %T", deps.Store)
	}
	worker := strings.TrimSpace(e.cfg.Media.WorkerSchema)
	if worker == "" {
		worker = e.schema + "_media_worker"
	}
	var out []binding
	for ns, o := range bind {
		b := binding{ns: ns, NamespaceOwner: NamespaceOwner{Schema: strings.TrimSpace(o.Schema),
			WorkerSchema: strings.TrimSpace(o.WorkerSchema), Bucket: strings.TrimSpace(o.Bucket)}, pool: deps.Postgres}
		if b.WorkerSchema == "" {
			b.WorkerSchema = b.Schema + "_media_worker"
		}
		bad := func(why string) error { return fmt.Errorf("contentkit: Config.Media.Bind[%q]: %s", ns, why) }
		switch {
		case !shared[ns]:
			return nil, bad("no media kind lives in this shared namespace")
		case b.Bucket == "":
			return nil, bad("bucket is required")
		case !schemaRE.MatchString(b.Schema) || len(b.Schema) > 63 || workqueue.ValidSchema(b.WorkerSchema) != nil:
			return nil, bad("schema and worker_schema must be lower-case identifiers")
		case b.Schema == e.schema || b.WorkerSchema == worker:
			return nil, bad("the owner is another engine, with its own schema and worker schema")
		}
		var err error
		if b.store, err = opener.OpenBucket(b.Bucket); err != nil {
			return nil, bad(err.Error())
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ns < out[j].ns })
	return out, nil
}

// bindMedia routes each bound namespace's journal, processing, encode
// progress and objects to its owner: the store, queue and progress m's
// uploads, reads and jobs use.
func (e *Engine) bindMedia(m *engineMedia, journal *media.PGJournal, own media.Store, progress media.ProgressSource) (media.Store, media.ProcessQueue, media.ProgressSource, error) {
	if len(e.bound) == 0 {
		return own, m.queue, progress, nil
	}
	stores, queues, sources := map[string]media.Store{}, map[string]media.ProcessQueue{}, map[string]media.ProgressSource{}
	for _, b := range e.bound {
		queue, err := workqueue.New(e.pool, m.reg, b.WorkerSchema)
		if err != nil {
			return nil, nil, nil, err
		}
		owner, err := media.NewPGJournal(e.pool, b.Schema, queue)
		if err != nil {
			return nil, nil, nil, err
		}
		if err := journal.Bind(b.ns, owner); err != nil {
			return nil, nil, nil, err
		}
		if sources[b.ns], err = workqueue.NewProgressSource(e.pool, b.WorkerSchema); err != nil {
			return nil, nil, nil, err
		}
		stores[b.ns], queues[b.ns] = b.store, queue
	}
	store, err := media.RouteStore(own, stores)
	if err != nil {
		return nil, nil, nil, err
	}
	queue, err := media.RouteQueue(m.queue, queues)
	if err != nil {
		return nil, nil, nil, err
	}
	routed, err := media.RouteProgress(progress, sources)
	return store, queue, routed, err
}

// check reports whether the owner has migrated its journal and worker queue
// and its bucket answers. Until then the namespace's reads and writes fail
// and are retried by their callers; nothing falls back to this engine's own.
func (b binding) check(ctx context.Context) error {
	for _, t := range []string{pgx.Identifier{b.Schema, "content_media_commits"}.Sanitize(), pgx.Identifier{b.WorkerSchema, "river_job"}.Sanitize()} {
		var ok bool
		if err := b.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", t).Scan(&ok); err != nil {
			return fmt.Errorf("contentkit: media namespace %q: %w", b.ns, err)
		}
		if !ok {
			return fmt.Errorf("contentkit: media namespace %q: its owner has no %s yet", b.ns, t)
		}
	}
	if err := b.store.Check(ctx, "_contentkit/"); err != nil {
		return fmt.Errorf("contentkit: media namespace %q: bucket %q: %w", b.ns, b.Bucket, err)
	}
	return nil
}

// checkBindings warns of bound namespaces whose owner is not there yet.
func (e *Engine) checkBindings(ctx context.Context) {
	for _, b := range e.bound {
		c, cancel := context.WithTimeout(ctx, bindCheckTimeout)
		if err := b.check(c); err != nil {
			e.log.WarnContext(ctx, "contentkit: a bound media namespace is unavailable until its owner answers", "namespace", b.ns, "err", err)
		}
		cancel()
	}
}
