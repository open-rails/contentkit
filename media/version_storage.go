package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
)

// VersionStorageReport describes one explicit, resumable migration pass.
// Old folders are retained until the normal cleanup grace and late-write pass.
type VersionStorageReport struct {
	Versions            int `json:"versions"`
	Copied              int `json:"copied"`
	Resumed             int `json:"resumed"`
	RetirementScheduled int `json:"retirement_scheduled"`
	Absent              int `json:"absent"`
}

// VersionStorageOptions supplies catalog proof only for the explicit migration.
// Without it, a missing previously committed source is always refused.
type VersionStorageOptions struct {
	// VersionPurged must verify the exact logical version is permanently absent,
	// not merely soft deleted. Media still requires a committed deletion, no
	// later reset, no live/recent allocations and an entirely empty old folder.
	VersionPurged func(context.Context, contentref.ContentRef) (bool, error)
}

// versionStoragePlan is a durable checkpoint in the existing media journal.
// It is an applied metadata receipt, never a media processing/notification job.
type versionStoragePlan struct {
	Plan            LayoutPlan `json:"plan"`
	ConfigHash      string     `json:"config_hash"`
	Phase           string     `json:"phase"`
	TargetOperation string     `json:"target_operation,omitempty"`
}

const versionStorageActor = "migration:version-storage"

// MigrateVersionStorage explicitly moves recorded nested version folders to
// version-owned folders. Call with all old writers/workers stopped. It persists
// each exact copy inventory before writing objects; retries reuse that plan.
// It creates no parent media item and never runs as part of app startup.
func (j *Jobs) MigrateVersionStorage(ctx context.Context, riverSchema string, options ...VersionStorageOptions) (VersionStorageReport, error) {
	var report VersionStorageReport
	if len(options) > 1 {
		return report, errors.New("media: at most one VersionStorageOptions value is supported")
	}
	var opt VersionStorageOptions
	if len(options) == 1 {
		opt = options[0]
	}
	unlock, err := lockVersionStorage(ctx, j.cfg.Journal, j.cfg.Registry)
	if err != nil {
		return report, err
	}
	defer unlock()
	encoded, err := j.cfg.Registry.MarshalJSON()
	if err != nil {
		return report, err
	}
	sum := sha256.Sum256(encoded)
	configHash := hex.EncodeToString(sum[:])
	candidates, err := versionStorageSources(ctx, j.cfg.Registry, j.cfg.Journal)
	if err != nil {
		return report, err
	}
	for _, source := range candidates {
		report.Versions++
		resumed, copied, absent, err := j.migrateVersionFolder(ctx, source, riverSchema, configHash, opt)
		if err != nil {
			return report, fmt.Errorf("media: migrate version folder %s: %w", source.Prefix(), err)
		}
		if absent {
			report.Absent++
			continue
		}
		if resumed {
			report.Resumed++
		}
		if copied {
			report.Copied++
		}
		report.RetirementScheduled++
	}
	return report, nil
}

func versionStorageSources(ctx context.Context, reg *Registry, journal *PGJournal) ([]Item, error) {
	var candidates []Item
	owners := map[string]contentref.ContentRef{}
	for _, kind := range reg.all {
		if !kind.Versions {
			continue
		}
		jr := journal.forTenant(kind.ns)
		rows, err := jr.pool.Query(ctx, `SELECT DISTINCT content_id,content_version_id,folder_prefix FROM `+jr.table+`
WHERE tenant_id=$1 AND content_kind=$2 AND content_version_id IS NOT NULL ORDER BY folder_prefix`, kind.ns, kind.Name)
		if err != nil {
			return nil, err
		}
		type old struct{ Content, Version, Prefix string }
		found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[old])
		if err != nil {
			return nil, err
		}
		for _, entry := range found {
			ref, err := reg.VersionRef(kind.Name, entry.Content, entry.Version)
			if err != nil {
				return nil, err
			}
			target, _ := reg.Item(ref)
			if entry.Prefix == target.Prefix() {
				continue
			}
			if entry.Prefix != kind.nestedPrefix(entry.Content, entry.Version) {
				return nil, ErrCommitIdentity
			}
			if previous, exists := owners[target.Prefix()]; exists {
				if !previous.Equal(ref) {
					return nil, ErrCommitIdentity
				}
				continue
			}
			owners[target.Prefix()] = ref
			candidates = append(candidates, Item{ref: ref, kind: kind, prefix: entry.Prefix})
		}
	}
	slices.SortFunc(candidates, func(a, b Item) int { return strings.Compare(a.Prefix(), b.Prefix()) })
	return candidates, nil
}

func versionStorageID(source, target Item) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("version-storage:"+source.Ref().TenantID+":"+source.Prefix()+":"+target.Prefix()))
}

