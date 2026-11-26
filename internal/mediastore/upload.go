package mediastore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/clientaddr"
	"github.com/open-rails/contentkit/internal/ratelimit"
	"github.com/open-rails/contentkit/internal/uuidv7"
	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
	"golang.org/x/sync/errgroup"
)

// UploadOptions configure Uploads.
type UploadOptions struct {
	Store     media.Store
	Manifests *Manifests   // Hooks authorize uploads
	Processes ProcessQueue // places and processes committed uploads; optional
	Tickets   *token.Ring  // signs multipart tickets (domain-separated from access tokens); required above MaxSinglePut
	// PresignTTL bounds PUT and part URLs; default 15m. TicketTTL bounds a
	// multipart ticket; default 24h, the bucket's abort-incomplete rule.
	PresignTTL, TicketTTL time.Duration
	// ProcessOnUpload tells the SDK to commit each upload unattached as
	// soon as it lands, so the worker processes it while the user arranges
	// the rest; an attach op makes it part of the item.
	ProcessOnUpload bool
	// Commits limits each uploader's commits (default 1/s, burst 30, per
	// process; set Commits.Redis to share it); exempt grants are not
	// limited.
	Commits RateLimit
}

// TemporaryRetention is how long tmp/ keeps a staged upload: the bucket's
// tag-based expiry (s3.Store.Configure).
const TemporaryRetention = 7 * 24 * time.Hour

// Uploads presigns direct-to-bucket uploads and commits them. It keeps no
// state: a multipart upload is its S3 UploadId, carried in a signed ticket.
type Uploads struct {
	o       UploadOptions
	reg     *Registry
	commits *ratelimit.Limiter
}

func NewUploads(o UploadOptions) (*Uploads, error) {
	if o.Store == nil || o.Manifests == nil {
		return nil, errors.New("media: Uploads needs a Store and Manifests")
	}
	reg := o.Manifests.Registry()
	if reg.hooks.CanUpload == nil {
		return nil, errors.New("media: Uploads needs Hooks.CanUpload")
	}
	if o.PresignTTL <= 0 {
		o.PresignTTL = 15 * time.Minute
	}
	if o.TicketTTL <= 0 {
		o.TicketTTL = 24 * time.Hour
	}
	if o.PresignTTL > TemporaryRetention || o.TicketTTL > TemporaryRetention {
		return nil, errors.New("media: upload capabilities cannot outlive seven-day temporary storage")
	}
	return &Uploads{o: o, reg: reg,
		commits: newRateLimiter(o.Commits, commitLimit, "commit", time.Now, slog.Default())}, nil
}

// Presign checks an upload against its path's Upload and the app's
// permission, and plans it.
func (u *Uploads) Presign(ctx context.Context, actor access.Actor, r media.PresignRequest) (media.Presigned, error) {
	item, err := u.item(r.Ref)
	if err != nil {
		return media.Presigned{}, err
	}
	if r.Type == "" || r.Size <= 0 || len(r.SHA256) != sha256.Size {
		return media.Presigned{}, uploadErr(media.CodeInvalid, "type, size and the file's SHA-256 are required")
	}
	path, g, err := u.uploadPath(item.Kind(), r.Path, r.Type)
	if err != nil {
		return media.Presigned{}, err
	}
	if err := allows(item.Kind().Uploads[g], r.Type, r.Size); err != nil {
		return media.Presigned{}, err
	}
	stem, _ := splitExt(path)
	grant, err := u.authorize(ctx, actor, media.UploadTarget{Ref: r.Ref, Path: stem})
	if err != nil {
		return media.Presigned{}, err
	}
	if err := RequireWritable(ctx, u.o.Store); err != nil {
		return media.Presigned{}, err
	}
	out := media.Presigned{Path: path, SourceName: r.Path, ProcessOnUpload: u.o.ProcessOnUpload}
	// Bytes the item already holds in its upload area need no upload.
	// Anything else is staged: nothing a client sends is ever written under
	// a hash name, since the bucket does not verify checksums.
	current, err := u.current(ctx, item)
	if err != nil {
		return media.Presigned{}, err
	}
	views, area := u.reg.editorViews(current), item.Kind().Area("")
	for _, blob := range current.Blobs() {
		sum, ok := layout.BlobDigest(blob)
		if a, _, _ := layout.SplitBlobRef(blob); !ok || !bytes.Equal(sum, r.SHA256) || views[blob] || a != area {
			continue
		}
		key, _ := item.Blob(blob)
		obj, err := u.o.Store.Head(ctx, key)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return media.Presigned{}, err
		}
		if err == nil && obj.Size == r.Size && obj.ContentType == r.Type {
			out.Blob, out.Exists = blob, true
			return out, nil
		}
	}
	_ = grant
	out.Blob = NewStaged()
	key, _ := item.Staged(out.Blob)
	if err := u.presign(ctx, item, key, uploaderID(actor), r, &out); err != nil {
		return media.Presigned{}, err
	}
	return out, nil
}

