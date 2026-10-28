package mediastore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
	"golang.org/x/sync/errgroup"
)

// UploadOptions configure Uploads.
type UploadOptions struct {
	Store     media.Store
	Manifests *Manifests  // Hooks authorize uploads; its journal supplies the transactional processing queue
	Tickets   *token.Ring // signs multipart tickets (domain-separated from access tokens); required above MaxSinglePut
	Limiter   *PGLimiter  // optional; must share the journal's pool and ContentKit schema
	// PresignTTL bounds PUT and part URLs; default 15m. TicketTTL bounds a
	// multipart ticket; default 24h, the bucket's abort-incomplete rule.
	PresignTTL, TicketTTL time.Duration
	// Grace bounds the commit deadline below the lifetime of unfinished
	// private allocations (JobsConfig.Grace). Default 24h.
	Grace time.Duration
	// ProcessOnUpload tells the SDK to commit each upload unattached as
	// soon as it lands, so the worker processes it while the user arranges
	// the rest; an attach op makes it part of the item.
	ProcessOnUpload bool
	// Commits limits each uploader's commits (default 1/s, burst 30, per
	// process; set Commits.Redis to share it); exempt grants are not
	// limited.
	Commits RateLimit
}

// Uploads presigns direct-to-bucket uploads and commits them. It keeps no
// state: a multipart upload is its S3 UploadId, carried in a signed ticket.
type Uploads struct {
	o       UploadOptions
	reg     *Registry
	commits rateLimiter
}

func NewUploads(o UploadOptions) (*Uploads, error) {
	if o.Store == nil || o.Manifests == nil {
		return nil, errors.New("media: Uploads needs a Store and Manifests")
	}
	if o.Manifests.journal.queue == nil {
		return nil, errors.New("media: Uploads needs a journal with a transactional processing queue")
	}
	if o.Limiter != nil && (o.Limiter.pool != o.Manifests.journal.pool || o.Limiter.usage != o.Manifests.journal.limiter.usage) {
		return nil, errors.New("media: upload quota must use the journal's pool and ContentKit schema")
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
	if o.Grace <= 0 {
		o.Grace = 24 * time.Hour
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
	if err := RequireConditionalPut(ctx, u.o.Store); err != nil {
		return media.Presigned{}, err
	}
	out := media.Presigned{Path: path, SourceName: r.Path, ProcessOnUpload: u.o.ProcessOnUpload}
	// An identical current allocation needs no upload. Unreferenced objects
	// are never reused. Anything else is staged: nothing a client sends is
	// ever written under a private blob name.
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
			if err := u.o.Manifests.checkAllocations(ctx, item, []string{key}); err != nil {
				return media.Presigned{}, err
			}
			out.Blob, out.Exists = blob, true
			return out, nil
		}
	}
	out.Blob = NewStaged()
	key, _ := item.Staged(out.Blob)
	res := Reservation{Tenant: r.Ref.TenantID, Uploader: uploaderID(actor), Owner: grant.Owner, Key: key, Size: r.Size}
	limited := u.o.Limiter != nil && !grant.Exempt
	if limited {
		if err := u.o.Limiter.Reserve(ctx, res); err != nil {
			return media.Presigned{}, err
		}
	}
	if err := u.o.Manifests.allocate(ctx, item, []string{key}); err != nil {
		if limited {
			_ = u.o.Limiter.Settle(context.WithoutCancel(ctx), Settlement{Tenant: r.Ref.TenantID, Keys: []string{key}})
		}
		return media.Presigned{}, err
	}
	if err := u.presign(ctx, item, key, res.Uploader, r, &out); err != nil {
		_ = u.o.Manifests.retireAllocations(context.WithoutCancel(ctx), item, []string{key})
		if limited {
			_ = u.o.Limiter.Settle(context.WithoutCancel(ctx), Settlement{Tenant: r.Ref.TenantID, Keys: []string{key}})
		}
		return media.Presigned{}, err
	}
	return out, nil
}

