package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
)

const slotRecordExt = ".json"

// SlotRecord is originals/{slot}.json, private like the original. Commits
// and edits set Original and Edit; the image job sets Result.
type SlotRecord struct {
	Original string       `json:"original"`        // ETag of originals/{slot} when committed
	Edit     *Edit        `json:"edit,omitempty"`  // nil: the centred crop at the slot's Aspect
	Frame    *PosterFrame `json:"frame,omitempty"` // a video poster grabbed from a frame; nil for uploads
	Result   *SlotResult  `json:"result,omitempty"`
}

// SlotResult is what the served outputs were derived from.
type SlotResult struct {
	Of      string          `json:"of"`              // the Fingerprint last encoded
	Source  string          `json:"source"`          // the original (ETag) Dims measure
	Dims    Dims            `json:"dims"`            // EXIF-oriented; zero when undecodable
	Outputs []SlotRendition `json:"outputs"`         // one per rung, ascending
	Error   string          `json:"error,omitempty"` // Of failed; Outputs are older
	// An image refusal's code and details; empty for a processing fault.
	Code    string        `json:"code,omitempty"`
	Details *ErrorDetails `json:"details,omitempty"`
}

// SlotRendition is one output: the rung it is stored under and its size
// (narrower than the rung when the edited image is).
type SlotRendition struct {
	Rung int `json:"rung"`
	W    int `json:"w"`
	H    int `json:"h"`
}

// Fingerprint identifies the outputs the record yields under slot spec s.
func (rec SlotRecord) Fingerprint(s Slot) string {
	sum := sha256.Sum256([]byte(rec.Original + "|" + rec.Edit.Hash() + "|" + s.Hash()))
	return hex.EncodeToString(sum[:8])
}

// Slot returns a slot's record, or ErrNotFound before its first commit.
func (m *Manifests) Slot(ctx context.Context, ref contentref.ContentRef, slot string) (*SlotRecord, error) {
	key, err := m.slotKey(ref, slot)
	if err != nil {
		return nil, err
	}
	body, _, err := m.raw(ctx, key)
	if err != nil {
		return nil, err
	}
	var rec SlotRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return nil, fmt.Errorf("media: decode slot record %s: %w", key, err)
	}
	return &rec, nil
}

// UpdateSlot applies fn to the slot record (zero before the first commit)
// and writes it like Edit: conditionally, re-running fn on conflict, and only
// when changed.
func (m *Manifests) UpdateSlot(ctx context.Context, ref contentref.ContentRef, slot string, fn func(*SlotRecord) error) error {
	key, err := m.slotKey(ref, slot)
	if err != nil {
		return err
	}
	_, err = m.edit(ctx, key, func(body []byte) ([]byte, error) {
		var rec SlotRecord
		if body != nil {
			if err := json.Unmarshal(body, &rec); err != nil {
				return nil, fmt.Errorf("media: decode slot record %s: %w", key, err)
			}
		}
		before, _ := json.Marshal(rec)
		if err := fn(&rec); err != nil {
			return nil, err
		}
		out, err := json.Marshal(rec)
		if err != nil || bytes.Equal(out, before) {
			return nil, err
		}
		return out, nil
	})
	return err
}

func (m *Manifests) slotKey(ref contentref.ContentRef, slot string) (string, error) {
	item, err := m.kinds.Item(ref.Content())
	if err != nil {
		return "", err
	}
	return item.SlotRecord(slot)
}

// CommitSlot validates an uploaded slot or inline original and enqueues the
// re-encode of its outputs. A registered slot records it with edit (nil: the
// centred crop at the slot's Aspect); its crop's height follows its width.
// Inline images take no edit. sum is the SHA-256 the upload was presigned with.
func (u *Uploads) CommitSlot(ctx context.Context, actor access.Actor, ref contentref.ContentRef, slot string, sum []byte, edit *Edit) error {
	item, err := u.item(ref)
	if err != nil {
		return err
	}
	key, err := item.SlotOriginal(slot)
	if err != nil {
		return uploadErr(CodeNotFound, "%v", err)
	}
	if len(sum) != sha256.Size {
		return uploadErr(CodeInvalid, "the slot's SHA-256 is required")
	}
	spec, registered := item.Kind().Slots[slot]
	if !registered && edit.Normalize() != nil {
		return uploadErr(CodeInvalid, "inline images take no edit")
	}
	edit = spec.fit(edit)
	if err := edit.Check(0, 0); err != nil {
		return uploadErr(CodeInvalid, "edit: %v", err)
	}
	if _, err := u.authorize(ctx, actor, ref.Content()); err != nil {
		return err
	}
	obj, err := u.check(ctx, item, key, sum)
	if err != nil {
		return err
	}
	if u.o.Limiter != nil {
		if err := u.o.Limiter.Settle(ctx, Settlement{Tenant: ref.TenantID, Keys: []string{key}}); err != nil {
			return err
		}
	}
	if registered {
		if err := u.o.Manifests.UpdateSlot(ctx, ref, slot, func(rec *SlotRecord) error {
			rec.Original, rec.Edit, rec.Frame = obj.ETag, edit, nil
			return nil
		}); err != nil {
			return err
		}
	}
	return u.enqueueSlot(ctx, ref, slot)
}