// CallerOperation is the manifest op of a caller's commit operation_id.
// ContentKit's own edits take ops derived from public inputs (an item, a
// source); hashing a caller's id keeps every one of them out of its reach.
// The op is dated at the id's own instant, the Unix epoch for a UUIDv4.
func CallerOperation(id uuid.UUID) uuid.UUID {
	return uuidv7.Name(uuidv7.At(id.String()), uuid.Nil, "caller-commit:"+id.String())
}

// NewStaged is a fresh staged upload name: "u-{uuid}".
func NewStaged() string { return layout.StagedPrefix + uuidv7.NewString() }

// uploadPath is the path to commit for a requested one: its {name} cleaned
// (or chosen, for a Named upload) and an extension from the type when it
// has none; g is its Upload.
func (u *Uploads) uploadPath(k *RegisteredKind, path, contentType string) (string, int, error) {
	stem, ext := splitExt(path)
	dir, name := "", stem
	if i := strings.LastIndexByte(stem, '/'); i >= 0 {
		dir, name = stem[:i+1], stem[i+1:]
	}
	g, _, _, _, ok := k.upload(stem)
	if !ok {
		stem = dir + CleanName(name)
		if g, _, _, _, ok = k.upload(stem); !ok {
			g, _, _, _, ok = k.upload(dir + NewName())
		}
	}
	if !ok {
		return "", 0, uploadErr(media.CodeNotFound, "kind %q has no upload path %q", k.Name, path)
	}
	if k.Uploads[g].Named {
		stem = dir + NewName()
	}
	if ext == "" {
		ext = typeExt(contentType)
	}
	return stem + "." + ext, g, nil
}

func (u *Uploads) presign(ctx context.Context, item Item, key, uploader string, r media.PresignRequest, out *media.Presigned) error {
	if r.Size <= media.MaxSinglePut {
		p, err := u.o.Store.PresignPut(ctx, key, media.PresignPut{ContentType: r.Type, Size: r.Size, SHA256: r.SHA256, TTL: u.o.PresignTTL})
		out.Put = &p
		return err
	}
	if u.o.Tickets == nil {
		return errors.New("media: UploadOptions.Tickets is required for multipart uploads")
	}
	id, err := u.o.Store.CreateMultipart(ctx, key, r.Type)
	if err != nil {
		return err
	}
	t := ticket{Ref: item.Ref(), Blob: out.Blob, UploadID: id, Type: r.Type, Size: r.Size, Uploader: uploader,
		Exp: time.Now().Add(u.o.TicketTTL).Unix()}
	sealed, err := u.seal(t)
	if err != nil {
		_ = u.o.Store.AbortMultipart(context.WithoutCancel(ctx), key, id)
		return err
	}
	out.Multipart = &media.Multipart{Ticket: sealed, MinPartSize: media.MinPartSize, MaxPartSize: media.MaxPartSize, MaxParts: maxParts(r.Size)}
	return nil
}

// PresignParts signs parts of a multipart upload, each bound to its length
// and SHA-256. Parts re-signed after a failure replace the earlier attempt.
func (u *Uploads) PresignParts(ctx context.Context, actor access.Actor, sealed string, parts []media.PartRequest) ([]media.PresignedPart, error) {
	t, key, err := u.open(actor, sealed)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 || len(parts) > 100 {
		return nil, uploadErr(media.CodeInvalid, "presign 1 to 100 parts at a time")
	}
	limit := maxParts(t.Size)
	out := make([]media.PresignedPart, 0, len(parts))
	for _, p := range parts {
		if p.Number < 1 || int(p.Number) > limit || p.Size <= 0 || p.Size > min(media.MaxPartSize, t.Size) || len(p.SHA256) != sha256.Size {
			return nil, uploadErr(media.CodeInvalid, "part %d: number must be 1-%d, size 1-%d bytes, with a SHA-256", p.Number, limit, min(media.MaxPartSize, t.Size))
		}
		req, err := u.o.Store.PresignPart(ctx, key, t.UploadID, p.Number, p.Size, p.SHA256, u.o.PresignTTL)
		if err != nil {
			return nil, err
		}
		out = append(out, media.PresignedPart{Number: p.Number, PresignedRequest: req})
	}
	return out, nil
}

