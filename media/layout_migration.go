package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media/layout"
)

// ErrLayoutChanged refuses a stale migration or rollback checkpoint. It never
// authorizes overwriting newer media, nor deleting the original objects.
var ErrLayoutChanged = errors.New("media: layout migration checkpoint changed")

// LayoutMapping explicitly associates a source item with its destination.
// A legacy versionless cover may map to each existing version that owns it.
// The caller must establish that ownership from catalog metadata beforehand.
type LayoutMapping struct {
	From, To contentref.ContentRef
	// SourceFolder is an optional historical nested version folder. It must
	// exactly match From's old parent/version path; arbitrary roots are refused.
	SourceFolder string `json:"source_folder,omitempty"`
	// Owner preserves a charged app/group quota owner. Account kinds default to
	// their account ID; empty for other kinds means the source was uncharged.
	Owner string `json:"owner,omitempty"`
}

// LayoutPlan is a serializable, immutable copy inventory. Keep it outside the
// media bucket. Build and apply it with all media writers/workers quiesced.
// Apply is restartable; original objects and journal rows are never removed.
type LayoutPlan struct {
	Version int           `json:"version"`
	Entries []LayoutEntry `json:"entries"`
	Roots   []LayoutRoot  `json:"roots"`
}

// LayoutRoot captures the entire writer scope, including an initially empty one.
type LayoutRoot struct{ Prefix, Partition string }

type LayoutEntry struct {
	Mapping      LayoutMapping  `json:"mapping"`
	SourceFolder string         `json:"source_folder"`
	TargetFolder string         `json:"target_folder"`
	SourceETag   string         `json:"source_etag"`
	Manifest     *Manifest      `json:"manifest"`
	Objects      []LayoutObject `json:"objects"`
	ErasureFence bool           `json:"erasure_fence,omitempty"`
}
type LayoutObject struct {
	Source, Target string
	Size           int64
	SHA256, ETag   string
}

// PlanLayoutMigration verifies source manifests and hashes every referenced
// blob, editor view and public rendition. Staged uploads, pending producers,
// unresolved journal attempts and incompatible destination kinds are refused.
// It performs no writes. It also supports explicit legacy versionless roots.
func PlanLayoutMigration(ctx context.Context, source *Manifests, target *Registry, mappings []LayoutMapping) (*LayoutPlan, error) {
	if source == nil || target == nil {
		return nil, errors.New("media: layout plan requires source and target")
	}
	for _, root := range layoutRoots(source.reg) {
		if err := layoutRootQuiet(ctx, source.journal.forTenant(root.Partition), root.Partition); err != nil {
			return nil, err
		}
	}
	plan := &LayoutPlan{Version: 1, Roots: layoutRoots(target)}
	seen := map[string]bool{}
	for _, mapping := range mappings {
		from, err := migrationSource(source.reg, mapping.From, mapping.SourceFolder)
		if err != nil {
			return nil, err
		}
		to, err := target.Item(mapping.To)
		if err != nil {
			return nil, err
		}
		if mapping.Owner == "" && to.kind.Account {
			mapping.Owner = to.Ref().ContentID
		}
		if from.Prefix() == to.Prefix() {
			return nil, errors.New("media: source and destination layout are identical")
		}
		if seen[to.Prefix()] {
			return nil, errors.New("media: duplicate migration destination")
		}
		seen[to.Prefix()] = true
		cur, etag, err := source.get(ctx, from.ManifestKey())
		if err != nil {
			return nil, err
		}
		if err = settledLayout(ctx, source, from, cur, flattening(mapping, from, to.Prefix())); err != nil {
			return nil, err
		}
		// Old stored metadata need not be a fixed point of today's normalizer.
		// Compare how both kinds interpret it without rewriting the snapshot.
		sourceNormalized, targetNormalized := cur.Clone(), cur.Clone()
		from.kind.Normalize(sourceNormalized)
		to.kind.Normalize(targetNormalized)
		if digestManifest(targetNormalized) != digestManifest(sourceNormalized) {
			return nil, errors.New("media: destination kind changes source manifest semantics")
		}
		e := LayoutEntry{Mapping: mapping, SourceFolder: from.Prefix(), TargetFolder: to.Prefix(), SourceETag: etag, Manifest: cur.Clone()}
		if cur.Deleted {
			if _, err := source.store.Head(ctx, from.PublicPrefix()+layout.ErasureFenceName); err == nil {
				e.ErasureFence = true
			} else if !errors.Is(err, ErrNotFound) {
				return nil, err
			}
		}
		for _, key := range layoutEntryObjects(from, e) {
			obj, err := hashObject(ctx, source.store, key)
			if err != nil {
				return nil, err
			}
			e.Objects = append(e.Objects, LayoutObject{Source: key, Target: to.Prefix() + strings.TrimPrefix(key, from.Prefix()), Size: obj.Size, SHA256: obj.SHA256, ETag: obj.ETag})
		}
		plan.Entries = append(plan.Entries, e)
	}
	return plan, nil
}

