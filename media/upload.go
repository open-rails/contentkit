package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
)

// Upload size rules. Files up to MaxSinglePut are one checksum-bound PUT to
// originals/sha256-{hex}; larger ones are multipart to temp/u-{uuid} with
// parts of MinPartSize growing up to MaxPartSize (the last part may be
// smaller), until the media worker hashes and places them (Manifests.Place).
const (
	MaxSinglePut = 64 << 20
	MinPartSize  = 8 << 20
	MaxPartSize  = 16 << 20
)

// UploadAuthorizer is the host's upload permission check (AuthKit), run at
// presign and commit against the ref whose folder is written: the version
// for manifest uploads, the work (ref.Content()) for slots and inline images.
type UploadAuthorizer interface {
	CanUpload(ctx context.Context, actor access.Actor, ref contentref.ContentRef) (UploadGrant, error)
}

// UploadGrant is the host's verdict. Exempt (trusted roles) skips the
// UploadLimiter checks; Owner is the quota owner (creator, channel), "" for none.
type UploadGrant struct {
	Allowed bool
	Exempt  bool
	Owner   string
}

// ProcessJob asks for derivatives after a commit: the item's manifest, or one
// public slot when Slot is set.
type ProcessJob struct {
	Ref  contentref.ContentRef
	Slot string
}

// ProcessQueue enqueues processing in the media worker (workqueue.Queue).
type ProcessQueue interface {
	Enqueue(ctx context.Context, job ProcessJob) error
}

// ProcessCanceler is a ProcessQueue that can cancel an item's queued and
// running jobs (workqueue.Queue); a discard uses it.
type ProcessCanceler interface {
	Cancel(ctx context.Context, ref contentref.ContentRef) (int, error)
}

// UploadOptions configure Uploads.
type UploadOptions struct {
	Store      Store
	Kinds      *Registry
	Manifests  *Manifests
	Authorizer UploadAuthorizer
	Tickets    *token.Ring   // signs multipart tickets (domain-separated from access tokens); required for files over MaxSinglePut
	Limiter    UploadLimiter // optional
	Queue      ProcessQueue  // optional
	PresignTTL time.Duration // PUT and part URLs; default 15m
	// Grace and TempUploadTTL are the sweep's (JobsConfig): the Manifests'
	// Sweeps' when it is *Jobs, else 24 h and 48 h.
	Grace         time.Duration
	TempUploadTTL time.Duration
	TicketTTL     time.Duration // multipart ticket; default 24h, the abort-incomplete rule
	// Frames serves the video poster picker's frame grabs (media/video.Frames;
	// needs ffmpeg); nil answers not_found. FrameConcurrency bounds concurrent
	// grabs per process; default 2.
	Frames           FrameGrabber
	FrameConcurrency int
	// ProcessOnUpload tells the SDK to commit each manifest file as soon as
	// it is uploaded, unattached (Op.Unattached): the worker processes it
	// while the user is still arranging the upload, readers leave it out
	// until an attach op, and removing it discards its jobs and objects.
	// Quota is charged at that commit.
	ProcessOnUpload bool
}

// Uploads presigns direct-to-bucket uploads and commits them into manifests.
// It keeps no state: a multipart upload is its S3 UploadId, carried in a
// signed ticket.
type Uploads struct {
	o      UploadOptions
	frames chan struct{}
}

func NewUploads(o UploadOptions) (*Uploads, error) {
	if o.Store == nil || o.Kinds == nil || o.Manifests == nil || o.Authorizer == nil {
		return nil, errors.New("media: Uploads needs a Store, Kinds, Manifests and an Authorizer")
	}
	if o.PresignTTL <= 0 {
		o.PresignTTL = 15 * time.Minute
	}
	if o.TicketTTL <= 0 {
		o.TicketTTL = 24 * time.Hour
	}
	if jobs, ok := o.Manifests.sweeps.(*Jobs); ok {
		if o.Grace <= 0 {
			o.Grace = jobs.cfg.Grace
		}
		if o.TempUploadTTL <= 0 {
			o.TempUploadTTL = jobs.cfg.TempUploadTTL
		}
	}
	if o.Grace <= 0 {
		o.Grace = 24 * time.Hour
	}
	if o.TempUploadTTL <= 0 {
		o.TempUploadTTL = 48 * time.Hour
	}
	if o.FrameConcurrency <= 0 {
		o.FrameConcurrency = 2
	}
	return &Uploads{o: o, frames: make(chan struct{}, o.FrameConcurrency)}, nil
}