// ListParts reports the parts that landed, for resuming.
func (u *Uploads) ListParts(ctx context.Context, actor access.Actor, sealed string) ([]media.Part, error) {
	t, key, err := u.open(actor, sealed)
	if err != nil {
		return nil, err
	}
	return u.listParts(ctx, key, t.UploadID)
}

func (u *Uploads) listParts(ctx context.Context, key, id string) ([]media.Part, error) {
	parts, err := u.o.Store.ListParts(ctx, key, id)
	if errors.Is(err, ErrNotFound) {
		return nil, uploadErr(media.CodeNotFound, "multipart upload not found (completed, aborted or expired)")
	}
	return parts, err
}

// UploadedBlob is a completed multipart upload; Blob is its staged name.
type UploadedBlob struct {
	Blob string
	Type string
	Size int64
}

// Complete assembles the parts server-side. Missing parts answer
// CodeIncomplete and keep the upload; parts that cannot add up to the
// declared size abort it.
func (u *Uploads) Complete(ctx context.Context, actor access.Actor, sealed string) (UploadedBlob, error) {
	t, key, err := u.open(actor, sealed)
	if err != nil {
		return UploadedBlob{}, err
	}
	done := UploadedBlob{Blob: t.Blob, Type: t.Type, Size: t.Size}
	parts, err := u.listParts(ctx, key, t.UploadID)
	if err != nil {
		// A retried Complete finds the assembled object instead of the upload.
		if obj, herr := u.o.Store.Head(ctx, key); herr == nil && obj.Size == t.Size {
			return done, nil
		}
		return UploadedBlob{}, err
	}
	caps, err := u.o.Store.Capabilities(ctx)
	if err != nil {
		return UploadedBlob{}, err
	}
	var total int64
	for i, p := range parts {
		last := i == len(parts)-1
		switch {
		case p.Number != int32(i+1):
			return UploadedBlob{}, uploadErr(media.CodeIncomplete, "part %d is missing", i+1)
		case p.Size > media.MaxPartSize || (!last && p.Size < media.MinPartSize):
			return UploadedBlob{}, u.abort(ctx, t, key, uploadErr(media.CodeInvalid, "part %d is %d bytes; parts are %d-%d bytes, the last may be smaller", p.Number, p.Size, media.MinPartSize, media.MaxPartSize))
		case caps.ChecksumSHA256 && len(p.SHA256) != sha256.Size:
			return UploadedBlob{}, u.abort(ctx, t, key, uploadErr(media.CodeChecksum, "part %d has no SHA-256", p.Number))
		}
		total += p.Size
	}
	switch {
	case total > t.Size:
		return UploadedBlob{}, u.abort(ctx, t, key, uploadErr(media.CodeInvalid, "parts total %d bytes; declared %d", total, t.Size))
	case total < t.Size:
		return UploadedBlob{}, uploadErr(media.CodeIncomplete, "%d of %d bytes uploaded", total, t.Size)
	}
	if _, err := u.o.Store.CompleteMultipart(ctx, key, t.UploadID, parts); err != nil {
		return UploadedBlob{}, err
	}
	return done, nil
}

// Abort cancels a multipart upload.
func (u *Uploads) Abort(ctx context.Context, actor access.Actor, sealed string) error {
	t, key, err := u.open(actor, sealed)
	if err != nil {
		return err
	}
	return u.abort(ctx, t, key, nil)
}

func (u *Uploads) abort(ctx context.Context, t ticket, key string, cause error) error {
	ctx = context.WithoutCancel(ctx)
	err := u.o.Store.AbortMultipart(ctx, key, t.UploadID)
	if cause != nil {
		return cause
	}
	return err
}

