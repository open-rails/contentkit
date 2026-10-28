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

// ProcessingPublisher wakes the accepted release from a processor's completion
// transaction, including stock external workers. It preserves an unreleased
// edition's requested date and a follow-up when an older snapshot job is running.
func ProcessingPublisher(pool *pgxpool.Pool, riverSchema, contentSchema string, registry *mediastore.Registry) (func(context.Context, pgx.Tx, contentref.ContentRef) error, error) {
	if pool == nil || contentSchema == "" || registry == nil {
		return nil, errors.New("versions: processing publisher needs pool, content schema and registry")
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: riverSchema})
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, tx pgx.Tx, ref contentref.ContentRef) error {
		k, err := registry.Kind(ref.ContentKind)
		if err != nil {
			return err
		}
		if !k.Versions || ref.Version() == "" {
			return nil
		}
		if _, err := registry.Item(ref); err != nil {
			return err
		}
		catalog := k.NS()
		if scope, scoped := registry.Scope(); scoped {
			if err := tx.QueryRow(ctx, `SELECT catalog_key FROM `+pgxIdent(contentSchema, "content_app_bindings")+`
 WHERE tenant_id=$1::uuid AND app_id=$2::uuid`, scope.TenantID, scope.AppID).Scan(&catalog); err != nil {
				return err
			}
		}
		service := publicationService(pool, contentSchema, catalog, registry)
		service.client = client
		return service.wakePublicationTx(ctx, tx, versions.Target{Content: ref.ContentID, Version: ref.Version()})

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
