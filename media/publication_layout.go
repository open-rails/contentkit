package media

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

var ErrPublicationLayoutRequired = errors.New("media: explicit publication layout migration required")

const publicationLayoutActor = "migration:publication-layout"

// publicationLayoutPlan is an explicit migration receipt in the existing
// journal. Startup never creates it or discovers objects to populate it.
type publicationLayoutPlan struct {
	SourceDigest string         `json:"source_digest"`
	Manifest     *Manifest      `json:"manifest"`
	Copies       []DeliveryFile `json:"copies"`
	Retired      []string       `json:"retired"`
	Adopted      bool           `json:"adopted"`
	Complete     bool           `json:"complete"`
}

// MigratePublicationLayout explicitly stages a legacy item's uploads in
// originals and its prepared outputs in future. Old writers and readers must
// be stopped. After this conversion the normal publisher applies the approved
// released set before cutover. The marker is set only after exact old served
// keys are confirmed absent; failed copies/deletes retain their known receipt.
func (m *Manifests) MigratePublicationLayout(ctx context.Context, ref contentref.ContentRef, operation string) error {
	id, err := uuid.Parse(operation)
	if err != nil || id == uuid.Nil || id.String() != operation {
		return ErrCommitIdentity
	}
	item, err := m.reg.Item(ref)
	if err != nil {
		return err
	}
	if !item.Kind().Versions {
		return errors.New("media: publication layout migration requires a content version")
	}
	ctx = context.WithValue(ctx, synchronousMutationOwnerKey{}, true)
	unlock, err := m.locker.Lock(ctx, "publication-layout:"+item.ManifestKey())
	if err != nil {
		return err
	}
	defer unlock()
	if err := m.Recover(ctx, ref); err != nil {
		return err
	}
	journal := m.journal.forTenant(ref.TenantID)
	var record manifestCommit
	err = pgx.BeginFunc(ctx, journal.pool, func(tx pgx.Tx) error {
		var err error
		record, err = journal.load(ctx, tx, ref.TenantID, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		var current *Manifest
		current, _, err = m.Get(ctx, ref)
		if errors.Is(err, ErrNotFound) {
			// A version that never received media: its first manifest is
			// already in the publication layout.
			current, err = m.Edit(ctx, ref, func(*Manifest) error { return nil })
		}
		if err != nil {
			return err
		}
		if current.PublicationLayout == PublicationLayoutVersion || current.Deleted {
			return nil
		}
		var plan *publicationLayoutPlan
		plan, err = m.planPublicationLayout(ctx, item, id, current)
		if err != nil {
			return err
		}
		var body []byte
		body, err = json.Marshal(journalEffects{PublicationLayoutPlan: plan})
		if err != nil {
			return err
		}
		fingerprint := sha256.Sum256([]byte(plan.SourceDigest))
		err = pgx.BeginFunc(ctx, journal.pool, func(tx pgx.Tx) error {
			if err := journal.checkFolderOwner(ctx, tx, item, false); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO `+journal.table+`
(tenant_id,operation_id,content_kind,content_id,content_version_id,folder_prefix,actor_id,fingerprint,lease_id,state,effects)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'applied',$10) ON CONFLICT(tenant_id,operation_id) DO NOTHING`,
				ref.TenantID, id, ref.ContentKind, ref.ContentID, ref.ContentVersionID, item.Prefix(), publicationLayoutActor, fingerprint[:], uuid.New(), body)
			return err
		})
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, journal.pool, func(tx pgx.Tx) error {
			var err error
			record, err = journal.load(ctx, tx, ref.TenantID, id)
			return err
		})
	}
	if err != nil {
		return err
	}
	plan := record.Effects.PublicationLayoutPlan
	if !record.Ref.Equal(ref) || record.Folder != item.Prefix() || record.Actor != publicationLayoutActor || plan == nil {
		return ErrCommitIdentity
	}
	if plan.Complete {
		return nil
	}
	current, _, err := m.Get(ctx, ref)
	if err != nil {
		return err
	}
	if !plan.Adopted {
		adoptID := uuid.NewSHA1(id, []byte("adopt-publication-layout"))
		mutation := &manifestMutation{layoutCopy: true, commit: manifestCommit{ID: adoptID, Fingerprint: sha256.Sum256([]byte(plan.SourceDigest))}}
		if prior, done, err := m.replayCommit(ctx, item, manifestCommit{ID: adoptID, Ref: ref, Folder: item.Prefix(), Fingerprint: mutation.commit.Fingerprint}); err != nil {
			return err
		} else if !done {
			if digestManifest(current) != plan.SourceDigest {
				return errors.New("media: legacy publication layout changed after planning")
			}
			var keys []string
			for _, copy := range plan.Copies {
				key, _ := item.Blob(copy.Blob)
				keys = append(keys, key)
			}
			if err := m.reserveMigrationAllocations(ctx, item, keys); err != nil {
				return err
			}
			for _, copy := range plan.Copies {
				source := item.Prefix() + copy.Source
				destination, _ := item.Blob(copy.Blob)
				if err := m.verifyDelivery(ctx, destination, copy); err == nil {
					continue
				} else if !errors.Is(err, ErrNotFound) {
					return err
				}
				object, err := m.store.Head(ctx, source)
				if err != nil {
					return err
				}
				if _, err := m.store.Copy(ctx, source, destination, copyOptions(object, copy)); err != nil {
					return err
				}
				if err := m.verifyDelivery(ctx, destination, copy); err != nil {
					return err
				}
			}
			_, err := m.editOperation(ctx, ref, true, bound{}, mutation, func(next *Manifest) error {
				if digestManifest(next) != plan.SourceDigest {
					return errors.New("media: legacy publication layout changed during copying")
				}
				*next = *plan.Manifest.Clone()
				// Older served allocations may already be absent from the source
				// manifest while still awaiting retained cleanup. A new folder
				// token must not open those old bytes after conversion.
				mutation.effects.Private = append(mutation.effects.Private, plan.Retired...)
				return nil
			})
			if err != nil {
				return err
			}
		} else {
			current = prior
		}
		plan.Adopted = true
		if err := journal.savePublicationLayout(ctx, record, plan); err != nil {
			return err
		}
	}
	// The adopting edit already owns exact deletion. Recovery completes any
	// uncertain result before the absent-object verification and readiness mark.
	if err := m.Recover(ctx, ref); err != nil {
		return err
	}
	if err := m.completeRetirement(ctx, item, uuid.NewSHA1(id, []byte("adopt-publication-layout"))); err != nil {
		return err
	}
	for _, key := range plan.Retired {
		if _, err := m.store.Head(ctx, key); !errors.Is(err, ErrNotFound) {
			if err != nil {
				return err
			}
			return fmt.Errorf("%w: old served object remains: %s", ErrPublicationLayoutRequired, key)
		}
	}
	_, err = m.EditExisting(ctx, ref, func(next *Manifest) error {
		if next.PublicationLayout == PublicationLayoutVersion {
			return nil
		}
		if digestManifest(next) != digestManifest(plan.Manifest) {
			return errors.New("media: migrated layout changed before completion")
		}
		next.PublicationLayout = PublicationLayoutVersion
		return nil
	})
	if err != nil {
		return err
	}
	plan.Complete = true
	return journal.savePublicationLayout(ctx, record, plan)
}

func (j *PGJournal) savePublicationLayout(ctx context.Context, record manifestCommit, plan *publicationLayoutPlan) error {
	body, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	tag, err := j.pool.Exec(ctx, `UPDATE `+j.table+` SET effects=jsonb_set(effects,'{publication_layout_plan}',$3::jsonb),updated_at=now()
WHERE tenant_id=$1 AND operation_id=$2 AND actor_id=$4 AND fingerprint=$5`, record.Ref.TenantID, record.ID, body, publicationLayoutActor, record.Fingerprint[:])
	if err == nil && tag.RowsAffected() != 1 {
		return ErrCommitIdentity
	}
	return err
}

func (m *Manifests) reserveMigrationAllocations(ctx context.Context, item Item, keys []string) error {
	return m.cleanup(ctx, item, false, func(current *Manifest, exists bool) (journalEffects, error) {
		if !exists || current.Deleted {
			return journalEffects{}, ErrNotFound
		}
		known, err := m.allocationKeys(ctx, item)
		if err != nil {
			return journalEffects{}, err
		}
		var allocate []string
		for _, key := range keys {
			if slices.Contains(known, key) {
				if err := m.checkAllocationsIn(ctx, item, []string{key}, current.Incarnation); err != nil {
					return journalEffects{}, err
				}
			} else {
				allocate = append(allocate, key)
			}
		}
		return journalEffects{Allocate: allocate, Incarnation: current.Incarnation}, nil
	})
}

func (m *Manifests) planPublicationLayout(ctx context.Context, item Item, operation uuid.UUID, source *Manifest) (*publicationLayoutPlan, error) {
	if len(source.StagedNames()) != 0 || len(source.Deliveries) != 0 {
		return nil, errors.New("media: finish staged uploads and publication work before layout migration")
	}
	plan := &publicationLayoutPlan{SourceDigest: digestManifest(source), Manifest: source.Clone()}
	plan.Manifest.PublicationLayout = 0
	plan.Manifest.Receipt = nil
	plan.Manifest.Deliveries = nil
	known := map[string]string{}
	copyRef := func(ref, area, occurrence string) (string, error) {
		if ref == "" {
			return "", nil
		}
		fromArea, name, ok := layout.SplitBlobRef(ref)
		if !ok {
			return "", fmt.Errorf("media: invalid migration source %q", ref)
		}
		sourceRef := fromArea + "/" + name
		identity := sourceRef + ":" + area
		if area == layout.AreaFuture {
			// Legacy copies could share a derived blob across virtual files.
			// Their chapter membership can now diverge, so prepared outputs
			// need independent allocation lifetimes keyed by existing paths.
			identity += "\x00" + occurrence
		}
		if target, ok := known[identity]; ok {
			return target, nil
		}
		key := item.Prefix() + sourceRef
		object, err := m.store.Head(ctx, key)
		if err != nil {
			return "", fmt.Errorf("media: retained migration source %s: %w", key, err)
		}
		sum, named := layout.BlobDigest(ref)
		if !named {
			sum = object.ChecksumSHA256
		}
		if len(sum) != sha256.Size {
			reader, _, err := m.store.Get(ctx, key, GetOptions{})
			if err != nil {
				return "", err
			}
			hash := sha256.New()
			_, readErr := io.Copy(hash, reader)
			closeErr := reader.Close()
			if err := errors.Join(readErr, closeErr); err != nil {
				return "", err
			}
			sum = hash.Sum(nil)
		}
		allocation := uuid.NewSHA1(operation, []byte(identity)).String()
		target := layout.BlobRef(area, layout.BlobName(sum, allocation))
		known[identity] = target
		copy := DeliveryFile{Source: sourceRef, Blob: target, Type: object.ContentType, Size: object.Size}
		copy.Receipt = copyReceipt(operation.String(), copy)
		plan.Copies = append(plan.Copies, copy)
		plan.Retired = append(plan.Retired, key)
		return target, nil
	}
	for i := range plan.Manifest.Files {
		file := &plan.Manifest.Files[i]
		var err error
		oldKey := file.Key()
		if file.IsUpload() && file.Gone {
			return nil, fmt.Errorf("media: original %q was not retained; resolve before migration", file.Path)
		}
		if file.Blob != "" && !file.Gone {
			area := layout.AreaFuture
			if file.IsUpload() {
				area = layout.AreaOriginals
			}
			if file.Blob, err = copyRef(file.Blob, area, "file:"+file.Path); err != nil {
				return nil, err
			}
		}
		if file.Editor != nil {
			if file.Editor.Blob, err = copyRef(file.Editor.Blob, layout.AreaFuture, "editor:"+file.Path); err != nil {
				return nil, err
			}
		}
		if file.Track != nil {
			if file.Track.Index, err = copyRef(file.Track.Index, layout.AreaFuture, "track:"+file.Path); err != nil {
				return nil, err
			}
		}
		if file.Failed != nil && file.Failed.Of == oldKey {
			file.Failed.Of = file.Key()
		}
		for j := range file.Public {
			publication := &file.Public[j]
			if !publication.Ready() {
				return nil, errors.New("media: finish public-preset preparation before layout migration")
			}
			previous := slices.Clone(publication.Blobs)
			publication.Blobs = make([]string, len(publication.Names))
			for index, name := range publication.NamesOnDisk() {
				ref := layout.BlobRef(layout.AreaPublic, name)
				if len(previous) == len(publication.Names) {
					ref = previous[index]
				}
				if publication.Blobs[index], err = copyRef(ref, layout.AreaFuture, "public:"+file.Path+"\x00"+publication.Preset+"\x00"+name); err != nil {
					return nil, err
				}
			}
			publication.Source = file.Key()
			if publication.Stable == nil || *publication.Stable {
				for _, name := range publication.Names {
					plan.Retired = append(plan.Retired, item.PublicPrefix()+name)
				}
			}
		}
	}
	// Frame provenance names source uploads, not separate output objects.
	for i := range plan.Manifest.Files {
		file := &plan.Manifest.Files[i]
		if file.Frame != nil && file.Frame.Of != "" {
			area, name, valid := layout.SplitBlobRef(file.Frame.Of)
			if valid {
				if next := known[area+"/"+name+":"+layout.AreaOriginals]; next != "" {
					file.Frame.Of = next
				}
			}
		}
	}
	allocated, err := m.allocationKeys(ctx, item)
	if err != nil {
		return nil, err
	}
	for _, key := range allocated {
		parsed, valid := layout.Parse(key)
		if !valid || parsed.Folder() != item.Prefix() {
			return nil, ErrCommitIdentity
		}
		if parsed.Area == layout.AreaPrivate || parsed.Area == layout.AreaPublic || parsed.Area == layout.AreaDownload {
			plan.Retired = append(plan.Retired, key)
		}
	}
	slices.Sort(plan.Retired)
	plan.Retired = slices.Compact(plan.Retired)
	for _, key := range plan.Retired {
		parsed, valid := layout.Parse(key)
		if !valid || !strings.HasPrefix(key, item.Prefix()) || parsed.Folder() != item.Prefix() {
			return nil, errors.New("media: migration retirement escaped item")
		}
	}
	if err := plan.Manifest.Validate(); err != nil {
		return nil, err
	}
	return plan, nil
}

// PublicationLayoutReady is a local manifest check for the reader and
// publisher; it performs no migration or storage effects.
func (m *Manifest) PublicationLayoutReady() bool {
	return m != nil && m.PublicationLayout == PublicationLayoutVersion && !m.Deleted
}

// ValidatePublicationLayoutMigration is a read-only cold-start check over
// journal-owned version folders. It never scans a bucket, repairs a manifest
// or starts conversion. Engines and workers call it before accepting writes.
func (m *Manifests) ValidatePublicationLayoutMigration(ctx context.Context) error {
	for _, kind := range m.reg.Kinds() {
		if !kind.Versions {
			continue
		}
		journal := m.journal.forTenant(kind.ns)
		rows, err := journal.pool.Query(ctx, `SELECT DISTINCT content_id,content_version_id,folder_prefix FROM `+journal.table+`
WHERE tenant_id=$1 AND content_kind=$2 AND content_version_id IS NOT NULL ORDER BY folder_prefix`, kind.ns, kind.Name)
		if err != nil {
			return err
		}
		type owner struct{ Content, Version, Folder string }
		owners, err := pgx.CollectRows(rows, pgx.RowToStructByPos[owner])
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, entry := range owners {
			ref, err := m.reg.VersionRef(kind.Name, entry.Content, entry.Version)
			if err != nil {
				return err
			}
			item, err := m.reg.Item(ref)
			if err != nil {
				return err
			}
			if entry.Folder != item.Prefix() {
				// Prior nested storage has its own explicit owner/migration gate.
				// It cannot substitute for checking the canonical flat owner.
				if entry.Folder != kind.nestedPrefix(entry.Content, entry.Version) {
					return ErrCommitIdentity
				}
			}
			if seen[item.Prefix()] {
				continue
			}
			seen[item.Prefix()] = true
			if err := journal.checkFolderOwner(ctx, journal.pool, item, false); err != nil {
				return err
			}
			manifest, _, err := m.get(ctx, item.ManifestKey())
			if errors.Is(err, ErrNotFound) {
				var allocated, committed, erased bool
				err = journal.pool.QueryRow(ctx, `SELECT
EXISTS(SELECT 1 FROM `+journal.allocations+` WHERE tenant_id=$1 AND folder_prefix=$2),
EXISTS(SELECT 1 FROM `+journal.table+` WHERE tenant_id=$1 AND folder_prefix=$2 AND
 (state IN ('prepared','frozen') OR state='applied' AND effects->>'cleanup' IS DISTINCT FROM 'true')),
EXISTS(SELECT 1 FROM `+journal.table+` WHERE tenant_id=$1 AND folder_prefix=$2 AND state='applied' AND effects->>'erasure'='true')`, kind.ns, item.Prefix()).Scan(&allocated, &committed, &erased)
				if err != nil {
					return err
				}
				if !allocated && !committed && entry.Folder == item.Prefix() {
					continue
				}
				if erased {
					if _, err := m.store.Head(ctx, item.PublicPrefix()+layout.ErasureFenceName); err == nil {
						continue
					} else if !errors.Is(err, ErrNotFound) {
						return err
					}
				}
				return fmt.Errorf("%w: known version manifest is missing: %s", ErrPublicationLayoutRequired, item.Prefix())
			}
			if err != nil {
				return err
			}
			if manifest.Deleted {
				continue
			}
			if !manifest.PublicationLayoutReady() {
				return fmt.Errorf("%w: %s", ErrPublicationLayoutRequired, item.Prefix())
			}
		}
	}
	return nil
}