// Commit applies ops to ref's manifest in one conditional write. operationID
// is a caller-stable UUIDv4 or UUIDv7 for the complete ordered batch: reuse it
// after an uncertain response, but never for different inputs; it is the
// write's op, so a retry recognises a write that landed. Every op is
// authorized against what it writes (Hooks.CanUpload), before any copy and
// again on every attempt, so a grant revoked while the commit retried writes
// nothing. A put names a staged upload or a blob in the folder, HEAD-checked
// against its Upload; copies are copied server-side first. Then the worker
// places staged uploads and processes the item. What the commit stops
// referencing is retired (a grace period, or at once for a takedown).
func (u *Uploads) Commit(ctx context.Context, actor access.Actor, ref contentref.ContentRef, operationID string, ops []media.Op) (*Manifest, error) {
	item, err := u.item(ref)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(operationID)
	if err != nil || id == uuid.Nil || id.String() != operationID {
		return nil, uploadErr(media.CodeInvalid, "operation_id must be a canonical nonzero UUID")
	}
	trusted := actor.Kind == "contentkit:system" && actor.ID == "contentkit" && !actor.Anonymous
	if !trusted {
		if id.Variant() != uuid.RFC4122 || id.Version() != 4 && id.Version() != 7 {
			return nil, uploadErr(media.CodeInvalid, "operation_id must be a UUIDv4 or UUIDv7")
		}
		id = CallerOperation(id)
	}
	limit := 1000
	if trusted {
		limit = 2*media.MaxUploads + 1 // one metadata put and move per source, plus regeneration
	}
	if len(ops) == 0 || len(ops) > limit {
		return nil, uploadErr(media.CodeInvalid, "commit 1 to %d operations", limit)
	}
	// Page provenance, analysis and selection are server-owned. External
	// upload metadata cannot impersonate an analyzer or an audited override.
	if !trusted {
		ops = clientUploadOps(ops)
	}
	for _, op := range ops {
		if err := validateOp(op); err != nil {
			return nil, err
		}
	}
	grant, err := u.authorizeOps(ctx, actor, item, ops)
	if err != nil {
		return nil, err
	}
	if err := RequireWritable(ctx, u.o.Store); err != nil {
		return nil, err
	}
	if !grant.Exempt {
		if slices.ContainsFunc(ops, func(op media.Op) bool { return op.Op == media.OpRegenerate && op.Force }) {
			return nil, uploadErr(media.CodeForbidden, "forced regeneration needs an exempt grant")
		}
		if u.commits != nil {
			if ok, wait := u.commits.Allow(ctx, uploaderID(actor)); !ok {
				return nil, &media.UploadError{Code: media.CodeRate, Message: "commit rate limit", RetryAfter: wait}
			}
		}
	}
	frames, err := u.frameVideos(ctx, item, ops)
	if err != nil {
		return nil, err
	}
	copies, err := u.copies(ctx, actor, grant, item, ops)
	if err != nil {
		return nil, err
	}
	takedown := slices.ContainsFunc(ops, func(op media.Op) bool { return op.Takedown })
	var renames map[string]string
	var removed []string
	var cancel bool
	sum, err := opsDigest(ops)
	if err != nil {
		return nil, err
	}
	// A commit is refused when the item, processed and released, would not
	// fit: later writes could not record its outputs or move its files.
	edit := EditOptions{Op: id.String(), Sum: sum, Reserve: item.Kind().Unwritten}
	if takedown {
		edit.Grace = -1
	}
	man, err := u.o.Manifests.EditWith(ctx, ref, edit, func(m *Manifest) error {
		// Authority may change while the commit retries.
		current, err := u.authorizeOps(ctx, actor, item, ops)
		if err != nil {
			return err
		}
		if current.Exempt != grant.Exempt {
			return uploadErr(media.CodeConflict, "the upload grant changed during the commit: try again")
		}
		prior := m.Clone()
		o := &opRun{k: item.Kind(), m: m, ref: ref, copies: copies, frames: frames, exempt: grant.Exempt, trusted: trusted}
		o.renames = map[string]string{}
		for _, file := range prior.Files {
			if file.IsUpload() {
				o.renames[file.Path] = file.Path
			}
		}
		o.archiveAllowed = func(source, destination string) error {
			stem, _ := splitExt(source)
			delegated, err := u.authorize(ctx, actor, media.UploadTarget{Ref: ref, Path: stem})
			if err != nil {
				return err
			}
			if delegated.ArchiveTo != destination || delegated.Exempt != grant.Exempt {
				return uploadErr(media.CodeForbidden, "ZIP expansion requires an explicitly approved destination")
			}
			return nil
		}
		admissions := map[string]*media.SourceAdmission{}
		o.admission = func(source, destination string) (*media.SourceAdmission, error) {
			key := source + "\x00" + destination
			if prior := admissions[key]; prior != nil {
				return prior, nil
			}
			if err := o.archiveAllowed(source, destination); err != nil {
				return nil, err
			}
			admission := &media.SourceAdmission{Generation: operationID, Target: destination}
			admissions[key] = admission
			return admission, nil
		}
		o.authorizePath = func(path string) error {
			stem, _ := splitExt(path)
			allocated, err := u.authorize(ctx, actor, media.UploadTarget{Ref: ref, Path: stem})
			if err != nil {
				return err
			}
			if allocated.Exempt != grant.Exempt {
				return uploadErr(media.CodeForbidden, "the page has a different upload grant")
			}
			return nil
		}
		var names []string
		for _, op := range ops {
			if op.Op != media.OpPut {
				continue
			}
			created, conflict := o.created(op)
			// Defer retries and conflicts to the ordered ops below. Their
			// staged object may already be gone, or an earlier op may remove them.
			if !created && conflict == nil && !slices.Contains(names, op.Blob) {
				names = append(names, op.Blob)
			}
		}
		objects, err := u.verify(ctx, item, m, names)
		if err != nil {
			return err
		}
		o.objects = objects
		uploads := m.uploads()
		for n, op := range ops {
			if op.Op == media.OpPut {
				created, err := o.created(op)
				if err != nil {
					return err
				}
				// Placement may have removed the staged object. Its receipt
				// proves a retry without restoring old attachment or edits.
				if created {
					continue
				}
				if _, ok := o.objects[op.Blob]; !ok {
					// An earlier op in this batch removed or replaced the receipt.
					objects, err := u.verify(ctx, item, m, []string{op.Blob})
					if err != nil {
						return err
					}
					o.objects[op.Blob] = objects[op.Blob]
				}
			}
			if err := o.apply(n, op); err != nil {
				return err
			}
		}
		renames = map[string]string{}
		for before, after := range o.renames {
			if before != after {
				renames[before] = after
			}
		}
		removed = o.removed
		if n := m.uploads(); n > media.MaxUploads && n > uploads {
			return tooManyFiles(media.MaxUploads, "an item holds at most %d uploads", media.MaxUploads)
		}
		cancel = removesPending(prior, ops)
		if hook := u.reg.hooks.ValidatePathsChanged; hook != nil && (len(renames) > 0 || len(removed) > 0) {
			return hook(ctx, ref, renames, removed)
		}
		return nil
	})
	if errors.Is(err, ErrManifestTooLarge) {
		err = uploadErr(media.CodeTooLarge, "the item, processed, would pass its manifest's %d MiB: remove uploads or meta first", media.MaxManifestBytes>>20)
	} else if errors.Is(err, ErrOpReused) {
		err = uploadErr(media.CodeConflict, "operation_id was used for a different batch")
	}
	if err != nil {
		return nil, err
	}
	if hook := u.reg.hooks.PathsChanged; hook != nil && (len(renames) > 0 || len(removed) > 0) {
		if err := hook(ctx, ref, renames, removed); err != nil {
			return nil, err
		}
	}
	if u.o.Processes != nil {
		if canceler, ok := u.o.Processes.(interface {
			Cancel(context.Context, contentref.ContentRef) (int, error)
		}); ok && cancel {
			if _, err := canceler.Cancel(ctx, ref); err != nil {
				return nil, err
			}
		}
		job := ProcessJob{Ref: ref, Place: len(man.StagedNames()) > 0 || item.Kind().pendingArchives(man)}
		for _, op := range ops {
			if op.Op == media.OpRegenerate {
				job.Preset, job.Force = op.Preset, op.Force
			}
		}
		if job.Place || job.Force || item.Kind().NeedsProcessing(man) {
			if err := u.o.Processes.Enqueue(ctx, job); err != nil {
				return nil, err
			}
		}
	}
	if hook := u.reg.hooks.ItemCommitted; hook != nil {
		if err := hook(ctx, ref); err != nil {
			return nil, fmt.Errorf("media: ItemCommitted: %w", err)
		}
	}
	return man, nil
}