// EditSlot re-edits the committed original (nil: centred) without a new
// upload; the job re-encodes every output from it. Once the original's size
// is known the edit is checked against it here, else by the job.
func (u *Uploads) EditSlot(ctx context.Context, actor access.Actor, ref contentref.ContentRef, slot string, edit *Edit) error {
	item, err := u.item(ref)
	if err != nil {
		return err
	}
	spec, ok := item.Kind().Slots[slot]
	if !ok {
		return uploadErr(CodeNotFound, "kind %q has no slot %q", item.Kind().Name, slot)
	}
	key, _ := item.SlotOriginal(slot)
	edit = spec.fit(edit)
	if err := edit.Check(0, 0); err != nil {
		return uploadErr(CodeInvalid, "edit: %v", err)
	}
	if _, err := u.authorize(ctx, actor, ref.Content()); err != nil {
		return err
	}
	if err := u.o.Manifests.UpdateSlot(ctx, ref, slot, func(rec *SlotRecord) error {
		if rec.Original == "" {
			return uploadErr(CodeNotFound, "slot %q has no committed original", slot)
		}
		obj, err := u.o.Store.Head(ctx, key)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err != nil || obj.ETag != rec.Original {
			return uploadErr(CodeNotUploaded, "slot %q original was replaced; commit the new one", slot)
		}
		if d := rec.dims(); d.W > 0 {
			if _, err := spec.Resolve(edit, d.W, d.H); err != nil {
				return editErr(err, "edit: %v")
			}
		}
		rec.Edit = edit
		return nil
	}); err != nil {
		return err
	}
	return u.enqueueSlot(ctx, ref, slot)
}

// dims is the committed original's size, once a result for it records one.
func (rec *SlotRecord) dims() Dims {
	if r := rec.Result; r != nil && r.Source == rec.Original {
		return r.Dims
	}
	return Dims{}
}

// SlotFromFile names a slot and the manifest image to fill it from.
type SlotFromFile struct {
	Ref  contentref.ContentRef // the slot's item; a version ref also names From's manifest
	Slot string
	// From is the manifest holding File when it is not Ref's: another item
	// (or version) of the same tenant, e.g. a post image for a channel avatar.
	From contentref.ContentRef
	File string
	// Edit crops the copy (default: the file's own edit; an empty Edit clears it).
	Edit *Edit
}