// PresignRequest declares one file. SHA256 is required up to MaxSinglePut and
// for slots; Slot targets the kind's fixed slot original, and Inline a new
// inline image, named in Presigned.Name. Both commit with CommitSlot.
type PresignRequest struct {
	Ref    contentref.ContentRef
	Type   string
	Size   int64
	SHA256 []byte
	Slot   string
	Inline bool
}

// Presigned is the upload plan: Exists (already in the folder; commit it),
// a single Put, or a Multipart upload. ProcessOnUpload is the host's
// UploadOptions.ProcessOnUpload, for manifest files.
type Presigned struct {
	Name            string
	Exists          bool
	Put             *PresignedRequest
	Multipart       *Multipart
	ProcessOnUpload bool
}

// Multipart carries the opaque ticket for PresignParts, ListParts, Complete
// and Abort, and the part-size bounds the client adapts within.
type Multipart struct {
	Ticket      string
	MinPartSize int64
	MaxPartSize int64
	MaxParts    int
}

// PartRequest is one part to presign: its exact length and SHA-256.
type PartRequest struct {
	Number int32
	Size   int64
	SHA256 []byte
}

// PresignedPart is a part URL.
type PresignedPart struct {
	Number int32
	PresignedRequest
}

func (u *Uploads) Presign(ctx context.Context, actor access.Actor, r PresignRequest) (Presigned, error) {
	p, err := u.presignFile(ctx, actor, r)
	p.ProcessOnUpload = err == nil && u.o.ProcessOnUpload && r.Slot == "" && !r.Inline
	return p, err
}

func (u *Uploads) presignFile(ctx context.Context, actor access.Actor, r PresignRequest) (Presigned, error) {
	item, err := u.item(r.Ref)
	if err != nil {
		return Presigned{}, err
	}
	if r.Type == "" || r.Size <= 0 {
		return Presigned{}, uploadErr(CodeInvalid, "type and size are required")
	}
	if err := item.Kind().Allows(r.Type, r.Size); err != nil {
		return Presigned{}, err
	}
	if r.Inline {
		if r.Slot != "" || item.Kind().Inline == nil {
			return Presigned{}, uploadErr(CodeInvalid, "kind %q takes no inline images, or a slot was also named", item.Kind().Name)
		}
		r.Slot = NewInlineName()
	} else if _, ok := item.Kind().Slots[r.Slot]; r.Slot != "" && !ok {
		return Presigned{}, uploadErr(CodeNotFound, "kind %q has no slot %q", item.Kind().Name, r.Slot)
	}
	single := r.Slot != "" || r.Size <= MaxSinglePut
	if single && len(r.SHA256) != sha256.Size {
		return Presigned{}, uploadErr(CodeInvalid, "a SHA-256 is required for uploads up to %d bytes and slots", MaxSinglePut)
	}
	if r.Slot != "" && r.Size > MaxSinglePut {
		return Presigned{}, uploadErr(CodeTooLarge, "slot originals are at most %d bytes", MaxSinglePut)
	}
	target := r.Ref
	if r.Slot != "" {
		target = r.Ref.Content() // slots and inline images live in the work's folder
	}
	grant, err := u.authorize(ctx, actor, target)
	if err != nil {
		return Presigned{}, err
	}

	// An inline image is named by its new id; its original, like a slot's,
	// is hash-named.
	shown := func(name string) string {
		if r.Inline {
			return r.Slot
		}
		return name
	}
	var name, key string
	switch {
	case single:
		name = SHA256Name(r.SHA256)
		key, _ = item.Original(name)
		// Hash-named: an identical object already in this folder needs no
		// upload, unless the sweep may soon take it; a PUT then refreshes it.
		if obj, err := u.o.Store.Head(ctx, key); err == nil && obj.Size == r.Size && obj.ContentType == r.Type {
			ok, err := u.protected(ctx, item, name, obj, u.o.Grace/2)
			if err != nil {
				return Presigned{}, err
			}
			if ok {
				return Presigned{Name: shown(name), Exists: true}, nil
			}
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return Presigned{}, err
		}
	default:
		name = NewUploadName()
		key, _ = item.Original(name)
	}

	res := Reservation{Tenant: r.Ref.TenantID, Uploader: uploaderID(actor), Key: key, Size: r.Size}
	if r.Slot == "" {
		res.Owner = grant.Owner // slot and inline originals are not charged
	}
	if u.o.Limiter != nil && !grant.Exempt {
		if err := u.o.Limiter.Reserve(ctx, res); err != nil {
			return Presigned{}, err
		}
	}
	out, err := u.presign(ctx, item, name, key, res.Uploader, r, single)
	out.Name = shown(out.Name)
	if err != nil && u.o.Limiter != nil && !grant.Exempt {
		_ = u.o.Limiter.Settle(context.WithoutCancel(ctx), Settlement{Tenant: r.Ref.TenantID, Keys: []string{key}})
	}
	return out, err
}