// missing are the names in before that are not in after.
func missing(before, after []string) []string {
	return slices.DeleteFunc(slices.Clone(before), func(s string) bool { return slices.Contains(after, s) })
}

// publicPending reports an upload of a visible item waiting for a public
// preset.
func publicPending(k *RegisteredKind, m *Manifest) bool {
	return slices.ContainsFunc(m.Files, func(f media.File) bool {
		return f.IsUpload() && slices.ContainsFunc(f.Pending, func(p string) bool { return k.public(p) != nil })
	})
}

// removesPending reports ops removing an upload that is still being
// processed: its running jobs are cancelled rather than finished.
func removesPending(m *Manifest, ops []media.Op) bool {
	for _, op := range ops {
		if f, ok := m.Get(op.Path); op.Op == media.OpRemove && ok && f.IsUpload() && len(f.Pending) > 0 {
			return true
		}
	}
	return false
}

// authorizeOps checks every op's target once; the grants must agree on the
// quota owner. A copy, or a frame of another item's video, also needs the
// right to write its source.
func (u *Uploads) authorizeOps(ctx context.Context, actor access.Actor, item Item, ops []media.Op) (media.UploadGrant, error) {
	type target struct {
		ref  contentref.ContentKey
		path string
	}
	seen := map[target]media.UploadGrant{}
	var grant *media.UploadGrant
	check := func(t media.UploadTarget) (media.UploadGrant, error) {
		key := target{t.Ref.Key(), t.Path}
		if known, ok := seen[key]; ok {
			return known, nil
		}
		g, err := u.authorize(ctx, actor, t)
		if err != nil {
			return media.UploadGrant{}, err
		}
		if t.Ref.Equal(item.Ref()) {
			if grant == nil {
				grant = &g
			} else if g.Exempt != grant.Exempt {
				return media.UploadGrant{}, uploadErr(media.CodeForbidden, "the commit's paths have different grants")
			}
		}
		seen[key] = g
		return g, nil
	}
	for _, op := range ops {
		t := media.UploadTarget{Ref: item.Ref()}
		if op.Path != "" {
			t.Path, _ = splitExt(op.Path)
		}
		if op.Op == media.OpCopy {
			src, err := u.reg.VersionRef(item.Kind().Name, op.From.ID, op.From.Version)
			if err != nil {
				return media.UploadGrant{}, uploadErr(media.CodeInvalid, "copy from %q: not a media item of this kind", op.From.ID)
			}
			stem, _ := splitExt(op.From.Path)
			if _, err := check(media.UploadTarget{Ref: src, Path: stem}); err != nil {
				return media.UploadGrant{}, err
			}
			t.Path, _ = splitExt(cmpOr(op.To, op.From.Path))
		}
		if op.Op == media.OpFrame && op.From != nil {
			src, err := u.frameRef(item, op)
			if err != nil {
				return media.UploadGrant{}, err
			}
			stem, _ := splitExt(op.From.Path)
			if _, err := check(media.UploadTarget{Ref: src, Path: stem}); err != nil {
				return media.UploadGrant{}, err
			}
		}
		destination, err := check(t)
		if err != nil {
			return media.UploadGrant{}, err
		}
		if expected := item.Kind().ArchiveDestination(t.Path); op.Op == media.OpCopy && expected != "" && destination.ArchiveTo != expected {
			return media.UploadGrant{}, uploadErr(media.CodeForbidden, "ZIP copy requires an explicitly approved destination")
		}
	}
	return *grant, nil
}

