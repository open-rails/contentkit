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
	"slices"
	"strconv"
	"strings"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

const slotRecordExt = ".json"

// SlotRecord is originals/{slot}.json, private like the original. Commits
// and edits set Original and Edit; the image job sets Result.
type SlotRecord struct {
	Original string      `json:"original"`       // ETag of originals/{slot} when committed
	Edit     *Edit       `json:"edit,omitempty"` // nil: the centred crop at the slot's Aspect
	Result   *SlotResult `json:"result,omitempty"`
}

// SlotResult is what the served outputs were derived from.
type SlotResult struct {
	Of      string `json:"of"`              // the Fingerprint last encoded
	Version string `json:"version"`         // the Fingerprint Outputs were encoded under (Of, unless it failed)
	Source  string `json:"source"`          // the original (ETag) Dims measure
	Dims    Dims   `json:"dims"`            // EXIF-oriented; zero when undecodable
	Outputs []Dims `json:"outputs"`         // by ascending width
	Error   string `json:"error,omitempty"` // Of failed; Outputs are older
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
			rec.Original, rec.Edit = obj.ETag, edit
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
				return uploadErr(CodeInvalid, "edit: %v", err)
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
			return uploadErr(CodeInvalid, "file %q: %v", r.File, err)
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
		rec.Original, rec.Edit = put.ETag, edit
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

// SlotManifest describes a slot for srcset: its produced outputs by ascending
// width. Output URLs carry ?v={version}; the access worker serves a matching
// version as immutable, so a re-encode is a new URL.
type SlotManifest struct {
	Aspect  float64     `json:"aspect"`
	Edit    *Edit       `json:"edit,omitempty"`    // nil: the centred crop at aspect
	Dims    *Dims       `json:"dims,omitempty"`    // the committed original, EXIF-oriented, once measured
	Version string      `json:"version,omitempty"` // of the outputs listed
	Outputs []SlotImage `json:"outputs"`
	Pending bool        `json:"pending"`         // a commit, edit or spec change is not encoded yet
	Error   string      `json:"error,omitempty"` // the latest encode failed; the outputs are older
}

// SlotImage is one produced output.
type SlotImage struct {
	Name string `json:"name"`
	W    int    `json:"w"`
	H    int    `json:"h"`
	URL  string `json:"url"`
}

// SlotManifest reads a slot's manifest (one object), building output URLs on
// baseURL, the access worker origin. A slot never committed has no outputs.
func (m *Manifests) SlotManifest(ctx context.Context, baseURL string, ref contentref.ContentRef, slot string) (SlotManifest, error) {
	item, s, err := m.kinds.slot(ref, slot)
	if err != nil {
		return SlotManifest{}, err
	}
	out := SlotManifest{Aspect: s.Aspect, Outputs: []SlotImage{}}
	rec, err := m.Slot(ctx, ref, slot)
	if errors.Is(err, ErrNotFound) {
		return out, nil
	} else if err != nil {
		return SlotManifest{}, err
	}
	out.Edit = rec.Edit
	if d := rec.dims(); d.W > 0 {
		out.Dims = &d
	}
	fp := rec.Fingerprint(s)
	res := rec.Result
	out.Pending = res == nil || res.Of != fp
	if res == nil {
		return out, nil
	}
	if res.Of == fp {
		out.Error = res.Error
	}
	if len(res.Outputs) > 0 {
		out.Version = res.Version
	}
	for _, o := range res.Outputs {
		out.Outputs = append(out.Outputs, slotImage(baseURL, item, slot, o, out.Version))
	}
	return out, nil
}

// Slot reads a slot's manifest (no Resolve: outputs are public).
func (r *Reader) Slot(ctx context.Context, ref contentref.ContentRef, slot string) (SlotManifest, error) {
	return r.manifests.SlotManifest(ctx, r.base.String(), ref, slot)
}

// SlotStamp is the one value a host stores per slot to build its outputs
// without reads: the encode's version and the widths it produced,
// "{version}:{w},{w}…". Hooks.SlotEncoded reports it; the zero stamp is a
// slot the host never saw encoded.
type SlotStamp string

// NewSlotStamp stamps outputs of widths encoded under version.
func NewSlotStamp(version string, widths []int) SlotStamp {
	b := []byte(version + ":")
	for i, w := range widths {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendInt(b, int64(w), 10)
	}
	return SlotStamp(b)
}

// Parse splits the stamp; the zero stamp is ("", nil).
func (s SlotStamp) Parse() (version string, widths []int, err error) {
	if s == "" {
		return "", nil, nil
	}
	version, list, ok := strings.Cut(string(s), ":")
	if !ok || version == "" || list == "" {
		return "", nil, fmt.Errorf("media: malformed slot stamp %q", s)
	}
	for f := range strings.SplitSeq(list, ",") {
		w, err := strconv.Atoi(f)
		if err != nil || w <= 0 || w > maxSlotWidth || strconv.Itoa(w) != f || (len(widths) > 0 && w <= widths[len(widths)-1]) {
			return "", nil, fmt.Errorf("media: malformed slot stamp %q", s)
		}
		widths = append(widths, w)
	}
	return version, widths, nil
}

// Stamp is the manifest's SlotStamp ("" before the first encode), e.g. to
// backfill hosts that adopt Hooks.SlotEncoded after slots were set.
func (m SlotManifest) Stamp() SlotStamp {
	if m.Version == "" || len(m.Outputs) == 0 {
		return ""
	}
	widths := make([]int, len(m.Outputs))
	for i, o := range m.Outputs {
		widths[i] = o.W
	}
	return NewSlotStamp(m.Version, widths)
}

// SlotOutputs lists, reading nothing, the outputs of a slot encoded as stamp,
// with immutable ?v= URLs: the SlotManifest.Outputs of that encode. Widths
// the slot no longer declares are left out. The zero stamp lists the widths
// up to Min, which every processed slot has, with URLs revalidated on every
// view (a slot never uploaded answers 404).
func (r *Reader) SlotOutputs(ref contentref.ContentRef, slot string, stamp SlotStamp) ([]SlotImage, error) {
	item, s, err := r.kinds.slot(ref, slot)
	if err != nil {
		return nil, err
	}
	version, widths, err := stamp.Parse()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if stamp == "" {
		for _, w := range s.Widths {
			if w <= s.Min() {
				widths = append(widths, w)
			}
		}
	}
	out := []SlotImage{}
	for _, w := range widths {
		if slices.Contains(s.Widths, w) {
			out = append(out, slotImage(r.base.String(), item, slot, Dims{W: w, H: s.Height(w)}, version))
		}
	}
	return out, nil
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

// SlotVersionParam is the query parameter carrying an output's version.
const SlotVersionParam = layout.VersionParam

func slotImage(base string, item Item, slot string, o Dims, version string) SlotImage {
	key, _ := item.SlotOutput(slot, o.W)
	u := strings.TrimRight(base, "/") + "/" + key
	if version != "" {
		u += "?" + SlotVersionParam + "=" + version
	}
	return SlotImage{Name: SlotOutput(slot, o.W), W: o.W, H: o.H, URL: u}
}
