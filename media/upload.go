package media

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
)

// Upload size rules. An upload lands at a staged name in temp/ (u-{uuid}):
// up to MaxSinglePut as one checksum-bound PUT, larger in parts of
// MinPartSize up to MaxPartSize (the last may be smaller). Once committed,
// the media worker hashes it and places it at private/sha256-{hex}
// (Manifests.Place), so a blob's bytes always hash to its name.
const (
	MaxSinglePut = 64 << 20
	MinPartSize  = 8 << 20
	MaxPartSize  = 16 << 20
)

// UploadOptions configure Uploads.
type UploadOptions struct {
	Store     Store
	Manifests *Manifests    // its Registry's Hooks.CanUpload authorizes, Hooks.Resolver hides new items
	Tickets   *token.Ring   // signs multipart tickets (domain-separated from access tokens); required above MaxSinglePut
	Limiter   UploadLimiter // optional
	Queue     ProcessQueue  // places staged uploads and processes items in the media worker; required
	// PresignTTL bounds PUT and part URLs; default 15m. TicketTTL bounds a
	// multipart ticket; default 24h, the bucket's abort-incomplete rule.
	PresignTTL, TicketTTL time.Duration
	// Grace is the sweep's (JobsConfig.Grace): an existing blob is reused
	// only while the sweep cannot take it first. Default 24h.
	Grace time.Duration
	// Frames serves the frame picker (media/video.Frames); nil answers
	// not_found. FrameConcurrency bounds grabs per process; default 2.
	Frames           FrameGrabber
	FrameConcurrency int
	// ProcessOnUpload tells the SDK to commit each upload unattached as
	// soon as it lands, so the worker processes it while the user arranges
	// the rest; an attach op makes it part of the item.
	ProcessOnUpload bool
}

// Uploads presigns direct-to-bucket uploads and commits them. It keeps no
// state: a multipart upload is its S3 UploadId, carried in a signed ticket.
type Uploads struct {
	o      UploadOptions
	reg    *Registry
	frames chan struct{}
}

func NewUploads(o UploadOptions) (*Uploads, error) {
	if o.Store == nil || o.Manifests == nil || o.Queue == nil {
		return nil, errors.New("media: Uploads needs a Store, Manifests and a Queue")
	}
	reg := o.Manifests.Registry()
	if reg.cfg.Hooks.CanUpload == nil {
		return nil, errors.New("media: Uploads needs Hooks.CanUpload")
	}
	if o.PresignTTL <= 0 {
		o.PresignTTL = 15 * time.Minute
	}
	if o.TicketTTL <= 0 {
		o.TicketTTL = 24 * time.Hour
	}
	if o.Grace <= 0 {
		o.Grace = 24 * time.Hour
	}
	if o.FrameConcurrency <= 0 {
		o.FrameConcurrency = 2
	}
	return &Uploads{o: o, reg: reg, frames: make(chan struct{}, o.FrameConcurrency)}, nil
}

// PresignRequest declares one upload; SHA256 is the whole file's: it finds
// an identical blob already in the folder and binds a single PUT's body.
type PresignRequest struct {
	Ref    contentref.ContentRef
	Path   string
	Type   string
	Size   int64
	SHA256 []byte
}

