package mediastore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/contentref"
	media "github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// Promotion selects one immutable object and its desired area inside the same
// content item. Internal is used when a previously released file is withdrawn.
type Promotion struct {
	Source string `json:"source"`
	Area   string `json:"area"`
}

// DeliveryFile records the exact source and destination of one storage copy.
// Blob is an item-relative reference, never an arbitrary bucket key.
type DeliveryFile struct {
	Source  string `json:"source"`
	Blob    string `json:"blob"`
	Type    string `json:"type"`
	Size    int64  `json:"size"`
	Receipt string `json:"receipt,omitempty"`
}

const copyReceiptMetadata = "contentkit-copy-receipt"

func copyReceipt(operation string, f DeliveryFile) string {
	body, _ := json.Marshal(struct {
		Operation, Source, Destination, Type string
		Size                                 int64
	}{operation, f.Source, f.Blob, f.Type, f.Size})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func copyOptions(source media.Object, f DeliveryFile) media.CopyOptions {
	return media.CopyOptions{IfMatch: source.ETag, Metadata: map[string]string{copyReceiptMetadata: f.Receipt}}
}

// PublicationDelivery pins a durable publication operation's objects. It contains no
// chapter membership, schedule or audience policy; the catalog owns those.
type PublicationDelivery struct {
	ID          string         `json:"id"`
	Fingerprint string         `json:"fingerprint"`
	Files       []DeliveryFile `json:"files"`
	Adopted     bool           `json:"adopted,omitempty"`
	Ready       bool           `json:"ready,omitempty"`
	Withdrawn   bool           `json:"withdrawn,omitempty"`
}

var ErrDeliveryWithdrawn = errors.New("media: delivery was withdrawn")

func (d PublicationDelivery) clone() *PublicationDelivery {
	d.Files = slices.Clone(d.Files)
	return &d
}

func (m *Manifest) delivery(operation string) *PublicationDelivery {
	for i := range m.Deliveries {
		if m.Deliveries[i].ID == operation {
			return &m.Deliveries[i]
		}
	}
	return nil
}

// Delivered finds a completed delivery of source in area. Current file and
// publication references may already name the returned allocation directly.
func (m *Manifest) Delivered(source, area string) (string, bool) {
	for _, d := range slices.Backward(m.Deliveries) {
		if !d.Ready || d.Withdrawn {
			continue
		}
		for _, f := range d.Files {
			actual, _, ok := layout.SplitBlobRef(f.Blob)
			if ok && actual == area && f.Source == source {
				return f.Blob, true
			}
		}
	}
	return "", false
}

func validateDeliveries(deliveries []PublicationDelivery) error {
	seen := map[string]bool{}
	for _, d := range deliveries {
		id, err := uuid.Parse(d.ID)
		if err != nil || id == uuid.Nil || id.String() != d.ID || seen[d.ID] || len(d.Fingerprint) != 64 {
			return errors.New("media: invalid delivery identity")
		}
		if _, err := hex.DecodeString(d.Fingerprint); err != nil {
			return errors.New("media: invalid delivery fingerprint")
		}
		seen[d.ID] = true
		for _, f := range d.Files {
			source, sourceOK := layout.BlobDigest(f.Source)
			destination, destinationOK := layout.BlobDigest(f.Blob)
			area, _, _ := layout.SplitBlobRef(f.Blob)
			if !sourceOK || !destinationOK || !bytes.Equal(source, destination) ||
				area != layout.AreaPublic && area != layout.AreaPrivate && area != layout.AreaInternal ||
				f.Type == "" || f.Size < 0 || f.Receipt != copyReceipt(d.ID, f) {
				return errors.New("media: invalid delivery object")
			}
		}
	}
	return nil
}

// PreparePromotion durably reserves and pins exact copies before any writes.
// operation is the owning catalog request's canonical UUID. Repeating that
// UUID must repeat the same ordered inputs; a withdrawn UUID is never reused.
// The caller must persist its publication request before calling this method.
func (m *Manifests) PreparePromotion(ctx context.Context, ref contentref.ContentRef, operation string, inputs []Promotion) (*PublicationDelivery, error) {
	id, err := uuid.Parse(operation)
	if err != nil || id == uuid.Nil || id.String() != operation {
		return nil, ErrCommitIdentity
	}
	item, err := m.reg.Item(ref)
	if err != nil {
		return nil, err
	}
	if !item.Kind().Versions {
		return nil, errors.New("media: promotion requires a version-owned content item")
	}
	if withdrawn, err := m.deliveryWithdrawn(ctx, item, id); err != nil {
		return nil, err
	} else if withdrawn {
		return nil, ErrDeliveryWithdrawn
	}
	seen := map[string]bool{}
	for _, input := range inputs {
		if _, valid := layout.BlobDigest(input.Source); !valid || seen[input.Source] ||
			input.Area != layout.AreaPublic && input.Area != layout.AreaPrivate && input.Area != layout.AreaInternal {
			return nil, errors.New("media: invalid or duplicate promotion source")
		}
		seen[input.Source] = true
	}
	body, err := json.Marshal(inputs)
	if err != nil {
		return nil, err
	}
	fingerprint := sha256.Sum256(body)
	d := PublicationDelivery{ID: operation, Fingerprint: hex.EncodeToString(fingerprint[:]), Files: make([]DeliveryFile, len(inputs))}
	current, _, err := m.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	if previous := current.delivery(operation); previous != nil {
		return matchDelivery(previous, d.Fingerprint)
	}
	known := current.Blobs()
	for i, input := range inputs {
		if !slices.Contains(known, input.Source) {
			return nil, fmt.Errorf("media: promotion source %q is not retained", input.Source)
		}
		key, _ := item.Blob(input.Source)
		object, err := m.store.Head(ctx, key)
		if err != nil {
			return nil, err
		}
		sum, _ := layout.BlobDigest(input.Source)
		if len(object.ChecksumSHA256) > 0 && !bytes.Equal(object.ChecksumSHA256, sum) {
			return nil, ErrChecksumMismatch
		}
		blob := input.Source
		if area, _, _ := layout.SplitBlobRef(input.Source); area != input.Area {
			blobs, err := m.newBlobs(ctx, ref, input.Area, [][]byte{sum})
			if err != nil {
				return nil, err
			}
			blob = blobs[0]
		}
		d.Files[i] = DeliveryFile{Source: input.Source, Blob: blob, Type: object.ContentType, Size: object.Size}
		d.Files[i].Receipt = copyReceipt(operation, d.Files[i])
	}
	ctx = context.WithValue(ctx, synchronousMutationOwnerKey{}, true)
	result, err := m.EditExisting(ctx, ref, func(next *Manifest) error {
		if withdrawn, err := m.deliveryWithdrawn(ctx, item, id); err != nil {
			return err
		} else if withdrawn {
			return ErrDeliveryWithdrawn
		}
		if previous := next.delivery(operation); previous != nil {
			_, err := matchDelivery(previous, d.Fingerprint)
			return err
		}
		known := next.Blobs()
		for _, f := range d.Files {
			if !slices.Contains(known, f.Source) {
				return fmt.Errorf("media: promotion source %q changed before admission", f.Source)
			}
		}
		next.Deliveries = append(next.Deliveries, d)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return matchDelivery(result.delivery(operation), d.Fingerprint)
}

func matchDelivery(d *PublicationDelivery, fingerprint string) (*PublicationDelivery, error) {
	if d == nil || d.Fingerprint != fingerprint {
		return nil, ErrCommitIdentity
	}
	if d.Withdrawn {
		return nil, ErrDeliveryWithdrawn
	}
	return d.clone(), nil
}

// Promote resumes the exact copies prepared by PreparePromotion. After all
// destinations are verified it adopts them in the current manifest, retaining
// originals and retiring unreferenced former locations through the journal.
// This is storage completion; the caller commits catalog publication afterward.
func (m *Manifests) Promote(ctx context.Context, ref contentref.ContentRef, operation string) (*PublicationDelivery, error) {
	item, err := m.reg.Item(ref)
	if err != nil {
		return nil, err
	}
	var prepared *PublicationDelivery
	err = m.WithLock(ctx, ref, func() error {
		current, _, err := m.Get(ctx, ref)
		if err != nil {
			return err
		}
		d := current.delivery(operation)
		if d == nil {
			if id, err := uuid.Parse(operation); err == nil {
				if withdrawn, err := m.deliveryWithdrawn(ctx, item, id); err != nil {
					return err
				} else if withdrawn {
					return ErrDeliveryWithdrawn
				}
			}
			return ErrNotFound
		}
		if d.Withdrawn || current.Deleted {
			return ErrDeliveryWithdrawn
		}
		prepared = d.clone()
		if d.Ready || d.Adopted {
			return nil
		}
		keys := make([]string, 0, 2*len(d.Files))
		for _, f := range d.Files {
			source, _ := item.Blob(f.Source)
			destination, _ := item.Blob(f.Blob)
			keys = append(keys, source, destination)
		}
		if err := m.checkAllocationsIn(ctx, item, keys, current.Incarnation); err != nil {
			return err
		}
		for _, f := range d.Files {
			source, _ := item.Blob(f.Source)
			destination, _ := item.Blob(f.Blob)
			if err := m.verifyDelivery(ctx, destination, f); err == nil {
				continue
			} else if !errors.Is(err, ErrNotFound) {
				return err
			}
			object, err := m.store.Head(ctx, source)
			if err != nil {
				return err
			}
			if object.Size != f.Size || object.ContentType != f.Type {
				return ErrCommitIdentity
			}
			if _, err := m.store.Copy(ctx, source, destination, copyOptions(object, f)); err != nil {
				return err
			}
			if err := m.verifyDelivery(ctx, destination, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || prepared.Ready {
		return prepared, err
	}
	ctx = context.WithValue(ctx, synchronousMutationOwnerKey{}, true)
	id, err := uuid.Parse(operation)
	if err != nil {
		return nil, ErrCommitIdentity
	}
	adoption := uuid.NewSHA1(id, []byte("adopt-delivery"))
	mutation := &manifestMutation{commit: manifestCommit{ID: adoption, Fingerprint: sha256.Sum256([]byte(prepared.Fingerprint))}}
	_, err = m.editOperation(ctx, ref, true, bound{}, mutation, func(current *Manifest) error {
		d := current.delivery(operation)
		if d == nil || d.Fingerprint != prepared.Fingerprint {
			return ErrCommitIdentity
		}
		if d.Withdrawn {
			return ErrDeliveryWithdrawn
		}
		if d.Ready || d.Adopted {
			return nil
		}
		mapping := make(map[string]string, len(d.Files))
		for _, f := range d.Files {
			key, _ := item.Blob(f.Blob)
			if err := m.verifyDelivery(ctx, key, f); err != nil {
				return err
			}
			mapping[f.Source] = f.Blob
		}
		for i := range current.Files {
			f := &current.Files[i]
			if !f.IsUpload() {
				if next := mapping[f.Blob]; next != "" {
					f.Blob = next
				}
			}
			if f.Track != nil {
				if next := mapping[f.Track.Index]; next != "" {
					f.Track.Index = next
				}
			}
			for j := range f.Public {
				for k, blob := range f.Public[j].Blobs {
					if next := mapping[blob]; next != "" {
						f.Public[j].Blobs[k] = next
					}
				}
			}
		}
		d.Adopted = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := m.completeRetirement(ctx, item, adoption); err != nil {
		return nil, err
	}
	result, err := m.EditExisting(ctx, ref, func(current *Manifest) error {
		d := current.delivery(operation)
		if d == nil || d.Fingerprint != prepared.Fingerprint || !d.Adopted {
			return ErrCommitIdentity
		}
		if d.Withdrawn {
			return ErrDeliveryWithdrawn
		}
		d.Ready = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result.delivery(operation).clone(), nil
}

func (m *Manifests) verifyDelivery(ctx context.Context, key string, f DeliveryFile) error {
	object, err := m.store.Head(ctx, key)
	if err != nil {
		return err
	}
	sum, _ := layout.BlobDigest(f.Blob)
	if object.Size != f.Size || object.ContentType != f.Type ||
		len(object.ChecksumSHA256) > 0 && !bytes.Equal(object.ChecksumSHA256, sum) {
		return ErrChecksumMismatch
	}
	// Sources were hashed during admission/processing. S3 copies those exact
	// immutable bytes conditionally, attaching this receipt atomically to the
	// destination. Retry verification therefore needs no application GET of
	// multi-gigabyte outputs when a backend omits a full-object SHA-256 header.
	if f.Source != f.Blob && (f.Receipt == "" || object.Metadata[copyReceiptMetadata] != f.Receipt) {
		return ErrCommitIdentity
	}
	return nil
}

// WithdrawPromotion releases an operation's storage pins. Current canonical
// file references and other operations still protect their own allocations.
// To withdraw readable bytes, first Promote their desired internal placement,
// commit that reader state, then release obsolete operation pins with this call.
func (m *Manifests) WithdrawPromotion(ctx context.Context, ref contentref.ContentRef, operation string) error {
	id, err := uuid.Parse(operation)
	if err != nil || id == uuid.Nil || id.String() != operation {
		return ErrCommitIdentity
	}
	item, err := m.reg.Item(ref)
	if err != nil {
		return err
	}
	retirement := uuid.NewSHA1(id, []byte("withdraw-delivery-pins"))
	mutation := &manifestMutation{commit: manifestCommit{ID: retirement, Fingerprint: sha256.Sum256([]byte(operation))}}
	ctx = context.WithValue(ctx, synchronousMutationOwnerKey{}, true)
	_, err = m.editOperation(ctx, ref, true, bound{}, mutation, func(current *Manifest) error {
		d := current.delivery(operation)
		if d == nil {
			return ErrNotFound
		}
		d.Withdrawn = true
		return nil
	})
	if err != nil {
		return err
	}
	if err := m.completeRetirement(ctx, item, retirement); err != nil {
		return err
	}
	// The applied withdrawal receipt remains in the existing durable journal.
	// The live manifest only needs active pins, not every past publication.
	_, err = m.EditExisting(ctx, ref, func(current *Manifest) error {
		current.Deliveries = slices.DeleteFunc(current.Deliveries, func(d PublicationDelivery) bool { return d.ID == operation && d.Withdrawn })
		return nil
	})
	return err
}

func (m *Manifests) deliveryWithdrawn(ctx context.Context, item Item, operation uuid.UUID) (bool, error) {
	withdrawal := uuid.NewSHA1(operation, []byte("withdraw-delivery-pins"))
	record, err := m.journal.existing(ctx, manifestCommit{ID: withdrawal, Ref: item.Ref(), Folder: item.Prefix(), Fingerprint: sha256.Sum256([]byte(operation.String()))})
	return record != nil && record.State != "absent", err
}

// completeRetirement executes the same exact-key intent as the existing purge
// owner. Publication withdrawal cannot wait for its periodic late-write sweep.
// The retained intent still lets that sweep catch delayed writes afterward.
func (m *Manifests) completeRetirement(ctx context.Context, item Item, operation uuid.UUID) error {
	jobs := &Jobs{cfg: JobsConfig{Store: m.store, Registry: m.reg, Locker: m.locker, Journal: m.journal}, manifests: m}
	return jobs.repeatDeletion(ctx, item, operation)
}