// SetSlotFromFile makes an image file the slot's original: its source is
// copied to originals/{slot} and recorded with the edit, and the slot's
// outputs are re-encoded through it. The crop's height follows its width at
// the slot's Aspect; it is checked against the file's Dims once processing
// has recorded them, else by the slot job. The actor must be allowed to
// upload to Ref's work and, when From names another item, to From.
func (u *Uploads) SetSlotFromFile(ctx context.Context, actor access.Actor, r SlotFromFile) error {
	item, err := u.item(r.Ref)
	if err != nil {
		return err
	}
	spec, ok := item.Kind().Slots[r.Slot] // inline images are write-once
	if !ok {
		return uploadErr(CodeNotFound, "kind %q has no slot %q", item.Kind().Name, r.Slot)
	}
	key, _ := item.SlotOriginal(r.Slot)
	from := r.From
	if from.ContentID == "" {
		from = r.Ref
	}
	if from.TenantID != r.Ref.TenantID {
		return uploadErr(CodeInvalid, "a slot is filled from its own tenant")
	}
	src, err := u.item(from)
	if err != nil {
		return err
	}
	if _, err := src.ManifestKey(); err != nil {
		return uploadErr(CodeInvalid, "%v", err)
	}
	if _, err := u.authorize(ctx, actor, r.Ref.Content()); err != nil {
		return err
	}
	if src.Prefix() != item.Prefix() {
		if _, err := u.authorize(ctx, actor, from); err != nil {
			return err
		}
	}
	man, _, err := u.o.Manifests.Get(ctx, from)
	if errors.Is(err, ErrNotFound) {
		return uploadErr(CodeNotFound, "no file %q", r.File)
	} else if err != nil {
		return err
	}
	i := man.File(r.File)
	if i < 0 {
		return uploadErr(CodeNotFound, "no file %q", r.File)
	}
	f := man.Files[i]
	if !strings.HasPrefix(f.Type, "image/") {
		return uploadErr(CodeInvalid, "file %q is not an image", r.File)
	}
	edit := r.Edit
	if edit == nil {
		edit = f.Edit
	}
	edit = spec.fit(edit)
	if err := edit.Check(0, 0); err != nil {
		return uploadErr(CodeInvalid, "file %q: %v", r.File, err)
	}
	if f.Dims != nil {
		if _, err := spec.Resolve(edit, f.Dims.W, f.Dims.H); err != nil {
			return editErr(err, "file %q: %v", r.File)
		}
	}
	srcKey, err := src.Original(f.Source())
	if err != nil {
		return err
	}
	rc, obj, err := u.o.Store.Get(ctx, srcKey, GetOptions{})
	if errors.Is(err, ErrNotFound) {
		return uploadErr(CodeNotUploaded, "%s has not been uploaded", f.Source())
	} else if err != nil {
		return err
	}
	defer rc.Close()
	if obj.Size > MaxSinglePut {
		return uploadErr(CodeTooLarge, "slot originals are at most %d bytes", MaxSinglePut)
	}
	if err := item.Kind().Allows(obj.ContentType, obj.Size); err != nil {
		return err
	}
	body, err := io.ReadAll(rc) // the SDK signs a seekable body over plain HTTP
	if err != nil {
		return err
	}
	opts := PutOptions{ContentType: obj.ContentType}
	if sum, ok := layout.ParseSHA256Name(f.Source()); ok {
		opts.ChecksumSHA256 = sum
	}
	put, err := u.o.Store.Put(ctx, key, bytes.NewReader(body), int64(len(body)), opts)
	if err != nil {
		return err
	}
	if err := u.o.Manifests.UpdateSlot(ctx, r.Ref, r.Slot, func(rec *SlotRecord) error {
		rec.Original, rec.Edit, rec.Frame = put.ETag, edit, nil
		return nil
	}); err != nil {
		return err
	}
	return u.enqueueSlot(ctx, r.Ref, r.Slot)
}

// SlotOriginal opens a slot's committed original for its uploaders (the
// editor); originals are never public.
func (u *Uploads) SlotOriginal(ctx context.Context, actor access.Actor, ref contentref.ContentRef, slot string) (io.ReadCloser, Object, error) {
	item, err := u.item(ref)
	if err != nil {
		return nil, Object{}, err
	}
	if _, ok := item.Kind().Slots[slot]; !ok {
		return nil, Object{}, uploadErr(CodeNotFound, "kind %q has no slot %q", item.Kind().Name, slot)
	}
	key, _ := item.SlotOriginal(slot)
	if _, err := u.authorize(ctx, actor, ref.Content()); err != nil {
		return nil, Object{}, err
	}
	rec, err := u.o.Manifests.Slot(ctx, ref, slot)
	if errors.Is(err, ErrNotFound) {
		return nil, Object{}, uploadErr(CodeNotFound, "slot %q has no committed original", slot)
	} else if err != nil {
		return nil, Object{}, err
	}
	rc, obj, err := u.o.Store.Get(ctx, key, GetOptions{})
	if err == nil && obj.ETag != rec.Original {
		rc.Close()
		err = ErrNotFound
	}
	if errors.Is(err, ErrNotFound) {
		return nil, Object{}, uploadErr(CodeNotUploaded, "slot %q original was replaced; commit the new one", slot)
	}
	return rc, obj, err
}

func (u *Uploads) enqueueSlot(ctx context.Context, ref contentref.ContentRef, slot string) error {
	if u.o.Queue == nil {
		return nil
	}
	return u.o.Queue.Enqueue(ctx, ProcessJob{Ref: ref.Content(), Slot: slot})
}

