package versionstore

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/contentkit/contentref"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	versions "github.com/open-rails/contentkit/media/versions"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// Recorded is the item cache's catalog hook (mediastore.CacheOptions Lock
// and Recorded) for versioned kinds: Lock takes the version's catalog rows,
// Recorded writes its media facts and wakes its release. A deleted manifest
// (a soft delete, a tombstone) needs neither, and its writer may hold those
// rows itself.
func Recorded(pool *pgxpool.Pool, riverSchema, contentSchema string, registry *mediastore.Registry) (mediastore.CatalogHook, error) {
	if pool == nil || contentSchema == "" || registry == nil {
		return mediastore.CatalogHook{}, errors.New("versions: the recorded hook needs pool, content schema and registry")
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: riverSchema})
	if err != nil {
		return mediastore.CatalogHook{}, err
	}
	service := func(ctx context.Context, tx pgx.Tx, item mediastore.Item, m *mediastore.Manifest) (*Service, versions.Target, bool, error) {
		ref, k := item.Ref(), item.Kind()
		if !k.Versions || ref.Version() == "" || m.Deleted != nil {
			return nil, versions.Target{}, false, nil
		}
		catalog := k.NS()
		if scope, scoped := registry.Scope(); scoped {
			if err := tx.QueryRow(ctx, `SELECT catalog_key FROM `+pgxIdent(contentSchema, "content_app_bindings")+`
 WHERE tenant_id=$1::uuid AND app_id=$2::uuid`, scope.TenantID, scope.AppID).Scan(&catalog); err != nil {
				return nil, versions.Target{}, false, err
			}
		}
		s := publicationService(pool, contentSchema, catalog, registry)
		s.client = client
		return s, versions.Target{Content: ref.ContentID, Version: ref.Version()}, true, nil
	}
	return mediastore.CatalogHook{
		Lock: func(ctx context.Context, tx pgx.Tx, item mediastore.Item, m *mediastore.Manifest) error {
			s, t, ok, err := service(ctx, tx, item, m)
			if !ok || err != nil {
				return err
			}
			return s.lockTarget(ctx, tx, t)
		},
		Recorded: func(ctx context.Context, tx pgx.Tx, item mediastore.Item, m *mediastore.Manifest) error {
			s, t, ok, err := service(ctx, tx, item, m)
			if !ok || err != nil {
				return err
			}
			var live bool
			err = tx.QueryRow(ctx, `SELECT (v.state='published' AND c.state='published') OR v.released FROM `+s.t.versions+` v
 JOIN `+s.t.items+` c ON c.tenant_id=v.tenant_id AND c.id=v.content_id WHERE v.tenant_id=$1 AND v.id=$2 AND v.content_id=$3`,
				s.o.Tenant, t.Version, t.Content).Scan(&live)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // the catalog no longer has it
			} else if err != nil {
				return err
			}
			if err := s.recordFacts(ctx, tx, item.Kind(), t, m); err != nil {
				return err
			}
			if !live {
				return nil
			}
			return mediastore.Wake(ctx, client, tx, releaseArgs{Content: t.Content, Version: t.Version},
				river.InsertOpts{Queue: QueueFor(s.o.Tenant), MaxAttempts: 25}, s.o.Now())
		},
	}, nil
}

// publicationService is the narrow storage/scheduling subset used by stock
// processors. It creates no authentication runtime or separate job handler.
func publicationService(pool *pgxpool.Pool, schema, tenant string, reg *mediastore.Registry) *Service {
	q := func(name string) string { return pgxIdent(schema, name) }
	return &Service{o: Options{Pool: pool, Schema: schema, Tenant: tenant, Now: time.Now}, reg: reg,
		t: tables{items: q("content_items"), versions: q("content_versions"), segments: q("content_segments"), entities: q("content_entities"), facets: q("content_facets")}}
}

// PrepareDerivatives prepares page order and automatic cover inputs inside the
// existing derivative attempt. Drafts get the same preparation without publishing.
func PrepareDerivatives(pool *pgxpool.Pool, schema string, manifests *mediastore.Manifests, uploads *mediastore.Uploads) func(context.Context, contentref.ContentRef) error {
	return func(ctx context.Context, ref contentref.ContentRef) error {
		reg := manifests.Registry()
		k, err := reg.Kind(ref.ContentKind)
		if err != nil {
			return err
		}
		if !k.Versions || ref.Version() == "" || k.Publish == nil {
			return nil
		}
		tenant := k.NS()
		if scope, ok := reg.Scope(); ok {
			if err := pool.QueryRow(ctx, `SELECT catalog_key FROM `+pgxIdent(schema, "content_app_bindings")+` WHERE tenant_id=$1::uuid AND app_id=$2::uuid`, scope.TenantID, scope.AppID).Scan(&tenant); err != nil {
				return err
			}
		}
		s := publicationService(pool, schema, tenant, reg)
		s.o.Logger = slog.Default()
		s.m = Media{Manifests: manifests, Uploads: uploads}
		t := versions.Target{Content: ref.ContentID, Version: ref.Version()}
		if err := s.preparePageSelection(ctx, k, t); err != nil && !errors.Is(err, mediastore.ErrNotFound) {
			return err
		}
		return s.applyCover(ctx, k, t)
	}
}
