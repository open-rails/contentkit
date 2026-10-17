package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/discovery"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/codes"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/offers"
)

// ErrInvalid is a batch refused as a whole.
var ErrInvalid = errors.New("importer: invalid batch")

// chunk is the most records one transaction writes.
const chunk = 1000

// Options configures a Service over the engine's stores.
type Options struct {
	Pool    *pgxpool.Pool
	Schema  string
	Tenant  string
	Catalog *catalog.Store
	Groups  *groups.Store
	Content *content.Runtime
	// Offers is nil without billing: offers are skipped.
	Offers *offers.Service
	// Signals is nil, or answers discovery.ErrNoSignals, without a signal
	// plane: signals are skipped.
	Signals *discovery.Service
	// Rebuild queues the jobs that rebuild what an import changed: search
	// documents, group counts and listing counts.
	Rebuild func(ctx context.Context) error
	Logger  *slog.Logger
}

// Service is one tenant's bulk import.
type Service struct {
	o   Options
	ids string // content_external_ids
	qs  string
}

// New checks the options.
func New(o Options) (*Service, error) {
	if o.Pool == nil || o.Catalog == nil || o.Groups == nil || o.Content == nil || strings.TrimSpace(o.Tenant) == "" {
		return nil, errors.New("importer: Pool, Tenant, Catalog, Groups and Content are required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Service{o: o, ids: pgx.Identifier{o.Schema, "content_external_ids"}.Sanitize(), qs: pgx.Identifier{o.Schema}.Sanitize()}, nil
}

var sourceRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// Import writes a batch, section by section. It fails as a whole only for a
// batch it refuses outright or when ctx ends; records that fail are in the
// result.
func (s *Service) Import(ctx context.Context, b Batch) (Result, error) {
	res := Result{Written: map[string]int{}, Failed: []RecordFailure{}, IDs: map[string]map[string]string{}}
	if !sourceRE.MatchString(b.Source) {
		return res, fmt.Errorf("%w: source %q must match %s", ErrInvalid, b.Source, sourceRE)
	}
	if n := b.records(); n > MaxRecords {
		return res, fmt.Errorf("%w: %d records; at most %d per batch", ErrInvalid, n, MaxRecords)
	}
	if b.PreferenceFloor != 0 {
		err := pgx.BeginFunc(ctx, s.o.Pool, func(tx pgx.Tx) error { return s.o.Content.RaisePreferenceFloor(ctx, tx, b.PreferenceFloor) })
		if errors.Is(err, content.ErrInvalid) {
			return res, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if err != nil {
			return res, err
		}
	}
	run := &batchRun{s: s, b: b, res: &res}
	for _, sec := range run.sections() {
		if sec.n == 0 {
			continue
		}
		if sec.skip != "" {
			res.skip(sec.name, sec.n)
			s.o.Logger.InfoContext(ctx, "contentkit: import skipped "+sec.name, "reason", sec.skip, "records", sec.n)
			continue
		}
		for start := 0; start < sec.n; start += chunk {
			if err := s.runChunk(ctx, run, sec, start, min(start+chunk, sec.n)); err != nil {
				return res, err
			}
		}
	}
	if len(res.Written) > 0 && s.o.Rebuild != nil {
		if err := s.o.Rebuild(ctx); err != nil {
			s.o.Logger.WarnContext(ctx, "contentkit: import could not queue its rebuild; the periodic jobs will", "err", err)
		}
	}
	return res, nil
}

func (b Batch) records() int {
	n := len(b.Groups) + len(b.Memberships) + len(b.Offers) + len(b.Aliases) + len(b.Comments) + len(b.Reactions) + len(b.Favorites) +
		len(b.Votes) + len(b.Answers) + len(b.LegacyReading) + len(b.Signals) + len(b.Deletes)
	for _, c := range b.Contents {
		n += 1 + len(c.Versions)
	}
	for _, p := range b.Polls {
		n += 1 + len(p.Options)
	}
	return n
}

func (r *Result) skip(section string, n int) {
	if r.Skipped == nil {
		r.Skipped = map[string]int{}
	}
	r.Skipped[section] += n
}

// mapping is one record's identity under the batch's source.
type mapping struct {
	Entity     string `json:"entity"`
	ExternalID string `json:"external_id"`
	ID         string `json:"id"`
	fresh      bool   // not stored yet
}

// pending is one prepared record: its store input and identities.
type pending struct {
	index    int
	external string
	in       any
	ids      []mapping
}

// section is one record group of a batch.
type section struct {
	name string
	n    int
	skip string // why the engine skips it
	// prepare resolves record i into its store input.
	prepare func(r *resolver, i int) (in any, external string, err error)
	// write writes prepared inputs in tx.
	write func(ctx context.Context, tx pgx.Tx, in []any) error
}

// runChunk writes records [from, to) of a section in one transaction: all
// together, else (one failed) each in its own savepoint, so only failures
// are left out.
func (s *Service) runChunk(ctx context.Context, run *batchRun, sec section, from, to int) error {
	tx, err := s.o.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	r := &resolver{s: s, source: run.b.Source, known: map[extKey]string{}, collect: true}
	for i := from; i < to; i++ {
		_, _, _ = sec.prepare(r, i)
	}
	if err := r.load(ctx, tx); err != nil {
		return err
	}
	r.collect = false
	var ready []pending
	for i := from; i < to; i++ {
		r.minted = nil
		in, external, err := sec.prepare(r, i)
		if err != nil {
			run.fail(sec.name, i, external, err)
			continue
		}
		ready = append(ready, pending{index: i, external: external, in: in, ids: r.minted})
	}
	write := func(list []pending) error {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		defer sp.Rollback(ctx)
		var ms []mapping
		ins := make([]any, len(list))
		for i, p := range list {
			ms, ins[i] = append(ms, p.ids...), p.in
		}
		if err := s.putIDs(ctx, sp, run.b.Source, ms); err != nil {
			return err
		}
		if err := sec.write(ctx, sp, ins); err != nil {
			return err
		}
		return sp.Commit(ctx)
	}
	written := ready
	if err := write(ready); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		written = nil
		for _, p := range ready {
			if err := write([]pending{p}); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				run.fail(sec.name, p.index, p.external, err)
				continue
			}
			written = append(written, p)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if len(written) > 0 {
		run.res.Written[sec.name] += len(written)
	}
	for _, p := range written {
		for _, m := range p.ids {
			if run.res.IDs[m.Entity] == nil {
				run.res.IDs[m.Entity] = map[string]string{}
			}
			run.res.IDs[m.Entity][m.ExternalID] = m.ID
		}
	}
	return nil
}

// putIDs records new identities, refusing one a concurrent import gave
// another id.
func (s *Service) putIDs(ctx context.Context, tx pgx.Tx, source string, ms []mapping) error {
	var fresh []mapping
	for _, m := range ms {
		if m.fresh {
			fresh = append(fresh, m)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	data, err := json.Marshal(fresh)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.ids+` (tenant_id, source, entity, external_id, id)
 SELECT $1, $2, r.entity, r.external_id, r.id FROM jsonb_to_recordset($3::jsonb) AS r(entity text, external_id text, id text)
 ON CONFLICT DO NOTHING`, s.o.Tenant, source, string(data)); err != nil {
		return err
	}
	var clash string
	if err := tx.QueryRow(ctx, `SELECT coalesce(min(r.entity || ' ' || r.external_id), '') FROM jsonb_to_recordset($3::jsonb) AS r(entity text, external_id text, id text)
 JOIN `+s.ids+` x ON x.tenant_id = $1 AND x.source = $2 AND x.entity = r.entity AND x.external_id = r.external_id WHERE x.id <> r.id`,
		s.o.Tenant, source, string(data)).Scan(&clash); err != nil {
		return err
	}
	if clash != "" {
		return fmt.Errorf("%w: %s was imported concurrently with another id; retry", errConflict, clash)
	}
	return nil
}

var (
	errNotFound = errors.New("not found")
	errConflict = errors.New("conflict")
	errInvalid  = errors.New("invalid")
)

// batchRun is one Import's state.
type batchRun struct {
	s   *Service
	b   Batch
	res *Result
}

func (run *batchRun) fail(section string, index int, external string, err error) {
	code, msg := classify(err)
	if code == CodeInternal {
		run.s.o.Logger.Error("contentkit: import record failed", "section", section, "index", index, "err", err)
	} else {
		run.s.o.Logger.Info("contentkit: import record refused", "section", section, "index", index, "external_id", external, "code", code, "err", err)
	}
	run.res.Failed = append(run.res.Failed, RecordFailure{Section: section, Index: index, ExternalID: external, Code: code, Error: msg})
}

// classify maps a store's error to a failure code and the rule the record
// broke, without internals (fail logs them).
func classify(err error) (string, string) {
	var pg *pgconn.PgError
	switch {
	case errors.Is(err, content.ErrSubjectErased):
		return CodeErased, "The person was erased."
	case errors.Is(err, errNotFound), errors.Is(err, catalog.ErrNotFound), errors.Is(err, groups.ErrNotFound), errors.Is(err, offers.ErrNotFound),
		errors.Is(err, content.ErrNotFound), errors.Is(err, codes.ErrNotFound), errors.Is(err, discovery.ErrNotFound):
		return CodeNotFound, httpapi.Rule(err, httpapi.CodeNotFound)
	case errors.Is(err, errConflict), errors.Is(err, catalog.ErrConflict), errors.Is(err, groups.ErrConflict), errors.Is(err, offers.ErrConflict),
		errors.Is(err, content.ErrConflict), errors.Is(err, codes.ErrConflict):
		return CodeConflict, httpapi.Rule(err, httpapi.CodeConflict)
	case errors.Is(err, errInvalid), errors.Is(err, catalog.ErrInvalid), errors.Is(err, groups.ErrInvalid), errors.Is(err, offers.ErrInvalid),
		errors.Is(err, content.ErrInvalid), errors.Is(err, codes.ErrInvalid), errors.Is(err, discovery.ErrInvalid), errors.Is(err, contentref.ErrInvalidID):
		return CodeInvalid, httpapi.Rule(err, httpapi.CodeInvalidRequest)
	case errors.As(err, &pg):
		switch {
		case pg.Code == "23505":
			return CodeConflict, httpapi.Message(httpapi.CodeConflict)
		case pg.Code == "23503":
			return CodeNotFound, httpapi.Message(httpapi.CodeNotFound)
		case strings.HasPrefix(pg.Code, "23"), strings.HasPrefix(pg.Code, "22"):
			return CodeInvalid, httpapi.Message(httpapi.CodeInvalidRequest)
		}
	}
	return CodeInternal, httpapi.Message(httpapi.CodeInternal)
}

// extKey is an external id of an entity.
type extKey struct{ entity, external string }

// resolver maps refs and identities to ids. A first pass collects what a
// chunk names (load reads it); the second resolves.
type resolver struct {
	s       *Service
	source  string
	collect bool
	keys    []extKey
	direct  []string // ids named directly, whose facts load reads
	known   map[extKey]string
	minted  []mapping // the record's identities

	contentKind    map[string]string
	versionContent map[string]string
	groupKind      map[string]string
	comments       map[string]bool
}

// own is a record's id: the one its external id maps to, else the one given
// (or a new one, timed createdAt), recorded with the record.
func (r *resolver) own(entity, external, given string, createdAt *time.Time) (string, error) {
	external = strings.TrimSpace(external)
	if external == "" || len(external) > 256 {
		return "", fmt.Errorf("%w: a %s needs an external_id of 1-256 bytes", errInvalid, entity)
	}
	k := extKey{entity, external}
	if r.collect {
		r.keys = append(r.keys, k)
		return given, nil
	}
	if given != "" {
		if err := contentref.ValidateID(given); err != nil {
			return "", fmt.Errorf("%w: %s %s id: %v", errInvalid, entity, external, err)
		}
	}
	id, stored := r.known[k]
	switch {
	case stored && given != "" && given != id:
		return "", fmt.Errorf("%w: %s %s is imported as %s, not %s", errConflict, entity, external, id, given)
	case !stored && given != "":
		id = given
	case !stored && createdAt != nil:
		id = contentref.IDAt(*createdAt)
	case !stored:
		id = contentref.NewID()
	}
	if !stored {
		r.known[k] = id // later records of the chunk name it
	}
	r.minted = append(r.minted, mapping{Entity: entity, ExternalID: external, ID: id, fresh: !stored})
	return id, nil
}

// ref is the id a reference names.
func (r *resolver) ref(entity string, ref Ref) (string, error) {
	switch {
	case ref.ID != "" && ref.ExternalID != "":
		return "", fmt.Errorf("%w: a %s reference names an id or an external_id, not both", errInvalid, entity)
	case ref.ID != "":
		if r.collect {
			r.direct = append(r.direct, ref.ID)
			return ref.ID, nil
		}
		if err := contentref.ValidateID(ref.ID); err != nil {
			return "", fmt.Errorf("%w: %s id: %v", errInvalid, entity, err)
		}
		return ref.ID, nil
	case ref.ExternalID != "":
		k := extKey{entity, ref.ExternalID}
		if r.collect {
			r.keys = append(r.keys, k)
			return "", nil
		}
		id, ok := r.known[k]
		if !ok {
			return "", fmt.Errorf("%w: %s %s is not imported", errNotFound, entity, ref.ExternalID)
		}
		return id, nil
	}
	return "", fmt.Errorf("%w: a %s reference names an id or an external_id", errInvalid, entity)
}

// load reads the chunk's external ids, then the kinds and contents of the
// records it names.
func (r *resolver) load(ctx context.Context, tx pgx.Tx) error {
	if len(r.keys) > 0 {
		entities, externals := make([]string, len(r.keys)), make([]string, len(r.keys))
		for i, k := range r.keys {
			entities[i], externals[i] = k.entity, k.external
		}
		rows, err := tx.Query(ctx, `SELECT x.entity, x.external_id, x.id FROM unnest($3::text[], $4::text[]) AS k(entity, external_id)
 JOIN `+r.s.ids+` x ON x.tenant_id = $1 AND x.source = $2 AND x.entity = k.entity AND x.external_id = k.external_id`,
			r.s.o.Tenant, r.source, entities, externals)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k extKey
			var id string
			if err := rows.Scan(&k.entity, &k.external, &id); err != nil {
				rows.Close()
				return err
			}
			r.known[k] = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	ids := append([]string(nil), r.direct...)
	for _, id := range r.known {
		ids = append(ids, id)
	}
	r.contentKind, r.versionContent, r.groupKind, r.comments = map[string]string{}, map[string]string{}, map[string]string{}, map[string]bool{}
	if len(ids) == 0 {
		return nil
	}
	// Comment ids are uuids; only well-formed ids reach that column.
	uuids := make([]string, 0, len(ids))
	for _, id := range ids {
		if contentref.ValidateID(id) == nil {
			uuids = append(uuids, id)
		}
	}
	rows, err := tx.Query(ctx, `SELECT 'c', id, kind FROM `+r.s.qs+`.content_items WHERE tenant_id = $1 AND id = ANY($2)
 UNION ALL SELECT 'v', id, content_id FROM `+r.s.qs+`.content_versions WHERE tenant_id = $1 AND id = ANY($2)
 UNION ALL SELECT 'g', id, kind FROM `+r.s.qs+`.content_groups WHERE tenant_id = $1 AND id = ANY($2)
 UNION ALL SELECT 'm', id::text, '' FROM `+r.s.qs+`.content_comments WHERE tenant_id = $1 AND id = ANY($3::uuid[])`, r.s.o.Tenant, ids, uuids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var t, id, v string
		if err := rows.Scan(&t, &id, &v); err != nil {
			return err
		}
		switch t {
		case "c":
			r.contentKind[id] = v
		case "v":
			r.versionContent[id] = v
		case "g":
			r.groupKind[id] = v
		case "m":
			r.comments[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// A version's content is a fact too.
	var contents []string
	for _, c := range r.versionContent {
		if _, ok := r.contentKind[c]; !ok {
			contents = append(contents, c)
		}
	}
	if len(contents) == 0 {
		return nil
	}
	more, err := tx.Query(ctx, `SELECT id, kind FROM `+r.s.qs+`.content_items WHERE tenant_id = $1 AND id = ANY($2)`, r.s.o.Tenant, contents)
	if err != nil {
		return err
	}
	defer more.Close()
	for more.Next() {
		var id, kind string
		if err := more.Scan(&id, &kind); err != nil {
			return err
		}
		r.contentKind[id] = kind
	}
	return more.Err()
}

// resolved is what a TargetRef names.
type resolved struct {
	ref     contentref.ContentRef // kind and id, version for a version target
	group   string                // a group target's id
	content string                // the content of a content or version target
	version string
	comment string
}

// Target kinds a section allows.
const (
	allowContent = 1 << iota
	allowVersion
	allowGroup
	allowComment
)

// target resolves t to what it names, within what the section allows.
func (r *resolver) target(t TargetRef, allow int) (resolved, error) {
	var out resolved
	n := 0
	for _, p := range []*Ref{t.Group, t.Comment} {
		if p != nil {
			n++
		}
	}
	if t.Content != nil || t.Version != nil {
		n++
	}
	if n != 1 {
		return out, fmt.Errorf("%w: a target names a content (or version), a group or a comment", errInvalid)
	}
	tenant := r.s.o.Tenant
	switch {
	case t.Group != nil:
		if allow&allowGroup == 0 {
			return out, fmt.Errorf("%w: a group is no target here", errInvalid)
		}
		id, err := r.ref(EntityGroup, *t.Group)
		if err != nil || r.collect {
			return out, err
		}
		kind, ok := r.groupKind[id]
		if !ok {
			return out, fmt.Errorf("%w: group %s", errNotFound, id)
		}
		out.group, out.ref = id, contentref.New(tenant, kind, id)
	case t.Comment != nil:
		if allow&allowComment == 0 {
			return out, fmt.Errorf("%w: a comment is no target here", errInvalid)
		}
		id, err := r.ref(EntityComment, *t.Comment)
		if err != nil || r.collect {
			return out, err
		}
		if !r.comments[id] {
			return out, fmt.Errorf("%w: comment %s", errNotFound, id)
		}
		out.comment, out.ref = id, contentref.New(tenant, content.KindComment, id)
	default:
		var cid, vid string
		var err error
		if t.Content != nil {
			if cid, err = r.ref(EntityContent, *t.Content); err != nil {
				return out, err
			}
		}
		if t.Version != nil {
			if allow&allowVersion == 0 {
				return out, fmt.Errorf("%w: a version is no target here", errInvalid)
			}
			if vid, err = r.ref(EntityVersion, *t.Version); err != nil {
				return out, err
			}
		} else if allow&allowContent == 0 {
			return out, fmt.Errorf("%w: a content item is no target here", errInvalid)
		}
		if r.collect {
			return out, nil
		}
		if vid != "" {
			owner, ok := r.versionContent[vid]
			switch {
			case !ok:
				return out, fmt.Errorf("%w: version %s", errNotFound, vid)
			case cid != "" && cid != owner:
				return out, fmt.Errorf("%w: version %s is not of content %s", errInvalid, vid, cid)
			}
			cid = owner
		}
		kind, ok := r.contentKind[cid]
		if !ok {
			return out, fmt.Errorf("%w: content %s", errNotFound, cid)
		}
		out.content, out.version, out.ref = cid, vid, contentref.NewVersion(tenant, kind, cid, vid)
	}
	return out, nil
}