// ApplyLayoutMigration copies and verifies each immutable object, then commits
// its manifest and allocation/publication journal atomically through the normal
// media protocol. Existing different bytes or a changed source stop the copy.
// Completed entries are checked and skipped on retry, never overwritten.
func ApplyLayoutMigration(ctx context.Context, source *Manifests, target *Jobs, plan *LayoutPlan) error {
	if source == nil || target == nil || plan == nil || plan.Version != 1 {
		return errors.New("media: invalid layout migration")
	}
	if !slices.Equal(plan.Roots, layoutRoots(target.cfg.Registry)) {
		return ErrLayoutChanged
	}

	for _, e := range plan.Entries {
		if e.Manifest == nil {
			return ErrLayoutChanged
		}
		from, err := migrationSource(source.reg, e.Mapping.From, e.Mapping.SourceFolder)
		if err != nil {
			return err
		}
		to, err := target.cfg.Registry.Item(e.Mapping.To)
		if err != nil {
			return err
		}
		if from.Prefix() != e.SourceFolder || to.Prefix() != e.TargetFolder || e.Manifest == nil {
			return ErrLayoutChanged
		}
		if err = verifyLayoutSource(ctx, source, from, e); err != nil {
			return err
		}
		encoded, err := json.Marshal(e)
		if err != nil {
			return err
		}
		fingerprint := sha256.Sum256(encoded)
		commit := manifestCommit{ID: uuid.NewSHA1(uuid.NameSpaceURL, fingerprint[:]), Ref: to.Ref(), Folder: to.Prefix(),
			Actor: "migration:layout", Fingerprint: fingerprint}
		prior, err := target.cfg.Journal.existing(ctx, commit)
		if err != nil {
			return err
		}
		if prior != nil {
			if err := target.manifests.Recover(ctx, to.Ref()); err != nil {
				return err
			}
			prior, err = target.cfg.Journal.existing(ctx, commit)
			if err != nil {
				return err
			}
		}
		cur, currentETag, err := target.manifests.get(ctx, to.ManifestKey())
		resumeETag := ""
		if err == nil {
			if digestManifest(cur) != digestManifest(e.Manifest) {
				// A failed first PUT is recovered by writing an empty fence.
				// Only this exact migration's absent attempt may replace it.
				empty := &Manifest{V: ManifestVersion, Incarnation: layoutRecoveryIncarnation(commit.ID), Files: []File{}}
				if prior == nil || prior.State != "absent" || cur.Receipt != nil || digestManifest(cur) != digestManifest(empty) {
					return fmt.Errorf("%w: destination %s", ErrLayoutChanged, to.Prefix())
				}
				resumeETag = currentETag
			} else {
				if err = settledLayout(ctx, target.manifests, to, cur, flattening(e.Mapping, from, to.Prefix())); err != nil {
					return err
				}
				for _, o := range e.Objects {
					if err = verifyLayoutObject(ctx, target.cfg.Store, o.Target, o); err != nil {
						return err
					}
				}
				continue
			}
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		allowed := map[string]bool{}
		if resumeETag != "" {
			allowed[to.ManifestKey()] = true
		}
		for _, o := range e.Objects {
			allowed[o.Target] = true
		}
		for obj, err := range target.cfg.Store.List(ctx, to.Prefix()) {
			if err != nil {
				return err
			}
			parsed, ok := layout.Parse(obj.Key)
			if ok && parsed.Folder() != to.Prefix() {
				continue
			}
			if !allowed[obj.Key] {
				return fmt.Errorf("%w: unexpected destination object %s", ErrLayoutChanged, obj.Key)
			}
		}
		if e.ErasureFence && !e.Manifest.Deleted {
			return ErrLayoutChanged
		}
		expected := layoutEntryObjects(from, e)
		if len(expected) != len(e.Objects) {
			return ErrLayoutChanged
		}
		for i, o := range e.Objects {
			if o.Source != expected[i] || o.Target != to.Prefix()+strings.TrimPrefix(o.Source, from.Prefix()) {
				return ErrLayoutChanged
			}
			if err = copyLayoutObject(ctx, source.store, target.cfg.Store, o); err != nil {
				return err
			}
		}
		if err = verifyLayoutSource(ctx, source, from, e); err != nil {
			return err
		}
		mutation := &manifestMutation{lifecycle: true, layoutCopy: true, commit: commit}
		_, err = target.manifests.editOperation(ctx, to.Ref(), false, bound{}, mutation, func(cur *Manifest) error {
			if head, err := target.cfg.Store.Head(ctx, to.ManifestKey()); err == nil {
				if resumeETag == "" || head.ETag != resumeETag {
					return ErrLayoutChanged
				}
			} else if !errors.Is(err, ErrNotFound) {
				return err
			} else if resumeETag != "" {
				return ErrLayoutChanged
			}
			if len(cur.Files) > 0 || cur.Deleted {
				return ErrLayoutChanged
			}
			next := e.Manifest.Clone()
			next.Receipt = nil
			keys := make([]string, 0, len(e.Objects))
			for _, o := range e.Objects {
				if k, ok := layout.Parse(o.Target); ok && (k.Area == layout.AreaPrivate || k.Area == layout.AreaDownload) {
					keys = append(keys, o.Target)
				}
			}
			if err := target.cfg.Journal.allocateLayout(ctx, mutation.commit, next.Incarnation, keys); err != nil {
				return err
			}
			mutation.effects.Incarnation = next.Incarnation
			delta := next.uploadBytes()
			if flattening(e.Mapping, from, to.Prefix()) {
				// Moving the same logical version does not create more owned
				// content. Its old allocation cleanup must not refund it either.
				delta = 0
			}
			mutation.effects.Settlement = Settlement{Tenant: to.Ref().TenantID, Owner: e.Mapping.Owner, Delta: delta}
			*cur = *next
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// The journal operation and a separate UUID domain bind a missing-target
// recovery fence to this migration, even if the process dies before retrying.
func layoutRecoveryIncarnation(operation uuid.UUID) string {
	return uuid.NewSHA1(operation, []byte("contentkit:layout-migration:recovery-fence")).String()
}

// Retrying the same copy can reuse only its same-incarnation, still-live
// allocations. Retired keys and different owners never become live again.
func (j *PGJournal) allocateLayout(ctx context.Context, c manifestCommit, incarnation string, keys []string) error {
	j = j.forTenant(c.Ref.TenantID)
	return pgx.BeginFunc(ctx, j.pool, func(tx pgx.Tx) error {
		found, err := j.load(ctx, tx, c.Ref.TenantID, c.ID)
		if err != nil {
			return err
		}
		if found.Lease != c.Lease || found.State != "open" {
			return ErrCommitPending
		}
		var count int
		err = tx.QueryRow(ctx, `WITH saved AS (
INSERT INTO `+j.allocations+` AS a(tenant_id,folder_prefix,object_key,incarnation)
SELECT $1,$2,key,$3 FROM unnest($4::text[]) AS key
ON CONFLICT(tenant_id,object_key) DO UPDATE SET incarnation=EXCLUDED.incarnation
WHERE a.folder_prefix=EXCLUDED.folder_prefix AND a.incarnation=EXCLUDED.incarnation AND a.retired_at IS NULL
RETURNING 1) SELECT count(*) FROM saved`, c.Ref.TenantID, c.Folder, incarnation, keys).Scan(&count)
		if err != nil {
			return err
		}
		if count != len(keys) {
			return ErrAllocationRetired
		}
		return nil
	})
}

// VerifyLayoutRollback is a read-only gate for switching back to the preserved
// legacy layout. It refuses post-copy edits, new folders, outstanding attempts
// and even new unreferenced objects. If it fails, create a reverse-copy plan;
// never just flip LegacyLayout and hide media written after the checkpoint.
func VerifyLayoutRollback(ctx context.Context, source *Manifests, target *Jobs, plan *LayoutPlan) error {
	if source == nil || target == nil || plan == nil || plan.Version != 1 {
		return errors.New("media: invalid rollback checkpoint")
	}
	if !slices.Equal(plan.Roots, layoutRoots(target.cfg.Registry)) {
		return ErrLayoutChanged
	}

	expected := map[string]bool{}
	for _, e := range plan.Entries {
		if e.Manifest == nil {
			return ErrLayoutChanged
		}
		from, err := migrationSource(source.reg, e.Mapping.From, e.Mapping.SourceFolder)
		if err != nil {
			return err
		}
		if err = verifyLayoutSource(ctx, source, from, e); err != nil {
			return err
		}
		expected[from.ManifestKey()] = true
		for _, object := range e.Objects {
			expected[object.Source] = true
		}
		to, err := target.cfg.Registry.Item(e.Mapping.To)
		if err != nil {
			return err
		}
		cur, _, err := target.manifests.get(ctx, to.ManifestKey())
		if err != nil {
			return err
		}
		if digestManifest(cur) != digestManifest(e.Manifest) {
			return fmt.Errorf("%w: post-copy edit in %s", ErrLayoutChanged, to.Prefix())
		}
		if err = settledLayout(ctx, target.manifests, to, cur, flattening(e.Mapping, from, to.Prefix())); err != nil {
			return err
		}
		if err = layoutRootQuiet(ctx, target.manifests.journal.forTenant(to.Ref().TenantID), to.Ref().TenantID); err != nil {
			return err
		}
		expected[to.ManifestKey()] = true
		for _, o := range e.Objects {
			expected[o.Target] = true
			if err = verifyLayoutObject(ctx, target.cfg.Store, o.Target, o); err != nil {
				return err
			}
		}
	}
	for _, root := range plan.Roots {
		if err := layoutRootQuiet(ctx, target.manifests.journal.forTenant(root.Partition), root.Partition); err != nil {
			return err
		}
		for obj, err := range target.cfg.Store.List(ctx, root.Prefix) {
			if err != nil {
				return err
			}
			if k, ok := layout.Parse(obj.Key); ok && k.ID == layout.DefaultID && k.Area == layout.AreaPublic {
				continue
			}
			if !expected[obj.Key] {
				return fmt.Errorf("%w: new object %s", ErrLayoutChanged, obj.Key)
			}
		}
	}
	return nil
}
func migrationSource(reg *Registry, ref contentref.ContentRef, sourceFolder string) (Item, error) {
	item, err := reg.Item(ref)
	if err == nil {
		if sourceFolder != "" && sourceFolder != item.Prefix() {
			if !item.Kind().Versions || ref.Version() == "" || sourceFolder != item.Kind().nestedPrefix(ref.ContentID, ref.Version()) {
				return Item{}, ErrLayoutChanged
			}
			item.prefix = sourceFolder
		}
		return item, nil
	}
	k, e := reg.Kind(ref.ContentKind)
	if e != nil || !reg.cfg.Scope.IsZero() || !k.Versions || ref.Version() != "" || ref.TenantID != k.ns || contentref.ValidateID(ref.ContentID) != nil {
		return Item{}, err
	}
	prefix := k.nestedPrefix(ref.ContentID, "")
	if sourceFolder != "" && sourceFolder != prefix {
		return Item{}, ErrLayoutChanged
	}
	return Item{ref: ref, kind: k, prefix: prefix}, nil
}
func flattening(mapping LayoutMapping, source Item, targetPrefix string) bool {
	return mapping.From.Equal(mapping.To) && mapping.From.Version() != "" &&
		mapping.SourceFolder == source.Kind().nestedPrefix(mapping.From.ContentID, mapping.From.Version()) &&
		source.Prefix() != targetPrefix
}

func settledLayout(ctx context.Context, m *Manifests, item Item, cur *Manifest, allowDeleted bool) error {
	if cur == nil || cur.V != ManifestVersion || cur.Deleted && !allowDeleted || cur.Full || cur.Incarnation == "" {
		return errors.New("media: source must be an upgraded live manifest")
	}
	if err := m.journal.checkReceipt(ctx, item, cur.Receipt); err != nil {
		return err
	}
	for _, f := range cur.Files {
		if f.Staged != "" || len(f.Pending) > 0 {
			return ErrCommitPending
		}
		for _, p := range f.Public {
			if !p.Ready() {
				return ErrCommitPending
			}
		}
	}
	j := m.journal.forTenant(item.Ref().TenantID)
	var busy bool
	err := j.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+j.table+" WHERE tenant_id=$1 AND folder_prefix=$2 AND state IN ('open','prepared','frozen'))", item.Ref().TenantID, item.Prefix()).Scan(&busy)
	if err != nil {
		return err
	}
	if busy {
		return ErrCommitPending
	}
	return nil
}
func digestManifest(m *Manifest) string {
	copy := m.Clone()
	copy.Receipt = nil
	b, _ := json.Marshal(copy)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func layoutObjects(item Item, m *Manifest) []string {
	keys := map[string]bool{}
	for _, blob := range m.Blobs() {
		key, _ := item.Blob(blob)
		keys[key] = true
	}
	for _, f := range m.Files {
		for _, p := range f.Public {
			for _, name := range p.NamesOnDisk() {
				key, _ := item.Public(name)
				keys[key] = true
			}
			if spec := item.kind.public(p.Preset); spec != nil && spec.Stable {
				for _, name := range p.Names {
					key, _ := item.Public(name)
					keys[key] = true
				}
			}
		}
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}
func layoutEntryObjects(item Item, entry LayoutEntry) []string {
	keys := layoutObjects(item, entry.Manifest)
	if entry.ErasureFence {
		keys = append(keys, item.PublicPrefix()+layout.ErasureFenceName)
		slices.Sort(keys)
	}
	return keys
}

func verifyLayoutSource(ctx context.Context, m *Manifests, item Item, e LayoutEntry) error {
	cur, etag, err := m.get(ctx, item.ManifestKey())
	if err != nil {
		return err
	}
	if etag != e.SourceETag || digestManifest(cur) != digestManifest(e.Manifest) {
		return fmt.Errorf("%w: source %s", ErrLayoutChanged, item.Prefix())
	}
	return settledLayout(ctx, m, item, cur, flattening(e.Mapping, item, e.TargetFolder))
}
func hashObject(ctx context.Context, s Store, key string) (LayoutObject, error) {
	r, obj, err := s.Get(ctx, key, GetOptions{})
	if err != nil {
		return LayoutObject{}, err
	}
	defer r.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, r)
	if err == nil && size != obj.Size {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return LayoutObject{}, err
	}
	return LayoutObject{Size: size, SHA256: hex.EncodeToString(hash.Sum(nil)), ETag: obj.ETag}, nil
}
func verifyLayoutObject(ctx context.Context, s Store, key string, want LayoutObject) error {
	got, err := hashObject(ctx, s, key)
	if err != nil {
		return err
	}
	if got.Size != want.Size || got.SHA256 != want.SHA256 {
		return fmt.Errorf("%w: bytes at %s", ErrLayoutChanged, key)
	}
	return nil
}
func copyLayoutObject(ctx context.Context, source, target Store, o LayoutObject) error {
	if _, err := target.Head(ctx, o.Target); err == nil {
		return verifyLayoutObject(ctx, target, o.Target, o)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	r, obj, err := source.Get(ctx, o.Source, GetOptions{})
	if err != nil {
		return err
	}
	defer r.Close()
	if obj.ETag != o.ETag {
		return ErrLayoutChanged
	}
	tmp, err := os.CreateTemp("", "contentkit-layout-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), r)
	if err != nil {
		return err
	}
	if n != o.Size || hex.EncodeToString(hash.Sum(nil)) != o.SHA256 {
		return ErrLayoutChanged
	}
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = target.Put(ctx, o.Target, tmp, n, PutOptions{ContentType: obj.ContentType, CacheControl: obj.CacheControl, Metadata: obj.Metadata, ChecksumSHA256: hash.Sum(nil), IfNoneMatch: "*"})
	if err != nil && !errors.Is(err, ErrPreconditionFailed) {
		return err
	}
	return verifyLayoutObject(ctx, target, o.Target, o)
}

func layoutRootQuiet(ctx context.Context, j *PGJournal, partition string) error {
	var busy bool
	err := j.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+j.table+" WHERE tenant_id=$1 AND state IN ('open','prepared','frozen')) OR EXISTS(SELECT 1 FROM "+j.limiter.res+" WHERE tenant_id=$1 AND expires_at>now())", partition).Scan(&busy)
	if err != nil {
		return err
	}
	if busy {
		return ErrCommitPending
	}
	return nil
}

func layoutRoots(reg *Registry) []LayoutRoot {
	seen := map[string]bool{}
	var roots []LayoutRoot
	for _, kind := range reg.all {
		root := kind.root()
		if !seen[root] {
			roots = append(roots, LayoutRoot{Prefix: root, Partition: kind.ns})
			seen[root] = true
		}
	}
	slices.SortFunc(roots, func(a, b LayoutRoot) int { return strings.Compare(a.Prefix, b.Prefix) })
	return roots
}