func (u *Uploads) presign(ctx context.Context, item Item, name, key, uploader string, r PresignRequest, single bool) (Presigned, error) {
	if single {
		p, err := u.o.Store.PresignPut(ctx, key, PresignPut{ContentType: r.Type, Size: r.Size, SHA256: r.SHA256, TTL: u.o.PresignTTL})
		if err != nil {
			return Presigned{}, err
		}
		return Presigned{Name: name, Put: &p}, nil
	}
	id, err := u.o.Store.CreateMultipart(ctx, key, r.Type)
	if err != nil {
		return Presigned{}, err
	}
	t := ticket{Ref: item.Ref(), Name: name, UploadID: id, Type: r.Type, Size: r.Size, Uploader: uploader,
		Exp: time.Now().Add(u.o.TicketTTL).Unix()}
	sealed, err := u.seal(t)
	if err != nil {
		_ = u.o.Store.AbortMultipart(context.WithoutCancel(ctx), key, id)
		return Presigned{}, err
	}
	return Presigned{Name: name, Multipart: &Multipart{Ticket: sealed, MinPartSize: MinPartSize,
		MaxPartSize: MaxPartSize, MaxParts: maxParts(r.Size)}}, nil
}

// PresignParts signs parts of a multipart upload, each bound to its length and
// SHA-256. Parts re-signed after a failure replace the earlier attempt.
func (u *Uploads) PresignParts(ctx context.Context, actor access.Actor, sealed string, parts []PartRequest) ([]PresignedPart, error) {
	t, key, err := u.open(actor, sealed)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 || len(parts) > 100 {
		return nil, uploadErr(CodeInvalid, "presign 1 to 100 parts at a time")
	}
	limit := maxParts(t.Size)
	out := make([]PresignedPart, 0, len(parts))
	for _, p := range parts {
		if p.Number < 1 || int(p.Number) > limit || p.Size <= 0 || p.Size > min(MaxPartSize, t.Size) || len(p.SHA256) != sha256.Size {
			return nil, uploadErr(CodeInvalid, "part %d: number must be 1-%d, size 1-%d bytes, with a SHA-256", p.Number, limit, min(MaxPartSize, t.Size))
		}
		req, err := u.o.Store.PresignPart(ctx, key, t.UploadID, p.Number, p.Size, p.SHA256, u.o.PresignTTL)
		if err != nil {
			return nil, err
		}
		out = append(out, PresignedPart{Number: p.Number, PresignedRequest: req})
	}
	return out, nil
}

// ListParts reports the parts that landed, for resuming.
func (u *Uploads) ListParts(ctx context.Context, actor access.Actor, sealed string) ([]Part, error) {
	t, key, err := u.open(actor, sealed)
	if err != nil {
		return nil, err
	}
	return u.listParts(ctx, key, t.UploadID)
}