// frameRef is the item a frame op's From names, in item's namespace.
func (u *Uploads) frameRef(item Item, op media.Op) (contentref.ContentRef, error) {
	src, err := u.reg.VersionRef(cmpOr(op.From.Kind, item.Kind().Name), op.From.ID, op.From.Version)
	if err != nil || src.TenantID != item.Ref().TenantID {
		return src, uploadErr(media.CodeInvalid, "frame from %q: not a media item here", op.From.ID)
	}
	return src, nil
}

// frameVideos reads the videos frame ops name in other items, by op index.
// The worker checks them again when it grabs.
func (u *Uploads) frameVideos(ctx context.Context, item Item, ops []media.Op) (map[int]media.File, error) {
	out := map[int]media.File{}
	for n, op := range ops {
		if op.Op != media.OpFrame || op.From == nil {
			continue
		}
		src, err := u.frameRef(item, op)
		if err != nil {
			return nil, err
		}
		if src.Equal(item.Ref()) {
			continue // read under the item's manifest lock
		}
		m, _, err := u.o.Manifests.Get(ctx, src)
		if errors.Is(err, ErrNotFound) {
			m = &Manifest{}
		} else if err != nil {
			return nil, err
		}
		v, ok := uploadAt(m, op.From.Path)
		if err := frameVideo(v, ok, op); err != nil {
			return nil, err
		}
		out[n] = v
	}
	return out, nil
}