// Presigned is the upload plan: Exists (commit it), a single Put, or a
// Multipart upload. Path is the path to commit; Blob is the name to commit:
// the folder's blob when Exists, else the staged upload (u-{uuid}) that the
// PUT or the parts write.
type Presigned struct {
	Path            string
	Blob            string
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

// Presign checks an upload against its path's Upload and the app's
// permission, and plans it.
func (u *Uploads) Presign(ctx context.Context, actor access.Actor, r PresignRequest) (Presigned, error) {
	item, err := u.item(r.Ref)
	if err != nil {
		return Presigned{}, err
	}
	if r.Type == "" || r.Size <= 0 || len(r.SHA256) != sha256.Size {
		return Presigned{}, uploadErr(CodeInvalid, "type, size and the file's SHA-256 are required")
	}
	path, g, err := u.uploadPath(item.Kind(), r.Path, r.Type)
	if err != nil {
		return Presigned{}, err
	}
	if err := allows(item.Kind().Uploads[g], r.Type, r.Size); err != nil {
		return Presigned{}, err
	}
	stem, _ := splitExt(path)
	grant, err := u.authorize(ctx, actor, UploadTarget{Ref: r.Ref, Path: stem})
	if err != nil {
		return Presigned{}, err
	}
	out := Presigned{Path: path, ProcessOnUpload: u.o.ProcessOnUpload}
	// An identical blob already in the folder needs no upload, unless the
	// sweep may soon take it. Anything else is staged: nothing a client
	// sends is ever written under a blob name.
	blob := layout.SHA256Name(r.SHA256)
	key, _ := item.Blob(blob)
	if obj, err := u.o.Store.Head(ctx, key); err == nil && obj.Size == r.Size && obj.ContentType == r.Type {
		if ok, err := u.protected(ctx, item, blob, obj); err != nil {
			return Presigned{}, err
		} else if ok {
			out.Blob, out.Exists = blob, true
			return out, nil
		}
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return Presigned{}, err
	}
	out.Blob = NewStaged()
	key, _ = item.Staged(out.Blob)
	res := Reservation{Tenant: r.Ref.TenantID, Uploader: uploaderID(actor), Owner: grant.Owner, Key: key, Size: r.Size}
	limited := u.o.Limiter != nil && !grant.Exempt
	if limited {
		if err := u.o.Limiter.Reserve(ctx, res); err != nil {
			return Presigned{}, err
		}
	}
	if err := u.presign(ctx, item, key, res.Uploader, r, &out); err != nil {
		if limited {
			_ = u.o.Limiter.Settle(context.WithoutCancel(ctx), Settlement{Tenant: r.Ref.TenantID, Keys: []string{key}})
		}
		return Presigned{}, err
	}
	return out, nil
}

// NewStaged is a fresh staged upload name: "u-{uuid}".
func NewStaged() string { return layout.StagedPrefix + uuid.NewString() }

// uploadPath is the path to commit for a requested one: its {name} cleaned
// (or chosen, for a Named upload) and an extension from the type when it
// has none; g is its Upload.
func (u *Uploads) uploadPath(k *Kind, path, contentType string) (string, int, error) {
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
		return "", 0, uploadErr(CodeNotFound, "kind %q has no upload path %q", k.Name, path)
	}
	if k.Uploads[g].Named {
		stem = dir + NewName()
	}
	if ext == "" {
		ext = typeExt(contentType)
	}
	return stem + "." + ext, g, nil
}