// SlotManifest describes a slot: its outputs by ascending width. Outputs are
// rewritten in place at fixed URLs, served no-cache with an ETag, so a new
// crop shows on the next revalidation.
type SlotManifest struct {
	Aspect  Aspect      `json:"aspect"`         // "W:H"; a native slot's from its outputs ("" before any)
	Edit    *Edit       `json:"edit,omitempty"` // nil: the centred crop at aspect (native: the whole image)
	Dims    *Dims       `json:"dims,omitempty"` // the committed original, EXIF-oriented, once measured
	Outputs []SlotImage `json:"outputs"`
	Pending bool        `json:"pending"`         // a commit, edit or spec change is not encoded yet
	Error   string      `json:"error,omitempty"` // the latest encode failed; the outputs are older
	// ErrorCode is an image refusal's code (image_too_small, …) with its
	// details; empty when Error is a processing fault.
	ErrorCode    string        `json:"error_code,omitempty"`
	ErrorDetails *ErrorDetails `json:"error_details,omitempty"`
	// MinWidth is the narrowest edited width the slot accepts: croppers
	// keep crops at or above it.
	MinWidth int `json:"min_width,omitempty"`
	// Animation is the slot's policy: "reject" refuses animated images.
	Animation Animation `json:"animation,omitempty"`
}

// SlotImage is one produced output.
type SlotImage struct {
	Name string `json:"name"`
	W    int    `json:"w"`
	H    int    `json:"h"`
	URL  string `json:"url"`
}

// OutputURLs builds slot output URLs for one caller.
// Outputs of gated slots (video posters) are served from
// editor/ with EditorToken to editors, from public/ to others when Exposure
// publishes them, and left out otherwise.
type OutputURLs struct {
	BaseURL     string   // the access worker origin
	EditorToken string   // the item's editor/ folder token; editors only
	Exposure    Exposure // what the item has published
}

// editorURL is an editor/ key's URL under the editor token.
func (u OutputURLs) editorURL(key string) string {
	return strings.TrimRight(u.BaseURL, "/") + "/" + key + "?t=" + u.EditorToken
}

// SlotManifest reads a slot's manifest (one object), building output URLs
// with urls. A slot never committed has no outputs, nor has a gated slot the
// caller may not see.
func (m *Manifests) SlotManifest(ctx context.Context, urls OutputURLs, ref contentref.ContentRef, slot string) (SlotManifest, error) {
	out, _, err := m.slotManifest(ctx, urls, ref, slot)
	return out, err
}

// slotManifest is SlotManifest plus the record it was built from: nil when
// the slot was never committed or the caller may not see it.
func (m *Manifests) slotManifest(ctx context.Context, urls OutputURLs, ref contentref.ContentRef, slot string) (SlotManifest, *SlotRecord, error) {
	item, s, err := m.kinds.slot(ref, slot)
	if err != nil {
		return SlotManifest{}, nil, err
	}
	out := SlotManifest{Aspect: s.Aspect, Outputs: []SlotImage{}, MinWidth: s.Min(), Animation: s.Animation}
	if item.Gated(slot) && urls.EditorToken == "" && !urls.Exposure.Poster {
		return out, nil, nil
	}
	rec, err := m.Slot(ctx, ref, slot)
	if errors.Is(err, ErrNotFound) {
		return out, nil, nil
	} else if err != nil {
		return SlotManifest{}, nil, err
	}
	out.Edit = rec.Edit
	if d := rec.dims(); d.W > 0 {
		out.Dims = &d
	}
	fp := rec.Fingerprint(s)
	res := rec.Result
	out.Pending = res == nil || res.Of != fp
	if res == nil {
		return out, rec, nil
	}
	if res.Of == fp {
		out.Error, out.ErrorCode, out.ErrorDetails = res.Error, res.Code, res.Details
	}
	for _, o := range res.Outputs {
		img := slotImage(urls.BaseURL, item, slot, o)
		if item.Gated(slot) && urls.EditorToken != "" {
			key, _ := item.SlotOutput(slot, o.Rung)
			img.URL = urls.editorURL(key)
		}
		out.Outputs = append(out.Outputs, img)
	}
	out.Aspect = outputAspect(s, out.Outputs)
	return out, rec, nil
}

