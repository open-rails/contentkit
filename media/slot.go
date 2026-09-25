package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
	"github.com/open-rails/contentkit/media/token"
)

// SlotRecord is a registered slot's or inline image's entry in Root.Slots.
// Commits and edits set Original (with its upload name, type and size) and
// Edit; the image job sets Result.
type SlotRecord struct {
	Original string       `json:"original"` // originals/ name
	Filename string       `json:"filename,omitempty"`
	Type     string       `json:"type,omitempty"`
	Size     int64        `json:"size,omitempty"`
	Edit     *Edit        `json:"edit,omitempty"`  // nil: the centred crop at the slot's Aspect
	Frame    *PosterFrame `json:"frame,omitempty"` // a video poster grabbed from a frame; nil for uploads
	Result   *SlotResult  `json:"result,omitempty"`
}

// SlotResult is what the current renditions were derived from.
type SlotResult struct {
	Of      string          `json:"of"`              // the Fingerprint last encoded
	Source  string          `json:"source"`          // the original Dims measure
	Dims    Dims            `json:"dims"`            // EXIF-oriented; zero when undecodable
	Outputs []SlotRendition `json:"outputs"`         // one per rung, ascending
	Error   string          `json:"error,omitempty"` // Of failed; Outputs are older
	// An image refusal's code and details; empty for a processing fault.
	Code    string        `json:"code,omitempty"`
	Details *ErrorDetails `json:"details,omitempty"`
}

// SlotRendition is one output: private/{Blob}, the rung it renders and its
// size (narrower than the rung when the edited image is).
type SlotRendition struct {
	Rung int    `json:"rung"`
	W    int    `json:"w"`
	H    int    `json:"h"`
	Blob string `json:"blob"`
	Size int64  `json:"size,omitempty"`
}

// Fingerprint identifies the outputs the record yields under slot spec s.
func (rec SlotRecord) Fingerprint(s Slot) string {
	sum := sha256.Sum256([]byte(rec.Original + "|" + rec.Edit.Hash() + "|" + s.Hash()))
	return hex.EncodeToString(sum[:8])
}

// Slot returns a slot's record, or ErrNotFound before its first commit.
func (m *Manifests) Slot(ctx context.Context, ref contentref.ContentRef, slot string) (*SlotRecord, error) {
	root, _, err := m.Root(ctx, ref)
	if err != nil {
		return nil, err
	}
	rec, ok := root.Slots[slot]
	if !ok {
		return nil, ErrNotFound
	}
	return rec, nil
}

// UpdateSlot applies fn to the slot record (zero before the first commit)
// in one EditRoot; a record left zero is not added.
func (m *Manifests) UpdateSlot(ctx context.Context, ref contentref.ContentRef, slot string, fn func(*SlotRecord) error) error {
	_, err := m.EditRoot(ctx, ref, func(r *Root) error {
		rec := r.Slots[slot]
		if rec == nil {
			rec = &SlotRecord{}
		}
		if err := fn(rec); err != nil {
			return err
		}
		if rec.Original == "" && rec.Result == nil && rec.Frame == nil {
			return nil
		}
		if r.Slots == nil {
			r.Slots = map[string]*SlotRecord{}
		}
		r.Slots[slot] = rec
		return nil
	})
	return err
}

// SlotCommit names an uploaded slot or inline original: SHA256 is the one
// its upload was presigned with, Filename the uploaded file's name.
type SlotCommit struct {
	Ref      contentref.ContentRef
	Slot     string
	SHA256   []byte
	Edit     *Edit // registered slots; nil: centred
	Filename string
}