// verify HEAD-checks the put names in item's folder: staged uploads in
// tmp/, and blobs the item references now. A blob's bytes were hashed when
// they were placed or produced, so they are not read again; a name from
// another item's folder is simply absent here.
func (u *Uploads) verify(ctx context.Context, item Item, m *Manifest, names []string) (map[string]media.Object, error) {
	out := make(map[string]media.Object, len(names))
	var missing []string
	views := u.reg.editorViews(m)
	for _, n := range names {
		key, err := item.Staged(n)
		if err != nil {
			key, _ = item.Blob(n)
		}
		obj, err := u.o.Store.Head(ctx, key)
		if errors.Is(err, ErrNotFound) {
			missing = append(missing, n)
			continue
		} else if err != nil {
			return nil, err
		}
		if layout.ValidBlobRef(n) && !u.reusable(m, views, n) {
			missing = append(missing, n)
			continue
		}
		out[n] = obj
	}
	if len(missing) > 0 {
		return nil, &media.UploadError{Code: media.CodeNotUploaded, Blobs: missing,
			Message: "not uploaded or due for cleanup; upload again: " + strings.Join(missing, ", ")}
	}
	return out, nil
}

// reusable accepts only blobs the item references, excluding editor-only
// views: an unreferenced one may be due for collection.
func (u *Uploads) reusable(m *Manifest, views map[string]bool, blob string) bool {
	if views[blob] {
		return false
	}
	return slices.Contains(m.Blobs(), blob)
}

// current is item's manifest, empty when it has none.
func (u *Uploads) current(ctx context.Context, item Item) (*Manifest, error) {
	m, _, err := u.o.Manifests.GetAny(ctx, item.Ref())
	switch {
	case errors.Is(err, ErrNotFound):
		return &Manifest{}, nil // a new item
	case err == nil && m.Deleted != nil:
		return nil, ErrNotFound // deleted or erased: no uploads
	}
	return m, err
}