func (u *Uploads) presign(ctx context.Context, item Item, key, uploader string, r PresignRequest, out *Presigned) error {
	if r.Size <= MaxSinglePut {
		p, err := u.o.Store.PresignPut(ctx, key, PresignPut{ContentType: r.Type, Size: r.Size, SHA256: r.SHA256, TTL: u.o.PresignTTL})
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
	out.Multipart = &Multipart{Ticket: sealed, MinPartSize: MinPartSize, MaxPartSize: MaxPartSize, MaxParts: maxParts(r.Size)}
	return nil
}

// PresignParts signs parts of a multipart upload, each bound to its length
// and SHA-256. Parts re-signed after a failure replace the earlier attempt.
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
	if errors.Is(err, ErrNotFound) {
		return nil, uploadErr(CodeNotFound, "multipart upload not found (completed, aborted or expired)")
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
	var total int64
	for i, p := range parts {
		last := i == len(parts)-1
		switch {
		case p.Number != int32(i+1):
			return UploadedBlob{}, uploadErr(CodeIncomplete, "part %d is missing", i+1)
		case p.Size > MaxPartSize || (!last && p.Size < MinPartSize):
			return UploadedBlob{}, u.abort(ctx, t, key, uploadErr(CodeInvalid, "part %d is %d bytes; parts are %d-%d bytes, the last may be smaller", p.Number, p.Size, MinPartSize, MaxPartSize))
		case u.o.Store.Capabilities().ChecksumSHA256 && len(p.SHA256) != sha256.Size:
			return UploadedBlob{}, u.abort(ctx, t, key, uploadErr(CodeChecksum, "part %d has no SHA-256", p.Number))
		}
		total += p.Size
	}
	switch {
	case total > t.Size:
		return UploadedBlob{}, u.abort(ctx, t, key, uploadErr(CodeInvalid, "parts total %d bytes; declared %d", total, t.Size))
	case total < t.Size:
		return UploadedBlob{}, uploadErr(CodeIncomplete, "%d of %d bytes uploaded", total, t.Size)
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

// Commit applies ops to ref's manifest in one conditional write. Every op
// is authorized against what it writes (Hooks.CanUpload). A put names a
// staged upload or a blob in the folder, HEAD-checked against its Upload;
// copies are copied server-side first. The owner is charged the change in
// upload sizes; growth past its quota fails with CodeQuota (not for exempt
// grants). Then the worker places staged uploads and processes the item. A
// new item starts hidden unless anonymous viewers may see it
// (Hooks.Resolver).
func (u *Uploads) Commit(ctx context.Context, actor access.Actor, ref contentref.ContentRef, ops []Op) (*Manifest, error) {
	item, err := u.item(ref)
	if err != nil {
		return nil, err
	}
	if len(ops) == 0 || len(ops) > 1000 {
		return nil, uploadErr(CodeInvalid, "commit 1 to 1000 operations")
	}
	for _, op := range ops {
		if err := op.validate(); err != nil {
			return nil, err
		}
	}
	grant, err := u.authorizeOps(ctx, actor, item, ops)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, op := range ops {
		if op.Op == OpPut && !slices.Contains(names, op.Blob) {
			names = append(names, op.Blob)
		}
	}
	copies, err := u.copies(ctx, actor, item, ops)
	if err != nil {
		return nil, err
	}
	hidden, err := u.newHidden(ctx, item)
	if err != nil {
		return nil, err
	}

	// Growth is charged (and checked) inside the edit, before the manifest is
	// written; the final settlement refunds what a retried attempt no longer
	// needs and drops the reservations.
	var delta, charged int64
	var keys []string
	settle := func(ctx context.Context, s Settlement) error {
		if u.o.Limiter == nil {
			return nil
		}
		s.Tenant, s.Owner = ref.TenantID, grant.Owner
		return u.o.Limiter.Settle(ctx, s)
	}
	editCtx, cancel := context.WithTimeout(ctx, commitMargin(u.o.Grace)/2)
	defer cancel()
	var prior *Manifest
	man, err := u.o.Manifests.Edit(editCtx, ref, func(m *Manifest) error {
		prior = m.Clone()
		objects, err := u.verify(editCtx, item, names)
		if err != nil {
			return err
		}
		keys = keys[:0]
		for _, o := range objects {
			keys = append(keys, o.Key)
		}
		if hidden != nil && len(m.Files) == 0 && m.Meta == nil {
			m.Hidden = *hidden
		}
		before := m.uploadBytes()
		o := &opRun{k: item.Kind(), m: m, id: ref.ContentID, objects: objects, copies: copies}
		for n, op := range ops {
			if err := o.apply(n, op); err != nil {
				return err
			}
		}
		delta = m.uploadBytes() - before
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
	if c, ok := u.o.Queue.(ProcessCanceler); ok && removesPending(prior, ops) {
		if _, err := c.Cancel(context.WithoutCancel(ctx), ref); err != nil {
			return nil, err
		}
	}
	job := ProcessJob{Ref: ref, Place: len(man.StagedNames()) > 0}
	for _, op := range ops {
		if op.Op == OpRegenerate {
			job.Preset, job.Force = op.Preset, op.Force
		}
	}
	if err := u.o.Queue.Enqueue(ctx, job); err != nil {
		return nil, err
	}
	return man, nil
}

// removesPending reports ops removing an upload that is still being
// processed: its running jobs are cancelled rather than finished.
func removesPending(m *Manifest, ops []Op) bool {
	for _, op := range ops {
		if f, ok := m.Get(op.Path); op.Op == OpRemove && ok && f.IsUpload() && len(f.Pending) > 0 {
			return true
		}
	}
	return false
}

// authorizeOps checks every op's target once; the grants must agree on the
// quota owner. A copy also needs the right to write its source.
func (u *Uploads) authorizeOps(ctx context.Context, actor access.Actor, item Item, ops []Op) (UploadGrant, error) {
	seen := map[UploadTarget]bool{}
	var grant *UploadGrant
	check := func(t UploadTarget) error {
		if seen[t] {
			return nil
		}
		seen[t] = true
		g, err := u.authorize(ctx, actor, t)
		if err != nil {
			return err
		}
		if grant == nil {
			grant = &g
		} else if t.Ref == item.Ref() && (g.Owner != grant.Owner || g.Exempt != grant.Exempt) {
			return uploadErr(CodeForbidden, "the commit's paths have different owners")
		}
		return nil
	}
	for _, op := range ops {
		t := UploadTarget{Ref: item.Ref()}
		if op.Path != "" {
			t.Path, _ = splitExt(op.Path)
		}
		if op.Op == OpCopy {
			src, err := u.reg.Ref(item.Kind().Name, op.From.ID)
			if err != nil {
				return UploadGrant{}, uploadErr(CodeInvalid, "copy from %q: %v", op.From.ID, err)
			}
			stem, _ := splitExt(op.From.Path)
			if err := check(UploadTarget{Ref: src, Path: stem}); err != nil {
				return UploadGrant{}, err
			}
			t.Path, _ = splitExt(cmpOr(op.To, op.From.Path))
		}
		if err := check(t); err != nil {
			return UploadGrant{}, err
		}
	}
	return *grant, nil
}

// newHidden is whether a new item starts hidden, resolved anonymously; nil
// when the item has a manifest or the app has no resolver.
func (u *Uploads) newHidden(ctx context.Context, item Item) (*bool, error) {
	r := u.reg.cfg.Hooks.Resolver
	if r == nil {
		return nil, nil
	}
	if _, _, err := u.o.Manifests.Get(ctx, item.Ref()); !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	res, err := access.ResolveOne(ctx, r, item.Ref(), access.Actor{Anonymous: true})
	if err != nil {
		return nil, fmt.Errorf("media: resolve %s: %w", item.Ref(), err)
	}
	hidden := !res.Visible
	return &hidden, nil
}

// verify HEAD-checks the put names in item's folder: staged uploads in
// temp/, and blobs in private/ the sweep cannot take first. A blob's bytes
// were hashed when it was placed or produced, so they are not read again; a
// name from another item's folder is simply absent here.
func (u *Uploads) verify(ctx context.Context, item Item, names []string) (map[string]Object, error) {
	out := make(map[string]Object, len(names))
	var missing []string
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
		if layout.ValidHashName(n) {
			if ok, err := u.protected(ctx, item, n, obj); err != nil {
				return nil, err
			} else if !ok {
				missing = append(missing, n)
				continue
			}
		}
		out[n] = obj
	}
	if len(missing) > 0 {
		return nil, &UploadError{Code: CodeNotUploaded, Blobs: missing,
			Message: "not uploaded or due for cleanup; upload again: " + strings.Join(missing, ", ")}
	}
	return out, nil
}

// protected reports whether an existing blob may be newly referenced: the
// manifest references it, or the sweep cannot take it within the commit's
// margin.
func (u *Uploads) protected(ctx context.Context, item Item, blob string, obj Object) (bool, error) {
	if time.Now().Add(commitMargin(u.o.Grace)).Before(obj.LastModified.Add(u.o.Grace)) {
		return true, nil
	}
	m, _, err := u.o.Manifests.Get(ctx, item.Ref())
	if errors.Is(err, ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return slices.Contains(m.Blobs(), blob), nil
}

// commitMargin bounds the time between a commit's check of a blob and its
// manifest edit landing (the edit runs under half of it): a quarter of the
// grace period, at most 1 h.
func commitMargin(grace time.Duration) time.Duration { return min(grace/4, time.Hour) }

// copies reads each copy op's source upload and outputs from the other item
// and copies their blobs into item, keyed by op index.
func (u *Uploads) copies(ctx context.Context, actor access.Actor, item Item, ops []Op) (map[int][]File, error) {
	out := map[int][]File{}
	for n, op := range ops {
		if op.Op != OpCopy {
			continue
		}
		src, err := u.reg.Ref(item.Kind().Name, op.From.ID)
		if err != nil {
			return nil, uploadErr(CodeInvalid, "copy from item %q: %v", op.From.ID, err)
		}
		if src == item.Ref() {
			continue // read the source under the destination's manifest lock
		}
		from, _ := u.reg.Item(src)
		m, _, err := u.o.Manifests.Get(ctx, src)
		if errors.Is(err, ErrNotFound) {
			return nil, uploadErr(CodeNotFound, "item %s has no media", op.From.ID)
		} else if err != nil {
			return nil, err
		}
		files := copyFiles(m, op.From.Path)
		if len(files) == 0 {
			return nil, uploadErr(CodeNotFound, "no upload %q in item %s", op.From.Path, op.From.ID)
		}
		to, _, _, _, _ := item.Kind().upload(cmpOr(op.To, op.From.Path))
		group, _, _, _, _ := item.Kind().upload(op.From.Path)
		if to != group || op.Edit != nil {
			files = files[:1]
		}
		var blobs []string
		for _, f := range files {
			if f.Blob != "" && !f.Gone {
				blobs = append(blobs, f.Blob)
			}
			if f.Track != nil && f.Track.Index != "" {
				blobs = append(blobs, f.Track.Index)
			}
		}
		for _, b := range blobs {
			srcKey, _ := from.Blob(b)
			dstKey, _ := item.Blob(b)
			if _, err := u.o.Store.Head(ctx, dstKey); err == nil {
				continue
			}
			if _, err := u.o.Store.Copy(ctx, srcKey, dstKey, CopyOptions{}); err != nil {
				return nil, fmt.Errorf("media: copy %s: %w", srcKey, err)
			}
		}
		out[n] = files
	}
	return out, nil
}

// Frame grabs a still of the video upload at path for the frame picker.
func (u *Uploads) Frame(ctx context.Context, actor access.Actor, ref contentref.ContentRef, path string, t float64, width int) ([]byte, error) {
	if u.o.Frames == nil {
		return nil, uploadErr(CodeNotFound, "frames are not served here")
	}
	if t < 0 || width < 0 || width > 3840 {
		return nil, uploadErr(CodeInvalid, "t must be at least 0 and w at most 3840")
	}
	item, err := u.item(ref)
	if err != nil {
		return nil, err
	}
	stem, _ := splitExt(path)
	if _, err := u.authorize(ctx, actor, UploadTarget{Ref: ref, Path: stem}); err != nil {
		return nil, err
	}
	m, _, err := u.o.Manifests.Get(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return nil, uploadErr(CodeNotFound, "no upload %q", path)
	} else if err != nil {
		return nil, err
	}
	f, ok := m.Get(path)
	if !ok || !f.IsUpload() || !isVideoType(f.Type) || f.Source() == "" || f.Gone {
		return nil, uploadErr(CodeNotFound, "no video upload %q", path)
	}
	if f.Blob == "" {
		return nil, uploadErr(CodeConflict, "video %q is still being placed; retry", path)
	}
	select {
	case u.frames <- struct{}{}:
		defer func() { <-u.frames }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return u.o.Frames.Frame(ctx, item, f, t, width)
}

func (u *Uploads) item(ref contentref.ContentRef) (Item, error) {
	item, err := u.reg.Item(ref)
	if err != nil {
		if errors.Is(err, ErrUnknownKind) {
			return Item{}, err
		}
		return Item{}, uploadErr(CodeInvalid, "%v", err)
	}
	return item, nil
}

func (u *Uploads) authorize(ctx context.Context, actor access.Actor, t UploadTarget) (UploadGrant, error) {
	g, err := u.reg.cfg.Hooks.CanUpload.CanUpload(ctx, actor, t)
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

// uploadBytes is the storage charged for a manifest: the size of every
// upload put in it, staged or placed (frames are the worker's).
func (m *Manifest) uploadBytes() int64 {
	var n int64
	for _, f := range m.Files {
		if f.IsUpload() && f.Frame == nil {
			n += f.Size
		}
	}
	return n
}

// UploadBytes is the storage charged for a manifest; deleting or erasing
// the item releases it.
func (m *Manifest) UploadBytes() int64 { return m.uploadBytes() }

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
	invalid := uploadErr(CodeInvalid, "invalid or expired upload ticket")
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
		return ticket{}, "", uploadErr(CodeForbidden, "upload ticket belongs to another uploader")
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