// NewStaged is a fresh staged upload name: "u-{uuid}".
func NewStaged() string { return layout.StagedPrefix + uuid.NewString() }

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
	item, _ := u.reg.Item(t.Ref)
	if err := u.o.Manifests.checkAllocations(ctx, item, []string{key}); err != nil {
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
	item, _ := u.reg.Item(t.Ref)
	if err := u.o.Manifests.checkAllocations(ctx, item, []string{key}); err != nil {
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

// Abort cancels a multipart upload and drops its reservation.
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
	if u.o.Limiter != nil {
		err = errors.Join(err, u.o.Limiter.Settle(ctx, Settlement{Tenant: t.Ref.TenantID, Keys: []string{key}}))
	}
	if cause != nil {
		return cause
	}
	return err
}

// Commit applies ops to ref's manifest in one conditional write. operationID
// is a caller-stable UUIDv4 or UUIDv7 for the complete ordered batch: reuse it after an
// uncertain response, but never for different inputs. Replays authorize again
// and return the current manifest without repeating the original edit. Every op
// is authorized against what it writes (Hooks.CanUpload). A put names a
// staged upload or a blob in the folder, HEAD-checked against its Upload;
// copies are copied server-side first. The owner is charged the change in
// upload sizes; growth past its quota fails with CodeQuota (not for exempt
// grants). Then the worker places staged uploads and processes the item.
// Declared public presets are processed independently of visibility.
// Successful removes finish public cleanup before returning;
// unreferenced private blobs go when a grace period old, or at once for a
// takedown (Op.Takedown).
// A successful commit records processing and notification effects durably
// with the manifest mutation.
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
	// Deterministic internal IDs share this journal. Untrusted callers must
	// not reserve a lifecycle receipt belonging to another media item.
	if !trusted && (id.Variant() != uuid.RFC4122 || id.Version() != 4 && id.Version() != 7) {
		return nil, uploadErr(media.CodeInvalid, "operation_id must be a UUIDv4 or UUIDv7")
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
	if err := RequireConditionalPut(ctx, u.o.Store); err != nil {
		return nil, err
	}
	if !grant.Exempt {
		if slices.ContainsFunc(ops, func(op media.Op) bool { return op.Op == media.OpRegenerate && op.Force }) {
			return nil, uploadErr(media.CodeForbidden, "forced regeneration needs an exempt grant")
		}
		if u.commits != nil {
			if ok, wait := u.commits.allow(ctx, uploaderID(actor)); !ok {
				return nil, &media.UploadError{Code: media.CodeRate, Message: "commit rate limit", RetryAfter: wait}
			}
		}
	}
	// The journal owns the growth claim before S3 PUT and settles it only
	// after a definite success or a recovery fence. An uncertain response
	// must not refund quota or consume the upload's reservations.
	input, err := json.Marshal(ops)
	if err != nil {
		return nil, uploadErr(media.CodeInvalid, "commit operations must be JSON")
	}
	mutation := &manifestMutation{commit: manifestCommit{ID: id, Ref: ref, Folder: item.Prefix(),
		Actor: string(actor.Kind) + ":" + uploaderID(actor), Fingerprint: sha256.Sum256(input)}}
	if man, replayed, err := u.o.Manifests.replayCommit(ctx, item, mutation.commit); err != nil || replayed {
		if err != nil {
			return nil, err
		}
		if err := u.o.Manifests.notifyCommit(ctx, mutation.commit); err != nil {
			return nil, err
		}
		return man, nil
	}
	frames, err := u.frameVideos(ctx, item, ops)
	if err != nil {
		return nil, err
	}
	copies, copied, err := u.copies(ctx, actor, grant, item, ops)
	if err != nil {
		return nil, err
	}
	var keys []string
	editCtx, cancel := context.WithTimeout(ctx, commitMargin(u.o.Grace)/2)
	defer cancel()
	var prior *Manifest
	man, err := u.o.Manifests.editOperation(editCtx, ref, false, bound{project: item.Kind().Unwritten}, mutation, func(m *Manifest) error {
		prior = m.Clone()
		o := &opRun{k: item.Kind(), m: m, ref: ref, copies: copies, frames: frames, exempt: grant.Exempt, trusted: trusted}
		o.renames = map[string]string{}
		for _, file := range prior.Files {
			if file.IsUpload() {
				o.renames[file.Path] = file.Path
			}
		}
		o.archiveAllowed = func(source, destination string) error {
			stem, _ := splitExt(source)
			delegated, err := u.authorize(editCtx, actor, media.UploadTarget{Ref: ref, Path: stem})
			if err != nil {
				return err
			}
			if delegated.ArchiveTo != destination || delegated.Owner != grant.Owner || delegated.Exempt != grant.Exempt {
				return uploadErr(media.CodeForbidden, "ZIP expansion requires an explicitly approved destination with the same owner and quota policy")
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
			admission := &media.SourceAdmission{Generation: id.String(), Owner: grant.Owner, Enforce: u.o.Limiter != nil && !grant.Exempt, Target: destination, Accounting: u.o.Limiter != nil}
			if admission.Enforce {
				quota, err := u.o.Limiter.Quota(editCtx, ref.TenantID, grant.Owner)
				if err != nil {
					return nil, err
				}
				admission.Quota = quota
			}
			admissions[key] = admission
			return admission, nil
		}
		o.authorizePath = func(path string) error {
			stem, _ := splitExt(path)
			allocated, err := u.authorize(editCtx, actor, media.UploadTarget{Ref: ref, Path: stem})
			if err != nil {
				return err
			}
			if allocated.Owner != grant.Owner || allocated.Exempt != grant.Exempt {
				return uploadErr(media.CodeForbidden, "allocated page has a different upload owner")
			}
			return nil
		}
		keys = keys[:0]
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
			key, err := item.Staged(op.Blob)
			if err != nil {
				key, _ = item.Blob(op.Blob)
			}
			if !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
		objects, err := u.verify(editCtx, item, m, names)
		if err != nil {
			return err
		}
		if err := u.copied(editCtx, item, copies); err != nil {
			return err
		}
		o.objects = objects
		before, uploads := m.uploadBytes(), m.uploads()
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
					objects, err := u.verify(editCtx, item, m, []string{op.Blob})
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
		mutation.effects.Renames = map[string]string{}
		for before, after := range o.renames {
			if before != after {
				mutation.effects.Renames[before] = after
			}
		}
		mutation.effects.RemovedPaths = o.removed
		if n := m.uploads(); n > media.MaxUploads && n > uploads {
			return tooManyFiles(media.MaxUploads, "an item holds at most %d uploads", media.MaxUploads)
		}
		mutation.effects.Cancel = removesPending(prior, ops)
		job := ProcessJob{Ref: ref, Place: len(m.StagedNames()) > 0 || item.Kind().pendingArchives(m)}
		for _, op := range ops {
			if op.Op == media.OpRegenerate {
				job.Preset, job.Force = op.Preset, op.Force
			}
		}
		mutation.effects.Process = &job
		if u.o.Limiter != nil {
			mutation.effects.Settlement = Settlement{Tenant: ref.TenantID, Owner: grant.Owner,
				Keys: append(slices.Clone(keys), copied...), Delta: m.uploadBytes() - before, Enforce: !grant.Exempt}
			if mutation.effects.Settlement.Delta > 0 && !grant.Exempt {
				// Resolve host policy before the journal opens its transaction.
				// Shrinks and ordering edits must not depend on quota availability.
				mutation.quota, err = u.o.Limiter.Quota(editCtx, ref.TenantID, grant.Owner)
				if err != nil {
					return err
				}
			}
		}

		if slices.ContainsFunc(ops, func(op media.Op) bool { return op.Takedown }) {
			drop := Unreferenced{All: grant.Exempt, Staged: missing(prior.StagedNames(), m.StagedNames())}
			if !drop.All {
				drop.Blobs = missing(prior.Blobs(), m.Blobs())
				now := u.reg.editorViews(m)
				for view := range u.reg.editorViews(prior) {
					if !now[view] {
						drop.Blobs = append(drop.Blobs, view)
					}
				}
			}
			mutation.effects.Private, err = u.o.Manifests.unreferenced(editCtx, item, m, drop)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, ErrManifestTooLarge) {
		err = uploadErr(media.CodeTooLarge, "the item, processed, would pass its manifest's %d MiB: remove uploads or meta first", media.MaxManifestBytes>>20)
	}
	if err != nil {
		return nil, err
	}
	notifyCtx, notifyCancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer notifyCancel()
	if err := u.o.Manifests.notifyCommit(notifyCtx, mutation.commit); err != nil {
		return nil, err
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
			} else if g.Owner != grant.Owner || g.Exempt != grant.Exempt {
				return media.UploadGrant{}, uploadErr(media.CodeForbidden, "the commit's paths have different owners")
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
// temp/, and blobs in private/ the sweep cannot take first. A blob's bytes
// were hashed when it was placed or produced, so they are not read again; a
// name from another item's folder is simply absent here.
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
		if err := u.o.Manifests.checkAllocations(ctx, item, []string{key}); errors.Is(err, ErrAllocationRetired) {
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

// reusable accepts only current allocations, excluding editor-only views.
// An unreferenced allocation must never be adopted after cleanup selected it.
func (u *Uploads) reusable(m *Manifest, views map[string]bool, blob string) bool {
	if views[blob] {
		return false
	}
	return slices.Contains(m.Blobs(), blob)
}

// current is item's manifest, empty when it has none.
func (u *Uploads) current(ctx context.Context, item Item) (*Manifest, error) {
	m, _, err := u.o.Manifests.Get(ctx, item.Ref())
	if errors.Is(err, ErrNotFound) {
		return &Manifest{}, nil
	}
	return m, err
}

// commitMargin bounds the time between a commit's check of a blob and its
// manifest edit landing (the edit runs under half of it): a quarter of the
// grace period, at most 1 h.
func commitMargin(grace time.Duration) time.Duration { return min(grace/4, time.Hour) }

// copies reads each copy op's source files from the other item and copies
// their blobs into fresh destination allocations, all allocated in one step,
// keyed by op index. Each copy op that moves bytes is reserved once, like an
// upload of its source (rate limits and pending quota; the commit charges
// it), so a failed commit leaves no unmetered bytes; it returns the reserved
// keys.
func (u *Uploads) copies(ctx context.Context, actor access.Actor, grant media.UploadGrant, item Item, ops []media.Op) (map[int][]media.File, []string, error) {
	type copyKey struct {
		op        int
		key, area string
	}
	type source struct {
		n     int
		from  Item
		files []media.File
		todo  []string // blobs this op copies first
		first int      // the index of todo[0] in the allocations
	}
	var (
		sources  []source
		sums     [][]byte
		srcKeys  []string
		areas    []string
		manifest = map[contentref.ContentKey]*Manifest{}
		names    = map[copyKey]string{} // one allocation per copy, source and area
	)
	for n, op := range ops {
		if op.Op != media.OpCopy {
			continue
		}
		src, err := u.reg.VersionRef(item.Kind().Name, op.From.ID, op.From.Version)
		if err != nil {
			return nil, nil, uploadErr(media.CodeInvalid, "copy from %q: not a media item of this kind", op.From.ID)
		}
		if src.Equal(item.Ref()) {
			continue // read the source under the destination's manifest lock
		}
		from, _ := u.reg.Item(src)
		m, ok := manifest[src.Key()]
		if !ok {
			if m, _, err = u.o.Manifests.Get(ctx, src); errors.Is(err, ErrNotFound) {
				return nil, nil, uploadErr(media.CodeNotFound, "item %s has no media", op.From.ID)
			} else if err != nil {
				return nil, nil, err
			}
			manifest[src.Key()] = m
		}
		files := copyFiles(m, op.From.Path)
		if len(files) == 0 {
			return nil, nil, uploadErr(media.CodeNotFound, "no upload %q in item %s", op.From.Path, op.From.ID)
		}
		to, _, _, _, _ := item.Kind().upload(cmpOr(op.To, op.From.Path))
		group, _, _, _, _ := item.Kind().upload(op.From.Path)
		if to != group || op.Edit != nil {
			files = files[:1]
		}
		sc := source{n: n, from: from, files: files, first: len(sums)}
		for _, b := range copyBlobs(item.Kind(), files) {
			srcKey, _ := from.Blob(b[0])
			if _, seen := names[copyKey{n, srcKey, b[1]}]; seen {
				continue
			}
			sum, ok := layout.BlobDigest(b[0])
			if !ok {
				return nil, nil, fmt.Errorf("media: invalid copy source %q", b[0])
			}
			names[copyKey{n, srcKey, b[1]}] = ""
			sc.todo, sums, srcKeys, areas = append(sc.todo, b[0]), append(sums, sum), append(srcKeys, srcKey), append(areas, b[1])
		}
		sources = append(sources, sc)
	}
	// Each copy lands in its destination file's area.
	allocated := make([]string, len(sums))
	for _, area := range []string{layout.AreaPrivate, layout.AreaOriginals, layout.AreaInternal} {
		var at []int
		var of [][]byte
		for i, a := range areas {
			if a == area {
				at, of = append(at, i), append(of, sums[i])
			}
		}
		if len(at) == 0 {
			continue
		}
		refs, err := u.o.Manifests.NewBlobs(ctx, item.Ref(), area, of)
		if err != nil {
			return nil, nil, err
		}
		for j, i := range at {
			allocated[i] = refs[j]
		}
	}
	for _, sc := range sources {
		for j := range sc.todo {
			i := sc.first + j
			names[copyKey{sc.n, srcKeys[i], areas[i]}] = allocated[i]
		}
	}
	var reserved []string
	if u.o.Limiter != nil && !grant.Exempt {
		// One reservation per copy, for the upload's bytes: what the commit
		// charges (outputs are not charged).
		for _, sc := range sources {
			if len(sc.todo) == 0 {
				continue
			}
			key, _ := item.Blob(allocated[sc.first])
			if err := u.o.Limiter.Reserve(ctx, Reservation{Tenant: item.Ref().TenantID, Uploader: uploaderID(actor),
				Owner: grant.Owner, Key: key, Size: sc.files[0].Size}); err != nil {
				return nil, reserved, err

			}
			reserved = append(reserved, key)
		}
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for i, srcKey := range srcKeys {
		g.Go(func() error {
			dstKey, _ := item.Blob(allocated[i])
			if _, err := u.o.Store.Copy(gctx, srcKey, dstKey, media.CopyOptions{}); err != nil {
				return fmt.Errorf("media: copy %s: %w", srcKey, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, reserved, err
	}
	out := make(map[int][]media.File, len(sources))
	for _, sc := range sources {
		files := sc.files
		for i := range files {
			files[i].Editor = nil // editor views belong to their original item's allocation
			if !files[i].Gone && files[i].Blob != "" {
				from, _ := sc.from.Blob(files[i].Blob)
				files[i].Blob = names[copyKey{sc.n, from, copyArea(item.Kind(), i, files[i])}]
			}
			if files[i].Track != nil && files[i].Track.Index != "" {
				track := *files[i].Track
				from, _ := sc.from.Blob(track.Index)
				track.Index = names[copyKey{sc.n, from, copyIndexArea(item.Kind())}]
				files[i].Track = &track
			}
		}
		out[sc.n] = files
	}
	return out, reserved, nil
}

// fileBlobs are the blobs files reference.
// copyBlobs are the blobs of a copy's files, each with the area it lands in
// at the destination: the upload's, an output's by its preset; track indexes
// in the destination's prepared-output area.
func copyBlobs(k *RegisteredKind, files []media.File) [][2]string {
	var out [][2]string
	for i, f := range files {
		if f.Blob != "" && !f.Gone {
			out = append(out, [2]string{f.Blob, copyArea(k, i, f)})
		}
		if f.Track != nil && f.Track.Index != "" {
			out = append(out, [2]string{f.Track.Index, copyIndexArea(k)})
		}
	}
	return out
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

func fileBlobs(files []media.File) []string {
	var blobs []string
	for _, f := range files {
		if f.Blob != "" && !f.Gone {
			blobs = append(blobs, f.Blob)
		}
		if f.Track != nil && f.Track.Index != "" {
			blobs = append(blobs, f.Track.Index)
		}
	}
	return blobs
}

// copied checks, under the manifest lock, that the blobs copies put in
// item's folder are still there: a takedown since may have taken them.
func (u *Uploads) copied(ctx context.Context, item Item, copies map[int][]media.File) error {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	seen := map[string]bool{}
	for _, files := range copies {
		for _, b := range fileBlobs(files) {
			if seen[b] {
				continue
			}
			seen[b] = true
			g.Go(func() error {
				key, _ := item.Blob(b)
				if _, err := u.o.Store.Head(ctx, key); errors.Is(err, ErrNotFound) {
					return uploadErr(media.CodeConflict, "the item changed during the copy: try again")
				} else if err != nil {
					return err
				}
				return nil
			})
		}
	}
	return g.Wait()
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
		return "ip:" + a.IP
	}
	return a.ID
}

func maxParts(size int64) int { return int((size + media.MinPartSize - 1) / media.MinPartSize) }

// uploadFloor is the least an upload is charged: its share of the manifest
// and its outputs, so tiny files cannot grow an item for free.
const uploadFloor = 64 << 10

// uploadBytes is the storage charged for a manifest: the size of every
// upload put in it, staged or placed, at least uploadFloor (frames are the
// worker's).
func (m *Manifest) uploadBytes() int64 {
	var n int64
	for _, f := range m.Files {
		if f.IsUpload() && f.Frame == nil {
			n += max(f.Size, uploadFloor)
		}
	}
	return n
}

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