func (u *Uploads) listParts(ctx context.Context, key, id string) ([]Part, error) {
	parts, err := u.o.Store.ListParts(ctx, key, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, uploadErr(CodeNotFound, "multipart upload not found (completed, aborted or expired)")
		}
		return nil, err
	}
	return parts, nil
}

// UploadedObject is a completed multipart original.
type UploadedObject struct {
	Name string
	Type string
	Size int64
}

// Complete assembles the parts server-side. Missing parts answer CodeIncomplete
// and keep the upload; parts that cannot add up to the declared size abort it.
func (u *Uploads) Complete(ctx context.Context, actor access.Actor, sealed string) (UploadedObject, error) {
	t, key, err := u.open(actor, sealed)
	if err != nil {
		return UploadedObject{}, err
	}
	parts, err := u.listParts(ctx, key, t.UploadID)
	if err != nil {
		// A retried Complete finds the assembled object instead of the upload.
		if obj, herr := u.o.Store.Head(ctx, key); herr == nil && obj.Size == t.Size {
			return UploadedObject{Name: t.Name, Type: t.Type, Size: obj.Size}, nil
		}
		return UploadedObject{}, err
	}
	var total int64
	for i, p := range parts {
		last := i == len(parts)-1
		switch {
		case p.Number != int32(i+1):
			return UploadedObject{}, uploadErr(CodeIncomplete, "part %d is missing", i+1)
		case p.Size > MaxPartSize || (!last && p.Size < MinPartSize):
			return UploadedObject{}, u.abort(ctx, t, key, uploadErr(CodeInvalid, "part %d is %d bytes; parts are %d-%d bytes, the last may be smaller", p.Number, p.Size, MinPartSize, MaxPartSize))
		case u.o.Store.Capabilities().ChecksumSHA256 && len(p.SHA256) != sha256.Size:
			return UploadedObject{}, u.abort(ctx, t, key, uploadErr(CodeChecksum, "part %d has no SHA-256", p.Number))
		}
		total += p.Size
	}
	switch {
	case total > t.Size:
		return UploadedObject{}, u.abort(ctx, t, key, uploadErr(CodeInvalid, "parts total %d bytes; declared %d", total, t.Size))
	case total < t.Size:
		return UploadedObject{}, uploadErr(CodeIncomplete, "%d of %d bytes uploaded", total, t.Size)
	}
	if _, err := u.o.Store.CompleteMultipart(ctx, key, t.UploadID, parts); err != nil {
		return UploadedObject{}, err
	}
	return UploadedObject{Name: t.Name, Type: t.Type, Size: total}, nil
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

// Commit operations. Insert and Replace take an uploaded Original.
const (
	OpInsert  = "insert"  // add Name at Index (default: append), Unattached if set; a retry with the same Original is a no-op
	OpAttach  = "attach"  // make an unattached Name part of the item, after the attached files or at Index, merging Meta; idempotent
	OpReplace = "replace" // swap Name's original; variants are regenerated, a stale hls plays until re-encoded
	OpMove    = "move"    // move Name to Index
	OpRename  = "rename"  // rename Name to To
	OpRemove  = "remove"  // drop Name
	OpEdit    = "edit"    // set Name's Edit (an image); nil clears it. Its variants are regenerated
)

// Op is one manifest edit.
type Op struct {
	Op       string         `json:"op"`
	Name     string         `json:"name"`
	Original string         `json:"original,omitempty"`
	Index    *int           `json:"index,omitempty"`
	To       string         `json:"to,omitempty"`
	Meta     map[string]any `json:"meta,omitempty"` // insert, replace: the file's meta; attach: merged into it
	Edit     *Edit          `json:"edit,omitempty"` // edit, insert, replace: the image's edit (replace drops the old one)
	// Unattached inserts the file processed but not yet part of the item
	// (UploadOptions.ProcessOnUpload); attach makes it one, remove discards it.
	Unattached bool `json:"unattached,omitempty"`
}

// Commit applies ops to the manifest in one conditional write. Every new
// original is HEAD-checked against the kind's type and size cap (and re-hashed
// when the store does not enforce checksums). The owner is charged the change
// in distinct originals the manifest references; growth past its quota fails
// with CodeQuota (not for exempt grants). Then processing is enqueued.
func (u *Uploads) Commit(ctx context.Context, actor access.Actor, ref contentref.ContentRef, ops []Op) (*Manifest, error) {
	item, err := u.item(ref)
	if err != nil {
		return nil, err
	}
	if _, err := item.Section(); err != nil {
		return nil, uploadErr(CodeInvalid, "%v", err)
	}
	if len(ops) == 0 || len(ops) > 1000 {
		return nil, uploadErr(CodeInvalid, "commit 1 to 1000 operations")
	}
	grant, err := u.authorize(ctx, actor, ref)
	if err != nil {
		return nil, err
	}
	uploaded := map[string]Object{}
	var keys, missing []string
	for _, op := range ops {
		if err := op.validate(); err != nil {
			return nil, err
		}
		if op.Original == "" || uploaded[op.Original].Key != "" {
			continue
		}
		obj, err := u.verify(ctx, item, op.Original)
		if ue, ok := AsUploadError(err); ok && ue.Code == CodeNotUploaded {
			missing = append(missing, op.Original)
			continue
		} else if err != nil {
			return nil, err
		}
		if ok, err := u.protected(ctx, item, op.Original, obj, u.retention().margin(op.Original)); err != nil {
			return nil, err
		} else if !ok {
			missing = append(missing, op.Original)
			continue
		}
		uploaded[op.Original] = obj
		keys = append(keys, obj.Key)
	}
	if len(missing) > 0 {
		return nil, &UploadError{Code: CodeNotUploaded, Originals: missing,
			Message: fmt.Sprintf("not uploaded or due for cleanup; upload again: %s", strings.Join(missing, ", "))}
	}

	// Growth is charged (and checked) inside the edit, before the manifest is
	// written; the final settlement refunds what a retried attempt no longer
	// needs and drops the reservations.
	var delta, charged int64
	var discarded *Manifest // unattached files the ops removed, and their downloads
	settle := func(ctx context.Context, s Settlement) error {
		if u.o.Limiter == nil {
			return nil
		}
		s.Tenant, s.Owner = ref.TenantID, grant.Owner
		return u.o.Limiter.Settle(ctx, s)
	}
	editCtx, cancel := context.WithTimeout(ctx, min(commitMargin(u.o.Grace), commitMargin(u.o.TempUploadTTL))/2)
	defer cancel()
	man, err := u.o.Manifests.Edit(editCtx, ref, func(m *Manifest) error {
		before := m.originalSizes()
		prev := &Manifest{Files: slices.Clone(m.Files), Downloads: maps.Clone(m.Downloads)}
		for _, op := range ops {
			if err := m.apply(op, uploaded[op.Original]); err != nil {
				return err
			}
		}
		discarded = m.dropDiscarded(prev)
		if err := item.Kind().checkFiles(prev, m); err != nil {
			return err
		}
		delta = sizeDelta(before, m.originalSizes())
		if delta > charged && grant.Owner != "" {
			if err := settle(editCtx, Settlement{Delta: delta - charged, Enforce: !grant.Exempt}); err != nil {
				return err
			}
			charged = delta
		}
		return nil
	})
	if err != nil {
		if charged > 0 {
			err = errors.Join(err, settle(context.WithoutCancel(ctx), Settlement{Delta: -charged}))
		}
		return nil, err
	}
	if err := settle(context.WithoutCancel(ctx), Settlement{Keys: keys, Delta: delta - charged}); err != nil {
		return nil, err
	}
	if len(discarded.Files) > 0 {
		if err := u.discard(ctx, item, discarded); err != nil {
			return nil, err
		}
	}
	if u.o.Queue != nil {
		if err := u.o.Queue.Enqueue(ctx, ProcessJob{Ref: ref}); err != nil {
			return nil, err
		}
	}
	return man, nil
}

// protected reports whether an existing original may be newly referenced:
// it is referenced by a manifest in the folder, or the sweep cannot take it
// within margin.
func (u *Uploads) protected(ctx context.Context, item Item, name string, obj Object, margin time.Duration) (bool, error) {
	if time.Now().Add(margin).Before(u.retention().deleteAt(name, obj.LastModified)) {
		return true, nil
	}
	return u.o.Manifests.references(ctx, item, name)
}

func (u *Uploads) retention() retention {
	return retention{grace: u.o.Grace, upload: u.o.TempUploadTTL}
}

// verify HEAD-checks an uploaded original in item's folder. A name from another
// item's folder is simply absent here.
func (u *Uploads) verify(ctx context.Context, item Item, name string) (Object, error) {
	key, err := item.Original(name)
	if err != nil {
		return Object{}, uploadErr(CodeInvalid, "%v", err)
	}
	sum, _ := layout.ParseSHA256Name(name)
	return u.check(ctx, item, key, sum)
}

// check HEADs key against the kind's rules and, for a known SHA-256, the
// stored checksum, re-hashing the bytes when the store does not enforce it.
// A mismatching object is deleted.
func (u *Uploads) check(ctx context.Context, item Item, key string, sum []byte) (Object, error) {
	obj, err := u.o.Store.Head(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return Object{}, uploadErr(CodeNotUploaded, "%s has not been uploaded", key[strings.LastIndexByte(key, '/')+1:])
	} else if err != nil {
		return Object{}, err
	}
	if err := item.Kind().Allows(obj.ContentType, obj.Size); err != nil {
		return Object{}, err
	}
	if sum == nil {
		return obj, nil
	}
	got := obj.ChecksumSHA256
	if got == nil || !u.o.Store.Capabilities().ChecksumSHA256 {
		if got, err = u.rehash(ctx, key); err != nil {
			return Object{}, err
		}
	}
	if !bytes.Equal(got, sum) {
		_ = u.o.Store.Delete(context.WithoutCancel(ctx), key)
		return Object{}, uploadErr(CodeChecksum, "stored bytes do not match their SHA-256; upload again")
	}
	return obj, nil
}

func (u *Uploads) rehash(ctx context.Context, key string) ([]byte, error) {
	rc, _, err := u.o.Store.Get(ctx, key, GetOptions{})
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

func (u *Uploads) item(ref contentref.ContentRef) (Item, error) {
	item, err := u.o.Kinds.Item(ref)
	if err != nil {
		if errors.Is(err, ErrUnknownKind) {
			return Item{}, err
		}
		return Item{}, uploadErr(CodeInvalid, "%v", err)
	}
	return item, nil
}

func (u *Uploads) authorize(ctx context.Context, actor access.Actor, ref contentref.ContentRef) (UploadGrant, error) {
	g, err := u.o.Authorizer.CanUpload(ctx, actor, ref)
	if err != nil {
		return UploadGrant{}, fmt.Errorf("media: upload permission check: %w", err)
	}
	if !g.Allowed {
		return UploadGrant{}, uploadErr(CodeForbidden, "upload not allowed")
	}
	return g, nil
}

func uploaderID(a access.Actor) string {
	if a.Anonymous || a.ID == "" {
		return "ip:" + a.IP
	}
	return a.ID
}

func maxParts(size int64) int { return int((size + MinPartSize - 1) / MinPartSize) }

func (op Op) validate() error {
	bad := func(format string, a ...any) error {
		return uploadErr(CodeInvalid, "%s %q: "+format, append([]any{op.Op, op.Name}, a...)...)
	}
	if op.Name == "" {
		return bad("a name is required")
	}
	switch op.Op {
	case OpInsert, OpReplace:
		if !layout.ValidSourceName(op.Original) {
			return bad("original must be sha256-{hex} or u-{uuid}")
		}
	case OpMove:
		if op.Index == nil {
			return bad("an index is required")
		}
	case OpRename:
		if op.To == "" {
			return bad("a new name is required")
		}
	case OpRemove, OpEdit, OpAttach:
	default:
		return uploadErr(CodeInvalid, "unknown op %q", op.Op)
	}
	if op.Edit != nil && op.Op != OpEdit && op.Op != OpInsert && op.Op != OpReplace {
		return bad("only edit, insert and replace take an edit")
	}
	if op.Unattached && op.Op != OpInsert {
		return bad("only insert takes unattached")
	}
	if err := op.Edit.Check(0, 0); err != nil {
		return bad("%v", err)
	}
	return nil
}

// setEdit validates e against the file's type and known size.
func (f *File) setEdit(e *Edit) error {
	e = e.Normalize()
	if e != nil && !strings.HasPrefix(f.Type, "image/") {
		return uploadErr(CodeInvalid, "file %q: only images take an edit", f.Name)
	}
	var w, h int
	if f.Dims != nil {
		w, h = f.Dims.W, f.Dims.H
	}
	if err := e.Check(w, h); err != nil {
		return uploadErr(CodeInvalid, "file %q: %v", f.Name, err)
	}
	f.Edit = e
	return nil
}

func (m *Manifest) apply(op Op, obj Object) error {
	i := m.File(op.Name)
	if i < 0 && op.Op != OpInsert {
		return uploadErr(CodeNotFound, "no file %q", op.Name)
	}
	switch op.Op {
	case OpInsert:
		if i >= 0 {
			if m.Files[i].Original == op.Original {
				return nil
			}
			return uploadErr(CodeConflict, "file %q exists", op.Name)
		}
		at := len(m.Files)
		if op.Index != nil {
			if *op.Index < 0 || *op.Index > len(m.Files) {
				return uploadErr(CodeInvalid, "index %d out of range 0-%d", *op.Index, len(m.Files))
			}
			at = *op.Index
		}
		f := File{Name: op.Name, Original: op.Original, Type: obj.ContentType, Size: obj.Size, Meta: op.Meta, Unattached: op.Unattached}
		if err := f.setEdit(op.Edit); err != nil {
			return err
		}
		m.Files = append(m.Files[:at], append([]File{f}, m.Files[at:]...)...)
	case OpReplace:
		f := &m.Files[i]
		if f.Original == op.Original {
			return nil
		}
		meta := f.Meta
		if op.Meta != nil {
			meta = op.Meta
		}
		// A stale hls keeps playing until the re-encode promotes its successor.
		*f = File{Name: f.Name, Original: op.Original, Type: obj.ContentType, Size: obj.Size, Meta: meta, HLS: f.HLS, Unattached: f.Unattached}
		return f.setEdit(op.Edit)
	case OpMove:
		if *op.Index < 0 || *op.Index >= len(m.Files) {
			return uploadErr(CodeInvalid, "index %d out of range 0-%d", *op.Index, len(m.Files)-1)
		}
		f := m.Files[i]
		m.Files = append(m.Files[:i], m.Files[i+1:]...)
		m.Files = append(m.Files[:*op.Index], append([]File{f}, m.Files[*op.Index:]...)...)
	case OpRename:
		if op.To != op.Name && m.File(op.To) >= 0 {
			return uploadErr(CodeConflict, "file %q exists", op.To)
		}
		m.Files[i].Name = op.To
	case OpRemove:
		m.Files = append(m.Files[:i], m.Files[i+1:]...)
	case OpEdit:
		return m.Files[i].setEdit(op.Edit)
	case OpAttach:
		f := m.Files[i]
		if !f.Unattached {
			return nil // attached already: a retry
		}
		f.Unattached = false
		for k, v := range op.Meta {
			if f.Meta == nil {
				f.Meta = map[string]any{}
			}
			f.Meta[k] = v
		}
		m.Files = append(m.Files[:i], m.Files[i+1:]...)
		// After the attached files (before the other unattached ones), or at Index.
		at := slices.IndexFunc(m.Files, func(f File) bool { return f.Unattached })
		if at < 0 {
			at = len(m.Files)
		}
		if op.Index != nil {
			if *op.Index < 0 || *op.Index > len(m.Files) {
				return uploadErr(CodeInvalid, "index %d out of range 0-%d", *op.Index, len(m.Files))
			}
			at = *op.Index
		}
		m.Files = append(m.Files[:at], append([]File{f}, m.Files[at:]...)...)
	}
	return nil
}

// dropDiscarded removes the video downloads of unattached files the edit
// from prev removed, and returns those files and downloads.
func (m *Manifest) dropDiscarded(prev *Manifest) *Manifest {
	out := &Manifest{}
	for _, f := range prev.Files {
		if f.Unattached && m.File(f.Name) < 0 {
			out.Files = append(out.Files, f)
		}
	}
	for _, f := range out.Files {
		for k, d := range m.Downloads {
			if downloadFile(k) == f.Name {
				if out.Downloads == nil {
					out.Downloads = map[string]Download{}
				}
				out.Downloads[k] = d
				delete(m.Downloads, k)
			}
		}
	}
	return out
}

// discard cancels the item's processing (the worker's jobs for every file;
// Commit re-enqueues the rest) and deletes the discarded files' staged or
// placed originals and derivatives that no manifest in the folder
// references. Unattached files were never served, so nothing waits out the
// sweep's grace; a job still finishing leaves at most orphans the sweep takes.
func (u *Uploads) discard(ctx context.Context, item Item, gone *Manifest) error {
	ctx = context.WithoutCancel(ctx)
	if c, ok := u.o.Queue.(ProcessCanceler); ok {
		if _, err := c.Cancel(ctx, item.Ref()); err != nil {
			return err
		}
	}
	refs, err := u.o.Manifests.folderRefs(ctx, item)
	if err != nil {
		return err
	}
	var errs []error
	gone.walk(func(area, name string) {
		if area == AreaOriginals {
			area = layout.SourceArea(name)
		}
		if name == "" || refs[area+"/"+name] {
			return
		}
		refs[area+"/"+name] = true // once
		if err := u.o.Store.Delete(ctx, item.Prefix()+area+"/"+name); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, err)
		}
	})
	return errors.Join(errs...)
}