// Slot resolves ref for actor and reads a slot's manifest: ErrNotVisible for
// an item actor may not see. A video poster is listed from editor/ for
// editors, and from public/ for others once the item publishes it.
func (r *Reader) Slot(ctx context.Context, ref contentref.ContentRef, actor access.Actor, slot string) (SlotManifest, error) {
	urls, err := r.outputURLs(ctx, ref, actor)
	if err != nil {
		return SlotManifest{}, err
	}
	return r.manifests.SlotManifest(ctx, urls, ref, slot)
}

// outputURLs resolves ref for actor once: editors get an editor token, others
// the item's published Exposure.
func (r *Reader) outputURLs(ctx context.Context, ref contentref.ContentRef, actor access.Actor) (OutputURLs, error) {
	item, err := r.kinds.Item(ref.Content())
	if err != nil {
		return OutputURLs{}, fmt.Errorf("%w: %v", ErrNotVisible, err)
	}
	res, err := access.ResolveOne(ctx, r.resolver, ref, actor)
	if err != nil {
		return OutputURLs{}, fmt.Errorf("%w: %w", ErrResolve, err)
	}
	if !res.Visible {
		return OutputURLs{}, ErrNotVisible
	}
	urls := OutputURLs{BaseURL: r.base.String()}
	if res.Editor {
		urls.EditorToken = r.editorToken(item, token.Expiry(r.now(), r.delivery.TTL, r.delivery.Window))
		return urls, nil
	}
	if item.Kind().Video != nil {
		if urls.Exposure, err = r.manifests.Exposure(ctx, ref); err != nil {
			return OutputURLs{}, err
		}
	}
	return urls, nil
}

// EditorURLs are the OutputURLs of an item's editors (uploaders).
func (r *Reader) EditorURLs(ref contentref.ContentRef) (OutputURLs, error) {
	item, err := r.kinds.Item(ref.Content())
	if err != nil {
		return OutputURLs{}, err
	}
	return OutputURLs{BaseURL: r.base.String(), EditorToken: r.editorToken(item, token.Expiry(r.now(), r.delivery.TTL, r.delivery.Window))}, nil
}

// ListedSlot is a slot's manifest built without reads, for listings: every
// rung at its fixed URL (the image job renders each one, capped at the edited
// width, so none is missing once the slot is set). W is the rung, an upper
// bound; H follows aspect: the slot's, or for a native slot the one the host
// recorded from Hooks.SlotEncoded (unknown: 0). Hosts list it only for slots
// they know are set, and for gated slots only when the item publishes them.
func (r *Reader) ListedSlot(ref contentref.ContentRef, slot string, aspect Aspect) (SlotManifest, error) {
	item, s, err := r.kinds.slot(ref, slot)
	if err != nil {
		return SlotManifest{}, err
	}
	if !s.Native() {
		aspect = s.Aspect
	}
	out := SlotManifest{Aspect: aspect, Outputs: []SlotImage{}, MinWidth: s.Min(), Animation: s.Animation}
	for _, w := range s.Widths {
		out.Outputs = append(out.Outputs, slotImage(r.base.String(), item, slot, SlotRendition{Rung: w, W: w, H: aspect.Height(w)}))
	}
	return out, nil
}

// outputAspect is the slot's Aspect, or a native slot's from its widest output.
func outputAspect(s Slot, outs []SlotImage) Aspect {
	if !s.Native() {
		return s.Aspect
	}
	for i := len(outs) - 1; i >= 0; i-- {
		if outs[i].W > 0 && outs[i].H > 0 {
			return AspectOf(outs[i].W, outs[i].H)
		}
	}
	return AspectNative
}

func (r *Registry) slot(ref contentref.ContentRef, slot string) (Item, Slot, error) {
	item, err := r.Item(ref.Content())
	if err != nil {
		return Item{}, Slot{}, fmt.Errorf("%w: %v", ErrNotVisible, err)
	}
	s, ok := item.Kind().Slots[slot]
	if !ok {
		return Item{}, Slot{}, fmt.Errorf("%w: kind %q has no slot %q", ErrNotVisible, item.Kind().Name, slot)
	}
	return item, s, nil
}

// slotImage is an output at its public URL, stored under its rung.
func slotImage(base string, item Item, slot string, o SlotRendition) SlotImage {
	key, _ := item.SlotPublic(slot, o.Rung)
	return SlotImage{Name: SlotOutput(slot, o.Rung), W: o.W, H: o.H, URL: strings.TrimRight(base, "/") + "/" + key}
}
