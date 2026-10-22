package publishing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/auth"
	"github.com/riverqueue/river"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/groups"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/versions"
)

type Options struct {
	Pool      *pgxpool.Pool
	Schema    string
	Tenant    string
	Catalog   *catalog.Store
	Groups    *groups.Store
	Registry  *media.Registry
	Uploads   *media.Uploads
	Manifests *media.Manifests
	Versions  *versions.Service
	Auth      auth.Authenticator
	Authority Authority
	Logger    *slog.Logger
	Clock     func() time.Time
}

type Store struct {
	o                      Options
	table, items, versions string
	mu                     sync.Mutex
	jobs                   *river.Client[pgx.Tx]
}

func New(o Options) (*Store, error) {
	if o.Pool == nil || o.Schema == "" || o.Tenant == "" || o.Catalog == nil || o.Groups == nil || o.Registry == nil || o.Uploads == nil || o.Manifests == nil || o.Versions == nil {
		return nil, errors.New("publishing: pool, catalog, groups and media are required")
	}
	if o.Catalog.Tenant() != o.Tenant || o.Groups.Tenant() != o.Tenant {
		return nil, errors.New("publishing: catalog scope mismatch")
	}
	if err := gate.Check(o.Auth, access.ContentWrite, access.EntitiesWrite); err != nil {
		return nil, err
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	q := func(t string) string { return pgx.Identifier{o.Schema, t}.Sanitize() }
	return &Store{o: o, table: q("content_publishing_operations"), items: q("content_items"), versions: q("content_versions")}, nil
}

func failure(code, field, message string) *OperationError {
	return &OperationError{Code: code, Field: field, Message: message}
}

func owner(actor access.Actor) (string, error) {
	if actor.Anonymous || actor.ID == "" || actor.Verified == nil {
		return "", failure("unauthorized", "", "authentication required")
	}
	i := actor.Verified.Identity()
	key := []string{i.Issuer, string(i.SubjectKind), i.Subject}
	if i.SubjectKind == auth.SubjectApplication {
		key = append(key, string(i.Credential.Kind), i.Credential.ID)
	}
	b, _ := json.Marshal(key)
	return string(b), nil
}

func (s *Store) allowed(ctx context.Context, actor access.Actor, perm access.Permission) error {
	ok, err := gate.Can(ctx, s.o.Auth, actor, string(perm))
	if err != nil {
		return err
	}
	if !ok {
		return failure("forbidden", "", "permission denied")
	}
	return nil
}

func validID(id string) bool {
	v, e := uuid.Parse(id)
	return e == nil && v != uuid.Nil && v.String() == id
}

func (s *Store) normalize(in Request) (Request, []byte, string, error) {
	if in.Publish == "" {
		in.Publish = Draft
	}
	if in.Publish != Draft && in.Publish != WhenReady {
		return in, nil, "", failure("invalid_request", "publish", "publish must be draft or when_ready")
	}
	if in.PublishAt != nil {
		if in.Publish != WhenReady {
			return in, nil, "", failure("invalid_request", "publish_at", "publish_at requires when_ready publication")
		}
		at := in.PublishAt.UTC()
		in.PublishAt = &at
	}
	in.Archive.Name = strings.TrimSpace(in.Archive.Name)
	if in.Archive.Name == "" || len(in.Archive.Name) > 255 || path.Base(in.Archive.Name) != in.Archive.Name || !strings.EqualFold(path.Ext(in.Archive.Name), ".zip") {
		return in, nil, "", failure("invalid_request", "archive.name", "archive.name must be a ZIP filename")
	}
	if in.Archive.Size <= 0 {
		return in, nil, "", failure("invalid_request", "archive.size", "archive.size must be positive")
	}
	in.Archive.SHA256 = strings.ToLower(strings.TrimSpace(in.Archive.SHA256))
	if b, e := hex.DecodeString(in.Archive.SHA256); e != nil || len(b) != sha256.Size {
		return in, nil, "", failure("invalid_request", "archive.sha256", "archive.sha256 must contain 64 hexadecimal characters")
	}
	k, e := s.o.Registry.Kind(in.Content.Kind)
	if e != nil || !k.Versions || k.Publish == nil || k.Publish.Import == "" || k.Publish.Pages == "" {
		return in, nil, "", failure("invalid_request", "content.kind", "content kind must support a versioned page archive")
	}
	archiveAllowed := false
	for _, upload := range k.Uploads {
		if upload.Path == k.Publish.Import && slices.Contains(upload.Types, "application/zip") {
			archiveAllowed = true
			if upload.MaxBytes > 0 && in.Archive.Size > upload.MaxBytes {
				return in, nil, "", failure("too_large", "archive.size", "archive exceeds this content kind's upload limit")
			}
		}
	}
	if !archiveAllowed {
		return in, nil, "", failure("type_not_allowed", "archive", "this content kind does not accept ZIP archives")
	}
	if len(in.Content.Versions) > 1 {
		return in, nil, "", failure("invalid_request", "content.versions", "one initial version is supported")
	}
	if len(in.Content.Versions) == 1 {
		v := in.Content.Versions[0]
		if v.Fork != nil || v.State != "" && v.State != catalog.StatePublished || v.PublishAt != nil {
			return in, nil, "", failure("invalid_request", "content.versions", "initial version cannot be forked, hidden or scheduled")
		}
	}
	if len(in.Entities) > 100 {
		return in, nil, "", failure("invalid_request", "entities", "at most 100 groups are supported")
	}
	for i := range in.Entities {
		if err := s.normalizeGroup(&in.Entities[i], 0); err != nil {
			return in, nil, "", err
		}
	}
	if in.Source != nil {
		copy := *in.Source
		in.Source = &copy
		in.Source.Provider = strings.ToLower(strings.TrimSpace(copy.Provider))
		in.Source.ID = strings.TrimSpace(copy.ID)
		if in.Source.Provider == "" || len(in.Source.Provider) > 128 || in.Source.ID == "" || len(in.Source.ID) > 512 {
			return in, nil, "", failure("invalid_request", "source", "source provider and id are required and bounded")
		}
		if in.Source.URL != "" {
			u, e := url.Parse(in.Source.URL)
			if e != nil || u.Host == "" || u.Scheme != "https" && u.Scheme != "http" || u.User != nil {
				return in, nil, "", failure("invalid_request", "source.url", "source URL must be an HTTP URL without credentials")
			}
		}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return in, nil, "", failure("invalid_request", "", "request must be JSON")
	}
	sum := sha256.Sum256(b)
	return in, b, hex.EncodeToString(sum[:]), nil
}

func (s *Store) normalizeGroup(g *EntityReference, depth int) error {
	if depth > 4 {
		return failure("invalid_request", "entities.scope", "group scope nesting is too deep")
	}
	g.ID = strings.TrimSpace(g.ID)
	g.Kind = strings.TrimSpace(g.Kind)
	g.Name = strings.TrimSpace(g.Name)
	g.Language = strings.ToLower(strings.TrimSpace(g.Language))
	if g.ID != "" {
		if contentref.ValidateID(g.ID) != nil || g.Name != "" || g.Language != "" {
			return failure("invalid_request", "entities", "use a valid group id or a name, not both")
		}
	} else {
		if g.Kind == "" || g.Name == "" || len(g.Name) > 512 {
			return failure("invalid_request", "entities", "a group reference needs a kind and name")
		}
		if g.Language == "" {
			g.Language = s.o.Catalog.Kinds().Languages[0]
		}
		if !slices.Contains(s.o.Catalog.Kinds().Languages, g.Language) {
			return failure("invalid_request", "entities.language", "group language is not configured")
		}
	}
	if g.Scope != nil {
		cp := *g.Scope
		g.Scope = &cp
		return s.normalizeGroup(g.Scope, depth+1)
	}
	return nil
}

type record struct {
	Operation
	owner, hash                      string
	request                          Request
	authority                        json.RawMessage
	plan                             *media.PresignReply
	expires                          *time.Time
	commitID                         string
	commitStarted                    bool
	contentRevision, versionRevision int64
	groupIDs                         []string
}

func (s *Store) load(ctx context.Context, q catalog.Querier, id string) (record, error) {
	var r record
	var req, progress, result, fail, plan, entities []byte
	err := q.QueryRow(ctx, `SELECT id::text,owner_key,request_hash,request,authority,content_id,version_id,content_revision,version_revision,
group_ids,entity_resolutions,commit_id::text,upload,upload_expires_at,commit_started,state,progress,result,failure,created_at,updated_at FROM `+s.table+` WHERE tenant_id=$1 AND id=$2`, s.o.Tenant, id).Scan(
		&r.ID, &r.owner, &r.hash, &req, &r.authority, &r.ContentID, &r.VersionID, &r.contentRevision, &r.versionRevision, &r.groupIDs, &entities, &r.commitID, &plan, &r.expires, &r.commitStarted, &r.State, &progress, &result, &fail, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, failure("not_found", "", "publishing operation not found")
	}
	if err != nil {
		return r, err
	}
	for _, x := range []struct {
		b   []byte
		out any
	}{{req, &r.request}, {progress, &r.Progress}, {result, &r.Result}, {fail, &r.Error}, {plan, &r.plan}, {entities, &r.Entities}} {
		if len(x.b) > 0 {
			if err := json.Unmarshal(x.b, x.out); err != nil {
				return r, err
			}
		}
	}
	r.PublishAt = r.request.PublishAt
	return r, nil
}

func (s *Store) owned(ctx context.Context, actor access.Actor, id string) (record, error) {
	if !validID(id) {
		return record{}, failure("invalid_request", "id", "operation id must be a canonical nonzero UUID")
	}
	who, err := owner(actor)
	if err != nil {
		return record{}, err
	}
	r, err := s.load(ctx, s.o.Pool, id)
	if err != nil {
		return r, err
	}
	if r.owner != who {
		return record{}, failure("not_found", "", "publishing operation not found")
	}
	if err := s.allowed(ctx, actor, access.ContentWrite); err != nil {
		return record{}, err
	}
	return r, nil
}

// Put creates an operation and its draft atomically, or resumes the same
// immutable request. It renews expired upload instructions without a new draft.
func (s *Store) Put(ctx context.Context, actor access.Actor, id string, in Request) (Operation, error) {
	if !validID(id) {
		return Operation{}, failure("invalid_request", "id", "operation id must be a canonical nonzero UUID")
	}
	who, err := owner(actor)
	if err != nil {
		return Operation{}, err
	}
	if err = s.allowed(ctx, actor, access.ContentWrite); err != nil {
		return Operation{}, err
	}
	in, raw, hash, err := s.normalize(in)
	if err != nil {
		return Operation{}, err
	}
	var authority json.RawMessage
	if in.Publish == WhenReady {
		if s.o.Authority == nil {
			return Operation{}, failure("not_configured", "publish", "this authenticator cannot delegate background publication")
		}
		if authority, err = s.o.Authority.Capture(ctx, actor); err != nil {
			return Operation{}, err
		}
		if len(authority) == 0 || !json.Valid(authority) {
			return Operation{}, failure("not_configured", "publish", "background publication authority is unavailable")
		}
	}
	creation, err := s.o.Catalog.PrepareCreate(ctx, actor, []catalog.ContentInput{in.Content})
	if err != nil {
		return Operation{}, err
	}
	groupWriter := s.o.Groups.As(s.o.Auth, actor)
	if in.CreateMissingEntities {
		if _, err := groupWriter.PrepareStaffWrite(ctx); err != nil {
			return Operation{}, err
		}
	}
	tx, err := s.o.Pool.Begin(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback(ctx)
	// The same UUID never races into two drafts. Source identity is separately
	// serialized by its unique index; a collision rolls back every draft write.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "publishing:"+s.o.Tenant+":"+id); err != nil {
		return Operation{}, err
	}
	r, err := s.load(ctx, tx, id)
	if err == nil {
		if r.owner != who {
			return Operation{}, failure("not_found", "", "publishing operation not found")
		}
		if r.hash != hash {
			return Operation{}, failure("operation_conflict", "", "operation id already names different input")
		}
		if err = tx.Commit(ctx); err != nil {
			return Operation{}, err
		}
		return s.plan(ctx, actor, r)
	}
	var f *OperationError
	if !errors.As(err, &f) || f.Code != "not_found" {
		return Operation{}, err
	}
	if in.Source != nil {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table+` WHERE tenant_id=$1 AND source_provider=$2 AND source_id=$3)`, s.o.Tenant, in.Source.Provider, in.Source.ID).Scan(&exists); err != nil {
			return Operation{}, err
		}
		if exists {
			return Operation{}, failure("source_conflict", "source", "source already has a publishing operation")
		}
	}
	// The memberships written below wait out a hierarchy change before the
	// entities are locked, not after.
	if len(in.Entities) > 0 {
		if err = s.o.Groups.LockHierarchyShared(ctx, tx); err != nil {
			return Operation{}, err
		}
	}
	groupIDs, entities, err := s.resolveGroups(ctx, tx, groupWriter, in.Entities, in.CreateMissingEntities)
	if err != nil {
		return Operation{}, err
	}
	ids, err := creation.CreateTx(ctx, tx, []catalog.ContentInput{in.Content})
	if err != nil {
		return Operation{}, err
	}
	contentID := ids[0]
	var versionID string
	var cr, vr int64
	if err = tx.QueryRow(ctx, `SELECT c.revision,v.id,v.revision FROM `+s.items+` c JOIN `+s.versions+` v ON v.tenant_id=c.tenant_id AND v.content_id=c.id WHERE c.tenant_id=$1 AND c.id=$2`, s.o.Tenant, contentID).Scan(&cr, &versionID, &vr); err != nil {
		return Operation{}, err
	}
	if len(groupIDs) > 0 {
		ms := make([]groups.Membership, len(groupIDs))
		for i, g := range groupIDs {
			ms[i] = groups.Membership{GroupID: groups.GroupID(g)}
		}
		if err = s.o.Groups.SetContentGroupsTx(ctx, tx, []groups.ContentGroups{{ContentID: contentID, Memberships: ms}}); err != nil {
			return Operation{}, err
		}
		// A host's facet callback may advance the catalog revision.
		if err = tx.QueryRow(ctx, `SELECT revision FROM `+s.items+` WHERE tenant_id=$1 AND id=$2`, s.o.Tenant, contentID).Scan(&cr); err != nil {
			return Operation{}, err
		}
	}
	var provider, source any
	if in.Source != nil {
		provider, source = in.Source.Provider, in.Source.ID
	}
	_, err = tx.Exec(ctx, `INSERT INTO `+s.table+`(tenant_id,id,owner_key,request_hash,request,authority,content_id,version_id,content_revision,version_revision,group_ids,entity_resolutions,commit_id,source_provider,source_id,publish_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, s.o.Tenant, id, who, hash, raw, authority, contentID, versionID, cr, vr, groupIDs, jsonBytes(entities), uuid.NewString(), provider, source, in.PublishAt)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return Operation{}, failure("source_conflict", "source", "source already has a publishing operation")
	}
	if err != nil {
		return Operation{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Operation{}, err
	}
	r, err = s.load(ctx, s.o.Pool, id)
	if err != nil {
		return Operation{}, err
	}
	return s.plan(ctx, actor, r)
}

// Get is read-only. It never starts uploads, changes workflow state or publishes.
func (s *Store) Get(ctx context.Context, actor access.Actor, id string) (Operation, error) {
	r, err := s.owned(ctx, actor, id)
	return r.Operation, err
}

func (s *Store) ref(r record) (contentref.ContentRef, error) {
	return s.o.Registry.VersionRef(r.request.Content.Kind, r.ContentID, r.VersionID)
}

func (s *Store) plan(ctx context.Context, actor access.Actor, r record) (Operation, error) {
	if r.State != "awaiting_upload" {
		return r.Operation, nil
	}
	if r.plan != nil && (r.commitStarted || r.expires != nil && r.expires.After(s.o.Clock().Add(time.Minute))) {
		r.Upload = r.plan
		return r.Operation, nil
	}
	// A short database lease reserves transport renewal. No connection is held
	// while storage or its manifest journal runs.
	lease := uuid.NewString()
	tag, err := s.o.Pool.Exec(ctx, `UPDATE `+s.table+` SET lease_id=$3,lease_until=now()+interval '2 minutes' WHERE tenant_id=$1 AND id=$2 AND state='awaiting_upload' AND NOT commit_started AND (lease_until IS NULL OR lease_until<now())`, s.o.Tenant, r.ID, lease)
	if err != nil {
		return Operation{}, err
	}
	if tag.RowsAffected() == 0 {
		return r.Operation, &OperationError{Code: "operation_busy", Message: "upload instructions are being prepared; retry", Retryable: true}
	}
	defer s.release(ctx, r.ID, lease)
	ref, err := s.ref(r)
	if err != nil {
		return Operation{}, err
	}
	k, _ := s.o.Registry.Kind(r.request.Content.Kind)
	sum, _ := hex.DecodeString(r.request.Archive.SHA256)
	p, err := s.o.Uploads.Presign(ctx, actor, media.PresignRequest{Ref: ref, Path: strings.Replace(k.Publish.Import, "{name}", "source.zip", 1), Type: "application/zip", Size: r.request.Archive.Size, SHA256: sum})
	if err != nil {
		return Operation{}, err
	}
	out := media.PresignReply{Path: p.Path, Blob: p.Blob, Exists: p.Exists, ProcessOnUpload: p.ProcessOnUpload}
	expires := s.o.Clock().Add(23 * time.Hour)
	if p.Put != nil {
		headers := map[string]string{}
		for k, vs := range p.Put.Header {
			headers[k] = strings.Join(vs, ",")
		}
		out.Put = &media.RequestReply{Method: p.Put.Method, URL: p.Put.URL, Headers: headers, Expires: p.Put.Expires}
		expires = p.Put.Expires
	}
	if p.Multipart != nil {
		m := p.Multipart
		out.Multipart = &media.MultipartReply{Ticket: m.Ticket, MinPartSize: m.MinPartSize, MaxPartSize: m.MaxPartSize, MaxParts: m.MaxParts}
	}
	b, _ := json.Marshal(out)
	tag, err = s.o.Pool.Exec(ctx, `UPDATE `+s.table+` SET upload=$4,upload_expires_at=$5,updated_at=now() WHERE tenant_id=$1 AND id=$2 AND lease_id=$3 AND NOT commit_started`, s.o.Tenant, r.ID, lease, b, expires)
	if err != nil {
		return Operation{}, err
	}
	if tag.RowsAffected() != 1 {
		return r.Operation, &OperationError{Code: "operation_busy", Message: "upload lease changed while preparing; retry", Retryable: true}
	}
	r.Upload = &out
	return r.Operation, nil
}