func (j *Jobs) migrateVersionFolder(ctx context.Context, source Item, riverSchema, configHash string, opt VersionStorageOptions) (resumed, copied, absent bool, err error) {
	target, err := j.cfg.Registry.Item(source.Ref())
	if err != nil {
		return false, false, false, err
	}
	id := versionStorageID(source, target)
	jr := j.cfg.Journal.forTenant(source.Ref().TenantID)
	var record manifestCommit
	err = pgx.BeginFunc(ctx, jr.pool, func(tx pgx.Tx) error {
		var err error
		record, err = jr.load(ctx, tx, source.Ref().TenantID, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		var cp *versionStoragePlan
		if _, _, e := j.manifests.get(ctx, source.ManifestKey()); errors.Is(e, ErrNotFound) {
			for _, e := range j.cfg.Store.List(ctx, source.Prefix()) {
				if e != nil {
					return false, false, false, e
				}
				return false, false, false, errors.New("source has objects but no manifest")
			}
			if e := j.proveAbsentVersionStorage(ctx, source, opt); e != nil {
				return false, false, false, e
			}
			cp = &versionStoragePlan{Plan: LayoutPlan{Version: 1, Roots: layoutRoots(j.cfg.Registry), Entries: []LayoutEntry{{Mapping: LayoutMapping{From: source.Ref(), To: target.Ref(), SourceFolder: source.Prefix()}, SourceFolder: source.Prefix(), TargetFolder: target.Prefix()}}}, ConfigHash: configHash, Phase: "absence_checked"}
		} else if e != nil {
			return false, false, false, e
		} else {
			plan, e := PlanLayoutMigration(ctx, j.manifests, j.cfg.Registry, []LayoutMapping{{From: source.Ref(), To: target.Ref(), SourceFolder: source.Prefix()}})
			if e != nil {
				return false, false, false, e
			}
			cp = &versionStoragePlan{Plan: *plan, ConfigHash: configHash, Phase: "planned"}
		}
		if e := jr.persistVersionStoragePlan(ctx, id, target, cp); e != nil {
			return false, false, false, e
		}
		err = pgx.BeginFunc(ctx, jr.pool, func(tx pgx.Tx) error { var e error; record, e = jr.load(ctx, tx, source.Ref().TenantID, id); return e })
	} else {
		resumed = true
	}
	if err != nil {
		return resumed, false, false, err
	}
	cp := record.Effects.VersionStoragePlan
	if cp == nil || record.Actor != versionStorageActor || record.State != "applied" || !record.Ref.Equal(target.Ref()) || record.Folder != target.Prefix() || len(cp.Plan.Entries) != 1 {
		return resumed, false, false, ErrCommitIdentity
	}
	entry := cp.Plan.Entries[0]
	if !entry.Mapping.From.Equal(source.Ref()) || !entry.Mapping.To.Equal(target.Ref()) || entry.SourceFolder != source.Prefix() || entry.TargetFolder != target.Prefix() {
		return resumed, false, false, ErrCommitIdentity
	}
	fingerprint, e := versionStorageFingerprint(cp)
	if e != nil || fingerprint != record.Fingerprint {
		return resumed, false, false, ErrCommitIdentity
	}
	if cp.Phase != "retirement_scheduled" && cp.Phase != "absent" && cp.ConfigHash != configHash {
		return resumed, false, false, ErrLayoutChanged
	}
	switch cp.Phase {
	case "absent":
		return resumed, false, true, nil
	case "absence_checked":
		if err := j.proveAbsentVersionStorage(ctx, source, opt); err != nil {
			return resumed, false, false, err
		}
		if err := j.retireAbsentVersionStorage(ctx, source, riverSchema); err != nil {
			return resumed, false, false, err
		}
		cp.Phase = "absent"
		return resumed, false, true, jr.updateVersionStoragePlan(ctx, record, cp)
	case "retirement_scheduled":
		return resumed, false, false, nil
	case "planned":
		if err := ApplyLayoutMigration(ctx, j.manifests, j, &cp.Plan); err != nil {
			return resumed, false, false, err
		}
		current, _, err := j.manifests.get(ctx, target.ManifestKey())
		if err != nil {
			return resumed, false, false, err
		}
		if current.Receipt == nil {
			return resumed, false, false, ErrCommitIdentity
		}
		if err := jr.checkReceipt(ctx, target, current.Receipt); err != nil {
			return resumed, false, false, err
		}
		cp.TargetOperation = current.Receipt.Operation
		cp.Phase = "copied"
		if err := jr.updateVersionStoragePlan(ctx, record, cp); err != nil {
			return resumed, false, false, err
		}
		copied = true
	case "copied":
	default:
		return resumed, false, false, ErrCommitIdentity
	}
	targetOperation, err := uuid.Parse(cp.TargetOperation)
	if err != nil || targetOperation == uuid.Nil {
		return resumed, copied, false, ErrCommitIdentity
	}
	if err := verifyLayoutSource(ctx, j.manifests, source, entry); err != nil {
		return resumed, copied, false, err
	}
	keys := []string{source.ManifestKey()}
	for _, o := range entry.Objects {
		keys = append(keys, o.Source)
	}
	if err := j.scheduleVersionStorageRetirement(ctx, source, target, targetOperation, keys, j.cfg.Now().Add(j.cfg.Grace), riverSchema); err != nil {
		return resumed, copied, false, err
	}
	cp.Phase = "retirement_scheduled"
	return resumed, copied, false, jr.updateVersionStoragePlan(ctx, record, cp)
}

func versionStorageFingerprint(cp *versionStoragePlan) ([32]byte, error) {
	body, err := json.Marshal(struct {
		Plan   LayoutPlan
		Config string
	}{cp.Plan, cp.ConfigHash})
	return sha256.Sum256(body), err
}

func (j *PGJournal) persistVersionStoragePlan(ctx context.Context, id uuid.UUID, target Item, cp *versionStoragePlan) error {
	j = j.forTenant(target.Ref().TenantID)
	fingerprint, err := versionStorageFingerprint(cp)
	if err != nil {
		return err
	}
	body, err := json.Marshal(journalEffects{VersionStoragePlan: cp})
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
		if err := j.checkFolderOwner(ctx, tx, target, false); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO `+j.table+`(tenant_id,operation_id,content_kind,content_id,content_version_id,folder_prefix,actor_id,fingerprint,lease_id,state,effects)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'applied',$10) ON CONFLICT(tenant_id,operation_id) DO NOTHING`, target.Ref().TenantID, id, target.Ref().ContentKind, target.Ref().ContentID, target.Ref().ContentVersionID, target.Prefix(), versionStorageActor, fingerprint[:], uuid.New(), body)
		return err
	})
}

func (j *PGJournal) updateVersionStoragePlan(ctx context.Context, record manifestCommit, cp *versionStoragePlan) error {
	j = j.forTenant(record.Ref.TenantID)
	body, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	tag, err := j.pool.Exec(ctx, `UPDATE `+j.table+` SET effects=jsonb_set(effects,'{version_storage_plan}',$3::jsonb),updated_at=now()
WHERE tenant_id=$1 AND operation_id=$2 AND state='applied' AND actor_id=$4 AND fingerprint=$5`, record.Ref.TenantID, record.ID, body, versionStorageActor, record.Fingerprint[:])
	if err == nil && tag.RowsAffected() != 1 {
		return ErrCommitIdentity
	}
	return err
}

// ValidateVersionStorageMigration reads only journal decisions. Startup never
// scans or copies objects and cannot quietly serve an unmigrated directory.
func ValidateVersionStorageMigration(ctx context.Context, reg *Registry, journal *PGJournal) error {
	sources, err := versionStorageSources(ctx, reg, journal)
	if err != nil {
		return err
	}
	for _, source := range sources {
		target, err := reg.Item(source.Ref())
		if err != nil {
			return err
		}
		jr := journal.forTenant(source.Ref().TenantID)
		var body, fingerprint []byte
		err = jr.pool.QueryRow(ctx, `SELECT effects,fingerprint FROM `+jr.table+`
WHERE tenant_id=$1 AND operation_id=$2 AND actor_id=$3 AND state='applied' AND folder_prefix=$4 AND content_id=$5 AND content_version_id=$6`, source.Ref().TenantID, versionStorageID(source, target), versionStorageActor, target.Prefix(), source.Ref().ContentID, source.Ref().Version()).Scan(&body, &fingerprint)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("media: version storage requires explicit MigrateVersionStorage for %s", source.Ref())
		}
		if err != nil {
			return err
		}
		var effects journalEffects
		if err := json.Unmarshal(body, &effects); err != nil {
			return ErrCommitIdentity
		}
		cp := effects.VersionStoragePlan
		if cp == nil || len(cp.Plan.Entries) != 1 {
			return ErrCommitIdentity
		}
		entry := cp.Plan.Entries[0]
		if !entry.Mapping.From.Equal(source.Ref()) || !entry.Mapping.To.Equal(target.Ref()) || entry.SourceFolder != source.Prefix() || entry.TargetFolder != target.Prefix() {
			return ErrCommitIdentity
		}
		want, err := versionStorageFingerprint(cp)
		if err != nil || len(fingerprint) != len(want) || !bytes.Equal(fingerprint, want[:]) {
			return ErrCommitIdentity
		}
		if cp.Phase != "retirement_scheduled" && cp.Phase != "absent" {
			return fmt.Errorf("media: version storage requires explicit MigrateVersionStorage for %s", source.Ref())
		}
	}
	return nil
}

func (j *Jobs) proveAbsentVersionStorage(ctx context.Context, source Item, opt VersionStorageOptions) error {
	jr := j.cfg.Journal.forTenant(source.Ref().TenantID)
	if err := layoutRootQuiet(ctx, jr, source.Ref().TenantID); err != nil {
		return err
	}
	if err := jr.checkFolderOwner(ctx, jr.pool, source, true); err != nil {
		return err
	}
	// A missing manifest is not a deletion. The newest applied mutation must
	// prove deletion, with no later reset, and the owning catalog must separately
	// prove permanent removal. Notification retries change updated_at, not the
	// insertion order. Tied last mutations must all agree on deletion.
	rows, err := jr.pool.Query(ctx, `SELECT effects FROM (
SELECT effects,rank() OVER (ORDER BY created_at DESC) AS newest FROM `+jr.table+`
WHERE tenant_id=$1 AND folder_prefix=$2 AND state='applied'
AND effects->>'cleanup' IS DISTINCT FROM 'true'
AND NOT effects ? 'version_storage_plan') AS history WHERE newest=1`, source.Ref().TenantID, source.Prefix())
	if err != nil {
		return err
	}
	latest, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return err
	}
	for _, body := range latest {
		var effects journalEffects
		if err := json.Unmarshal(body, &effects); err != nil {
			return err
		}
		if !effects.Deletion || opt.VersionPurged == nil {
			return errors.New("media: committed source manifest is missing without permanent catalog deletion proof")
		}
	}
	if len(latest) > 0 {
		purged, err := opt.VersionPurged(ctx, source.Ref())
		if err != nil {
			return err
		}
		if !purged {
			return errors.New("media: committed source manifest is missing while its catalog version still exists")
		}
	}
	var live, recent bool
	err = jr.pool.QueryRow(ctx, `SELECT
EXISTS(SELECT 1 FROM `+jr.allocations+` WHERE tenant_id=$1 AND folder_prefix=$2 AND retired_at IS NULL),
EXISTS(SELECT 1 FROM `+jr.allocations+` WHERE tenant_id=$1 AND folder_prefix=$2 AND greatest(created_at,coalesce(retired_at,created_at))>$3)`, source.Ref().TenantID, source.Prefix(), j.cfg.Now().Add(-j.cfg.LateUploadWindow)).Scan(&live, &recent)
	if err != nil {
		return err
	}
	if live {
		return errors.New("media: missing source manifest still has live allocations")
	}
	if recent {
		return errors.New("media: absent folder still has a possible in-flight upload")
	}
	return nil
}

func (j *Jobs) retireAbsentVersionStorage(ctx context.Context, source Item, riverSchema string) error {
	// The absence was checked before recording its terminal checkpoint. Recheck
	// while retrying cleanup; never apply that decision to a later arriving file.
	for _, err := range j.cfg.Store.List(ctx, source.Prefix()) {
		if err != nil {
			return err
		}
		return ErrLayoutChanged
	}
	jr := j.cfg.Journal.forTenant(source.Ref().TenantID)
	return pgx.BeginFunc(ctx, jr.pool, func(tx pgx.Tx) error {
		var running bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+legacyCleanupTable(riverSchema)+` WHERE kind IN('contentkit_media_delete_folder','contentkit_media_sweep') AND args->>'prefix'=$1 AND state='running')`, source.Prefix()).Scan(&running); err != nil {
			return err
		}
		if running {
			return errors.New("media: stop old version workers before completing absent storage migration")
		}
		_, err := tx.Exec(ctx, `UPDATE `+legacyCleanupTable(riverSchema)+` SET state='cancelled',finalized_at=now() WHERE kind IN('contentkit_media_delete_folder','contentkit_media_sweep') AND args->>'prefix'=$1 AND state IN('available','pending','retryable','scheduled')`, source.Prefix())
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM `+jr.cleanup+` WHERE tenant_id=$1 AND folder_prefix=$2`, source.Ref().TenantID, source.Prefix())
		return err
	})
}

// A dedicated session serializes explicit migrations without occupying a pool
// connection while large objects are copied. It starts no background work.
func lockVersionStorage(ctx context.Context, journal *PGJournal, reg *Registry) (func(), error) {
	seen := map[string]bool{}
	for _, k := range reg.all {
		if k.Versions {
			seen[k.ns] = true
		}
	}
	if len(seen) == 0 {
		return func() {}, nil
	}
	conn, err := pgx.ConnectConfig(ctx, journal.pool.Config().ConnConfig.Copy())
	if err != nil {
		return nil, err
	}
	close := func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(c)
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, "contentkit-version-storage:"+key); err != nil {
			close()
			return nil, err
		}
	}
	return close, nil
}
