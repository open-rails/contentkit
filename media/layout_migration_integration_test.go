package media_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/layout"
)

func scopedFixture(t *testing.T, env *s3test.Env, scope layout.Scope) *fixture {
	return newFixtureOn(t, env, func(c *media.Config) {
		c.Namespace = ""
		c.Scope = scope
		for i := range c.Kinds {
			if c.Kinds[i].Namespace != "" {
				c.Kinds[i].Namespace = ""
				c.Kinds[i].Account = true
			}
		}
	})
}
func TestScopedMediaIsolatesTwoTenantsAndApps(t *testing.T) {
	env := s3test.Open(t)
	tenants := []string{uuid.NewString(), uuid.NewString()}
	apps := []string{uuid.NewString(), uuid.NewString()}
	var fixtures []*fixture
	for _, tenant := range tenants {
		for _, app := range apps {
			f := scopedFixture(t, env, layout.Scope{TenantID: tenant, AppID: app})
			fixtures = append(fixtures, f)
			ref := f.ref("gallery", 1)
			f.visible(1)
			f.put(ref, "cover", "image/png", png(len(fixtures)))
			f.produce(ref)
		}
	}
	for i, f := range fixtures {
		ref := f.ref("gallery", 1)
		url := f.reg.PublicURL(ref, f.publicName(ref, "cover-230.webp"))
		if !strings.Contains(url, "/tenant/"+tenants[i/2]+"/app/"+apps[i%2]+"/") {
			t.Fatal(url)
		}
		if code, _, _ := f.fetch(url); code != 200 {
			t.Fatalf("own URL %d", code)
		}
		for j, other := range fixtures {
			if i != j {
				if code, _, _ := other.fetch(url); code != 404 {
					t.Fatalf("foreign tenant/app served %d -> %d: %d", i, j, code)
				}
			}
		}
	}
	// Two apps under one tenant resolve the same account avatar path.
	a, _ := fixtures[0].reg.Ref("user", cid(99))
	b, _ := fixtures[1].reg.Ref("user", cid(99))
	c, _ := fixtures[2].reg.Ref("user", cid(99))
	ai, _ := fixtures[0].reg.Item(a)
	bi, _ := fixtures[1].reg.Item(b)
	ci, _ := fixtures[2].reg.Item(c)
	if ai.Prefix() != bi.Prefix() || ai.Prefix() == ci.Prefix() {
		t.Fatal("account media ownership is not tenant-wide")
	}
}

type stopCopyStore struct {
	media.Store
	left int
}