// sizeDelta is the change in stored bytes from before to after.
func sizeDelta(before, after map[string]int64) int64 {
	var d int64
	for name, size := range after {
		if _, ok := before[name]; !ok {
			d += size
		}
	}
	for name, size := range before {
		if _, ok := after[name]; !ok {
			d -= size
		}
	}
	return d
}

// originalSizes maps each distinct original the manifest references to its size.
func (m *Manifest) originalSizes() map[string]int64 {
	out := make(map[string]int64, len(m.Files))
	for _, f := range m.Files {
		out[f.Original] = max(out[f.Original], f.Size)
	}
	return out
}

// OriginalBytes is the storage charged for a manifest: the sizes of its
// distinct originals. Deleting or erasing an item releases it.
func (m *Manifest) OriginalBytes() int64 {
	var n int64
	for _, s := range m.originalSizes() {
		n += s
	}
	return n
}

// ticket binds a multipart upload to its item, key, declared size and type,
// and uploader. It is signed with the token ring under an "upload|" scope that
// no object path can equal, so it never works as an access token.
type ticket struct {
	Ref      contentref.ContentRef `json:"r"`
	Name     string                `json:"n"`
	UploadID string                `json:"u"`
	Type     string                `json:"t"`
	Size     int64                 `json:"s"`
	Uploader string                `json:"a"`
	Exp      int64                 `json:"e"`
}

func (u *Uploads) seal(t ticket) (string, error) {
	if u.o.Tickets == nil {
		return "", errors.New("media: UploadOptions.Tickets is required for multipart uploads")
	}
	b, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + u.o.Tickets.Sign("upload|"+payload, time.Unix(t.Exp, 0)), nil
}

func (u *Uploads) open(actor access.Actor, sealed string) (ticket, string, error) {
	invalid := uploadErr(CodeInvalid, "invalid or expired upload ticket")
	payload, sig, ok := strings.Cut(sealed, ".")
	if !ok || u.o.Tickets == nil || u.o.Tickets.Verify(sig, "upload|"+payload, "", time.Now()) != nil {
		return ticket{}, "", invalid
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	var t ticket
	if err != nil || json.Unmarshal(b, &t) != nil {
		return ticket{}, "", invalid
	}
	if t.Uploader != uploaderID(actor) {
		return ticket{}, "", uploadErr(CodeForbidden, "upload ticket belongs to another uploader")
	}
	item, err := u.o.Kinds.Item(t.Ref)
	if err != nil {
		return ticket{}, "", invalid
	}
	key, err := item.Original(t.Name)
	if err != nil {
		return ticket{}, "", invalid
	}
	return t, key, nil
}