// copies reads each copy op's source files from the other item and copies
// their bytes server-side into this item under the same hashes, each in the
// area its destination file lands in, keyed by op index.
func (u *Uploads) copies(ctx context.Context, actor access.Actor, grant media.UploadGrant, item Item, ops []media.Op) (map[int][]media.File, error) {
	type copyJob struct{ src, dst string }
	var jobs []copyJob
	seen := map[string]bool{}
	manifest := map[contentref.ContentKey]*Manifest{}
	out := map[int][]media.File{}
	for n, op := range ops {
		if op.Op != media.OpCopy {
			continue
		}
		src, err := u.reg.VersionRef(item.Kind().Name, op.From.ID, op.From.Version)
		if err != nil {
			return nil, uploadErr(media.CodeInvalid, "copy from %q: not a media item of this kind", op.From.ID)
		}
		if src.Equal(item.Ref()) {
			continue // read from the destination's own manifest by the edit
		}
		from, _ := u.reg.Item(src)
		m, ok := manifest[src.Key()]
		if !ok {
			if m, _, err = u.o.Manifests.Get(ctx, src); errors.Is(err, ErrNotFound) {
				return nil, uploadErr(media.CodeNotFound, "item %s has no media", op.From.ID)
			} else if err != nil {
				return nil, err
			}
			manifest[src.Key()] = m
		}
		files := copyFiles(m, op.From.Path)
		if len(files) == 0 {
			return nil, uploadErr(media.CodeNotFound, "no upload %q in item %s", op.From.Path, op.From.ID)
		}
		to, _, _, _, _ := item.Kind().upload(cmpOr(op.To, op.From.Path))
		group, _, _, _, _ := item.Kind().upload(op.From.Path)
		if to != group || op.Edit != nil {
			files = files[:1]
		}
		for i := range files {
			f := &files[i]
			f.Editor = nil // editor views belong to their original item
			if !f.Gone && f.Blob != "" {
				dst := layout.MoveRef(f.Blob, copyArea(item.Kind(), i, *f))
				if key := keyOf(item, dst); !seen[key] {
					seen[key] = true
					jobs = append(jobs, copyJob{keyOf(from, f.Blob), key})
				}
				f.Blob = dst
			}
			if f.Track != nil && f.Track.Index != "" {
				track := *f.Track
				dst := layout.MoveRef(track.Index, copyIndexArea(item.Kind()))
				if key := keyOf(item, dst); !seen[key] {
					seen[key] = true
					jobs = append(jobs, copyJob{keyOf(from, track.Index), key})
				}
				track.Index = dst
				f.Track = &track
			}
		}
		out[n] = files
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for _, j := range jobs {
		g.Go(func() error {
			if _, err := u.o.Store.Copy(gctx, j.src, j.dst, media.CopyOptions{}); err != nil {
				return fmt.Errorf("media: copy %s: %w", j.src, err)
			}
			return nil
		})
	}
	return out, g.Wait()
}

func copyIndexArea(k *RegisteredKind) string {
	if k.Versions {
		return layout.AreaInternal
	}
	return layout.AreaPrivate
}

// copyArea is the area the i-th file of a copy lands in.
func copyArea(k *RegisteredKind, i int, f media.File) string {
	if i == 0 {
		return k.Area("")
	}
	return k.Area(f.Preset)
}

func (u *Uploads) item(ref contentref.ContentRef) (Item, error) {
	item, err := u.reg.Item(ref)
	if err != nil {
		if errors.Is(err, ErrUnknownKind) {
			return Item{}, err
		}
		return Item{}, uploadErr(media.CodeInvalid, "not a valid media reference")
	}
	return item, nil
}

func (u *Uploads) authorize(ctx context.Context, actor access.Actor, t media.UploadTarget) (media.UploadGrant, error) {
	g, err := u.reg.hooks.CanUpload.CanUpload(ctx, actor, t)
	if err != nil {
		return media.UploadGrant{}, fmt.Errorf("media: upload permission check: %w", err)
	}
	if !g.Allowed {
		return media.UploadGrant{}, uploadErr(media.CodeForbidden, "upload not allowed")
	}
	return g, nil
}

func uploaderID(a access.Actor) string {
	if a.Anonymous || a.ID == "" {
		return "ip:" + clientaddr.Key(a.IP)
	}
	return a.ID
}

func maxParts(size int64) int { return int((size + media.MinPartSize - 1) / media.MinPartSize) }

// uploads counts the manifest's uploads.
func (m *Manifest) uploads() int {
	n := 0
	for _, f := range m.Files {
		if f.IsUpload() {
			n++
		}
	}
	return n
}

// ticket binds a multipart upload to its item, staged name, declared size
// and type, and uploader. It is signed with the token ring under an "upload|"
// scope that no object path can equal, so it never works as an access token.
type ticket struct {
	Ref      contentref.ContentRef `json:"r"`
	Blob     string                `json:"b"`
	UploadID string                `json:"u"`
	Type     string                `json:"t"`
	Size     int64                 `json:"s"`
	Uploader string                `json:"a"`
	Exp      int64                 `json:"e"`
}

func (u *Uploads) seal(t ticket) (string, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + u.o.Tickets.Sign("upload|"+payload, time.Unix(t.Exp, 0)), nil
}

func (u *Uploads) open(actor access.Actor, sealed string) (ticket, string, error) {
	invalid := uploadErr(media.CodeInvalid, "invalid or expired upload ticket")
	payload, sig, ok := strings.Cut(sealed, ".")
	if !ok || u.o.Tickets == nil || u.o.Tickets.VerifyScope(sig, "upload|"+payload, time.Now()) != nil {
		return ticket{}, "", invalid
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	var t ticket
	if err != nil || json.Unmarshal(b, &t) != nil {
		return ticket{}, "", invalid
	}
	if t.Uploader != uploaderID(actor) {
		return ticket{}, "", uploadErr(media.CodeForbidden, "upload ticket belongs to another uploader")
	}
	item, err := u.reg.Item(t.Ref)
	if err != nil {
		return ticket{}, "", invalid
	}
	key, err := item.Staged(t.Blob)
	if err != nil {
		return ticket{}, "", invalid
	}
	return t, key, nil
}

// opsDigest is a commit's request digest (EditOptions.Sum).
func opsDigest(ops []media.Op) (string, error) {
	body, err := json.Marshal(ops)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}