func (s *stopCopyStore) Put(ctx context.Context, key string, r io.Reader, n int64, o media.PutOptions) (media.Object, error) {
	if s.left == 0 {
		return media.Object{}, errors.New("injected interrupted copy")
	}
	s.left--
	return s.Store.Put(ctx, key, r, n, o)
}
func TestLayoutMigrationResumesAndRefusesStaleRollback(t *testing.T) {
	source := newFixture(t)
	ref := source.ref("gallery", 1)
	source.visible(1)
	source.put(ref, "cover", "image/png", png(1))
	source.produce(ref)
	target := scopedFixture(t, s3test.Open(t), layout.Scope{TenantID: uuid.NewString(), AppID: uuid.NewString()})
	to := target.ref("gallery", 1)
	target.visible(1)
	plan, err := media.PlanLayoutMigration(t.Context(), source.ms, target.reg, []media.LayoutMapping{{From: ref, To: to}})
	if err != nil {
		t.Fatal(err)
	}
	broken, err := media.NewJobs(media.JobsConfig{Store: &stopCopyStore{Store: target.env.Store, left: 1}, Registry: target.reg, Locker: s3test.Locker(t, target.env.Store), Journal: target.journal, Processes: target.q})
	if err != nil {
		t.Fatal(err)
	}
	if err = media.ApplyLayoutMigration(t.Context(), source.ms, broken, plan); err == nil {
		t.Fatal("copy interruption did not surface")
	}
	if err = media.ApplyLayoutMigration(t.Context(), source.ms, target.jobs, plan); err != nil {
		t.Fatal(err)
	}
	if err = media.ApplyLayoutMigration(t.Context(), source.ms, target.jobs, plan); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if err = media.VerifyLayoutRollback(t.Context(), source.ms, target.jobs, plan); err != nil {
		t.Fatal(err)
	}
	for _, e := range plan.Entries {
		for _, o := range e.Objects {
			src, _, err := source.env.Store.Get(t.Context(), o.Source, media.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			sb, _ := io.ReadAll(src)
			src.Close()
			dst, _, err := target.env.Store.Get(t.Context(), o.Target, media.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			db, _ := io.ReadAll(dst)
			dst.Close()
			if !bytes.Equal(sb, db) {
				t.Fatal("copy changed bytes")
			}
		}
	}
	target.put(to, "cover", "image/png", png(2))
	target.produce(to)
	if err = media.VerifyLayoutRollback(t.Context(), source.ms, target.jobs, plan); !errors.Is(err, media.ErrLayoutChanged) {
		t.Fatalf("rollback hid post-copy edits: %v", err)
	}
	if err = media.ApplyLayoutMigration(t.Context(), source.ms, target.jobs, plan); !errors.Is(err, media.ErrLayoutChanged) {
		t.Fatalf("replay overwrote post-copy edits: %v", err)
	}
	if _, _, err = source.ms.Get(t.Context(), ref); err != nil {
		t.Fatal("source was removed", err)
	}
}

func TestLayoutMigrationCopiesExplicitLegacyCoverVersions(t *testing.T) {
	source := newFixture(t)
	from := source.ref("gallery", 5)
	source.visible(5)
	source.put(from, "cover", "image/png", png(5))
	target := newFixtureOn(t, s3test.Open(t), func(c *media.Config) {
		c.Namespace = ""
		c.Scope = layout.Scope{TenantID: uuid.NewString(), AppID: uuid.NewString()}
		for i := range c.Kinds {
			if c.Kinds[i].Namespace != "" {
				c.Kinds[i].Namespace = ""
				c.Kinds[i].Account = true
			}
			if c.Kinds[i].Name == "gallery" {
				c.Kinds[i].Versions = true
			}
		}
	})
	first, _ := target.reg.VersionRef("gallery", cid(5), cid(6))
	second, _ := target.reg.VersionRef("gallery", cid(5), cid(7))
	maps := []media.LayoutMapping{{From: from, To: first}, {From: from, To: second}}
	if _, err := media.PlanLayoutMigration(t.Context(), source.ms, target.reg, maps); !errors.Is(err, media.ErrCommitPending) {
		t.Fatalf("pending producer accepted: %v", err)
	}
	source.produce(from)
	// The current registry knows this kind is versioned; the explicit mapping
	// is what permits reading its retired versionless item root.
	cfg := source.reg.Config()
	for i := range cfg.Kinds {
		if cfg.Kinds[i].Name == "gallery" {
			cfg.Kinds[i].Versions = true
		}
	}
	current, err := media.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := media.NewManifests(source.env.Store, current, media.ManifestOptions{Locker: s3test.Locker(t, source.env.Store), Journal: source.journal})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := media.PlanLayoutMigration(t.Context(), legacy, target.reg, maps)
	if err != nil {
		t.Fatal(err)
	}
	if err = media.ApplyLayoutMigration(t.Context(), legacy, target.jobs, plan); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []contentref.ContentRef{first, second} {
		m, _, err := target.ms.Get(t.Context(), ref)
		if err != nil || len(m.Files) != 1 {
			t.Fatalf("cover missing from explicit version: %+v %v", m, err)
		}
	}
	untouched, _ := target.reg.VersionRef("gallery", cid(5), cid(8))
	if _, _, err = target.ms.Get(t.Context(), untouched); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("unmapped version changed: %v", err)
	}
}

func TestLayoutMigrationEmptyPlanRefusesRollbackAfterFirstUpload(t *testing.T) {
	source := newFixture(t)
	target := scopedFixture(t, s3test.Open(t), layout.Scope{TenantID: uuid.NewString(), AppID: uuid.NewString()})
	plan, err := media.PlanLayoutMigration(t.Context(), source.ms, target.reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = media.VerifyLayoutRollback(t.Context(), source.ms, target.jobs, plan); err != nil {
		t.Fatal(err)
	}
	target.put(target.ref("gallery", 1), "cover", "image/png", png(1))
	if err = media.VerifyLayoutRollback(t.Context(), source.ms, target.jobs, plan); !errors.Is(err, media.ErrLayoutChanged) {
		t.Fatalf("empty checkpoint allowed new upload to disappear: %v", err)
	}
}