// CommitSlot validates an uploaded slot or inline original and enqueues the
// encode of its renditions. A registered slot records it with its edit (nil:
// the centred crop at the slot's Aspect); its crop's height follows its
// width. Inline images take no edit.
func (u *Uploads) CommitSlot(ctx context.Context, actor access.Actor, c SlotCommit) error {
	item, err := u.item(c.Ref)
	if err != nil {
		return err
	}
	spec, registered := item.Kind().Slots[c.Slot]
	if !registered && !item.Inline(c.Slot) {
		return uploadErr(CodeNotFound, "kind %q has no slot %q", item.Kind().Name, c.Slot)
	}
	if len(c.SHA256) != sha256.Size {
		return uploadErr(CodeInvalid, "the slot's SHA-256 is required")
	}
	if !registered && c.Edit.Normalize() != nil {
		return uploadErr(CodeInvalid, "inline images take no edit")
	}
	edit := c.Edit
	if registered {
		edit = spec.fit(edit)
	}
	if err := edit.Check(0, 0); err != nil {
		return uploadErr(CodeInvalid, "edit: %v", err)
	}
	if _, err := u.authorize(ctx, actor, c.Ref.Content()); err != nil {
		return err
	}
	name := SHA256Name(c.SHA256)
	key, _ := item.Original(name)
	obj, err := u.check(ctx, item, key, c.SHA256)
	if err != nil {
		return err
	}
	if u.o.Limiter != nil {
		if err := u.o.Limiter.Settle(ctx, Settlement{Tenant: c.Ref.TenantID, Keys: []string{key}}); err != nil {
			return err
		}
	}
	if err := u.o.Manifests.UpdateSlot(ctx, c.Ref.Content(), c.Slot, func(rec *SlotRecord) error {
		if !registered && rec.Original != "" && rec.Original != name {
			return uploadErr(CodeInvalid, "inline image %q is already set", c.Slot)
		}
		*rec = SlotRecord{Original: name, Filename: c.Filename, Type: obj.ContentType, Size: obj.Size, Edit: edit, Result: rec.Result}
		return nil
	}); err != nil {
		return err
	}
	return u.enqueueSlot(ctx, c.Ref, c.Slot)
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

// SetSlotFromFile makes an image file's source the slot's original (copied
// into the slot's folder when From is another item) with the edit, and
// re-encodes the slot's renditions through it. The crop's height follows its
// width at the slot's Aspect; it is checked against the file's Dims once
// processing has recorded them, else by the slot job. The actor must be
// allowed to upload to Ref's work and, when From names another item, to From.
func (u *Uploads) SetSlotFromFile(ctx context.Context, actor access.Actor, r SlotFromFile) error {
	item, err := u.item(r.Ref)
	if err != nil {
		return err
	}
	spec, ok := item.Kind().Slots[r.Slot] // inline images are write-once
	if !ok {
		return uploadErr(CodeNotFound, "kind %q has no slot %q", item.Kind().Name, r.Slot)
	}
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
	if _, err := src.Section(); err != nil {
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
	if !layout.ValidHashName(f.Source()) {
		return uploadErr(CodeNotUploaded, "file %q is still being processed", r.File)
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
	srcKey, _ := src.Original(f.Source())
	obj, err := u.o.Store.Head(ctx, srcKey)
	if errors.Is(err, ErrNotFound) {
		return uploadErr(CodeNotUploaded, "%s has not been uploaded", f.Source())
	} else if err != nil {
		return err
	}
	if obj.Size > MaxSinglePut {
		return uploadErr(CodeTooLarge, "slot originals are at most %d bytes", MaxSinglePut)
	}
	if err := item.Kind().Allows(obj.ContentType, obj.Size); err != nil {
		return err
	}
	if dst, _ := item.Original(f.Source()); dst != srcKey {
		if _, err := u.o.Store.Copy(ctx, srcKey, dst, CopyOptions{IfMatch: obj.ETag}); err != nil {
			return err
		}
	}
	if err := u.o.Manifests.UpdateSlot(ctx, r.Ref.Content(), r.Slot, func(rec *SlotRecord) error {
		*rec = SlotRecord{Original: f.Source(), Filename: f.Name, Type: obj.ContentType, Size: obj.Size, Edit: edit, Result: rec.Result}
		return nil
	}); err != nil {
		return err
	}
	return u.enqueueSlot(ctx, r.Ref, r.Slot)
}

func (u *Uploads) enqueueSlot(ctx context.Context, ref contentref.ContentRef, slot string) error {
	if u.o.Queue == nil {
		return nil
	}
	return u.o.Queue.Enqueue(ctx, ProcessJob{Ref: ref.Content(), Slot: slot})
}

// SlotManifest describes a slot: its outputs by ascending width. Every
// output URL names an immutable file; a change lists new URLs.
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
	// EditorURL is the committed original's editor view (Kind.Editor), what
	// the cropper draws on; editors only, "" while it renders.
	EditorURL string `json:"editor_url,omitempty"`

	editorMissing bool // the editor view is not there: render it (Reader.renderMissing)
}

// SlotImage is one produced output.
type SlotImage struct {
	W   int    `json:"w"`
	H   int    `json:"h"`
	URL string `json:"url"`
}

// OutputURLs builds slot output URLs for one caller: public/ copies for an
// item that is not hidden; private/ files under Token (editors) otherwise,
// and editor views under Editor.
type OutputURLs struct {
	BaseURL string // the access worker origin
	Token   string // the item's private/ folder token; editors only
	Editor  string // the item's temp/ editor token; editors only
}

// url is the output's URL for this caller, or "" when it may not see it.
func (u OutputURLs) url(item Item, blob string, public bool) string {
	base := strings.TrimRight(u.BaseURL, "/") + "/"
	switch {
	case public:
		key, _ := item.Public(blob)
		return base + key
	case u.Token != "":
		key, _ := item.Private(blob)
		return base + key + "?t=" + u.Token
	}
	return ""
}

// SlotManifest reads a slot (or inline image) from the item's manifest,
// building output URLs with urls. A slot never committed has no outputs.
func (m *Manifests) SlotManifest(ctx context.Context, urls OutputURLs, ref contentref.ContentRef, slot string) (SlotManifest, error) {
	out, _, err := m.slotManifest(ctx, urls, ref, slot)
	return out, err
}

// slotManifest is SlotManifest plus the record it was built from: nil when
// the slot was never committed.
func (m *Manifests) slotManifest(ctx context.Context, urls OutputURLs, ref contentref.ContentRef, slot string) (SlotManifest, *SlotRecord, error) {
	item, s, err := m.kinds.slot(ref, slot)
	if err != nil {
		return SlotManifest{}, nil, err
	}
	out := SlotManifest{Aspect: s.Aspect, Outputs: []SlotImage{}, MinWidth: s.Min(), Animation: s.Animation}
	root, _, err := m.Root(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return out, nil, nil
	} else if err != nil {
		return SlotManifest{}, nil, err
	}
	rec := root.Slots[slot]
	if rec == nil {
		return out, nil, nil
	}
	out.Edit = rec.Edit
	if d := rec.dims(); d.W > 0 {
		out.Dims = &d
	}
	fp := m.kinds.fingerprint(item, slot, *rec)
	res := rec.Result
	out.Pending = res == nil || res.Of != fp
	if res == nil {
		return out, rec, nil
	}
	if res.Of == fp {
		out.Error, out.ErrorCode, out.ErrorDetails = res.Error, res.Code, res.Details
	}
	for _, o := range res.Outputs {
		if u := urls.url(item, o.Blob, root.Private[o.Blob].Public); u != "" {
			out.Outputs = append(out.Outputs, SlotImage{W: o.W, H: o.H, URL: u})
		}
	}
	out.Aspect = outputAspect(s, out.Outputs)
	if err := m.slotEditorView(ctx, urls, item, slot, rec, &out); err != nil {
		return SlotManifest{}, nil, err
	}
	return out, rec, nil
}

// slotEditorView sets an editor's EditorURL for a registered slot's measured
// original, or editorMissing when it is not rendered.
func (m *Manifests) slotEditorView(ctx context.Context, urls OutputURLs, item Item, slot string, rec *SlotRecord, out *SlotManifest) error {
	key := item.EditorView(rec.Original)
	if _, registered := item.Kind().Slots[slot]; urls.Editor == "" || key == "" || !registered || rec.dims().W == 0 {
		return nil
	}
	if _, err := m.store.Head(ctx, key); errors.Is(err, ErrNotFound) {
		out.editorMissing = true
		return nil
	} else if err != nil {
		return err
	}
	out.EditorURL = strings.TrimRight(urls.BaseURL, "/") + "/" + key + "?t=" + urls.Editor
	return nil
}

// Slot resolves ref for actor and reads a slot: ErrNotVisible for an item
// actor may not see. Editors also see a hidden item's outputs.
func (r *Reader) Slot(ctx context.Context, ref contentref.ContentRef, actor access.Actor, slot string) (SlotManifest, error) {
	urls, err := r.outputURLs(ctx, ref, actor)
	if err != nil {
		return SlotManifest{}, err
	}
	out, err := r.manifests.SlotManifest(ctx, urls, ref, slot)
	r.renderMissing(ctx, ProcessJob{Ref: ref.Content(), Slot: slot}, out.editorMissing)
	return out, err
}

// outputURLs resolves ref for actor once: editors get private/ and temp/ tokens.
func (r *Reader) outputURLs(ctx context.Context, ref contentref.ContentRef, actor access.Actor) (OutputURLs, error) {
	if _, err := r.kinds.Item(ref.Content()); err != nil {
		return OutputURLs{}, fmt.Errorf("%w: %v", ErrNotVisible, err)
	}
	res, err := access.ResolveOne(ctx, r.resolver, ref, actor)
	if err != nil {
		return OutputURLs{}, fmt.Errorf("%w: %w", ErrResolve, err)
	}
	if !res.Visible {
		return OutputURLs{}, ErrNotVisible
	}
	if res.Editor {
		return r.EditorURLs(ref)
	}
	return OutputURLs{BaseURL: r.base.String()}, nil
}

// EditorURLs are the OutputURLs of an item's editors (uploaders).
func (r *Reader) EditorURLs(ref contentref.ContentRef) (OutputURLs, error) {
	item, err := r.kinds.Item(ref.Content())
	if err != nil {
		return OutputURLs{}, err
	}
	exp := token.Expiry(r.now(), r.delivery.TTL, r.delivery.Window)
	return OutputURLs{BaseURL: r.base.String(), Token: r.ring.Sign(item.PrivatePrefix(), exp),
		Editor: r.ring.Sign(token.EditorScope(item.TempPrefix()), exp)}, nil
}

// SlotListing is what a host stores from Hooks.SlotEncoded to list a slot
// without reads (Reader.ListedSlot).
type SlotListing struct {
	Aspect  Aspect          `json:"aspect"`
	Outputs []SlotRendition `json:"outputs"`
}

// Listing is the slot's current outputs, for Hooks.SlotEncoded.
func (res *SlotResult) Listing(s Slot) SlotListing {
	l := SlotListing{Aspect: s.Aspect, Outputs: res.Outputs}
	if n := len(res.Outputs); n > 0 && s.Native() {
		l.Aspect = AspectOf(res.Outputs[n-1].W, res.Outputs[n-1].H)
	}
	return l
}

// ListedSlot is a slot's manifest built without reads, for listings, from
// the SlotListing the host stored: every output's public URL. Hosts list only
// items that are not hidden.
func (r *Reader) ListedSlot(ref contentref.ContentRef, slot string, l SlotListing) (SlotManifest, error) {
	item, s, err := r.kinds.slot(ref, slot)
	if err != nil {
		return SlotManifest{}, err
	}
	aspect := s.Aspect
	if s.Native() {
		aspect = l.Aspect
	}
	out := SlotManifest{Aspect: aspect, Outputs: []SlotImage{}, MinWidth: s.Min(), Animation: s.Animation}
	urls := OutputURLs{BaseURL: r.base.String()}
	for _, o := range l.Outputs {
		if layout.ValidHashName(o.Blob) {
			out.Outputs = append(out.Outputs, SlotImage{W: o.W, H: o.H, URL: urls.url(item, o.Blob, true)})
		}
	}
	return out, nil
}

// InlineURL is an inline image's public URL, or ErrPending until the worker
// has rendered it.
func (r *Reader) InlineURL(ctx context.Context, ref contentref.ContentRef, id string) (string, error) {
	item, err := r.kinds.Item(ref.Content())
	if err != nil {
		return "", err
	}
	if !item.Inline(id) {
		return "", fmt.Errorf("media: kind %q takes no inline image %q", item.Kind().Name, id)
	}
	root, _, err := r.manifests.Root(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return "", ErrPending
	} else if err != nil {
		return "", err
	}
	rec := root.Slots[id]
	if rec == nil || rec.Result == nil || len(rec.Result.Outputs) == 0 {
		return "", ErrPending
	}
	blob := rec.Result.Outputs[len(rec.Result.Outputs)-1].Blob
	if !root.Private[blob].Public {
		return "", ErrPending
	}
	return OutputURLs{BaseURL: r.base.String()}.url(item, blob, true), nil
}

// ErrPending: an inline image is not rendered (or exposed) yet.
var ErrPending = errors.New("media: not rendered yet")

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

// slot resolves a registered slot or an inline image, which renders as a
// native slot with one width.
func (r *Registry) slot(ref contentref.ContentRef, slot string) (Item, Slot, error) {
	item, err := r.Item(ref.Content())
	if err != nil {
		return Item{}, Slot{}, fmt.Errorf("%w: %v", ErrNotVisible, err)
	}
	if s, ok := item.Kind().Slots[slot]; ok {
		return item, s, nil
	}
	if item.Inline(slot) {
		return item, InlineSlot(*item.Kind().Inline), nil
	}
	return Item{}, Slot{}, fmt.Errorf("%w: kind %q has no slot %q", ErrNotVisible, item.Kind().Name, slot)
}

// fingerprint is the record's Fingerprint under its slot's spec.
func (r *Registry) fingerprint(item Item, slot string, rec SlotRecord) string {
	if s, ok := item.Kind().Slots[slot]; ok {
		return rec.Fingerprint(s)
	}
	return rec.Fingerprint(InlineSlot(*item.Kind().Inline))
}

// InlineSlot is the Slot an inline image renders as: its spec's width at its
// own aspect.
func InlineSlot(s Spec) Slot {
	return Slot{Aspect: AspectNative, Widths: []int{s.Width}, Quality: s.Quality}
}
