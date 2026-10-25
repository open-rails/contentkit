// Seed real nested media through the last selected public pre-migration
// release. No legacy worker runs, and no application row is fabricated.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	medias3 "github.com/open-rails/contentkit/media/s3"
	"github.com/open-rails/contentkit/media/workqueue"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
)

const payload = "Real ContentKit v0.112.1 nested version media.\n"

func main() {
	ctx := context.Background()
	dsn, schema := os.Getenv("CONTENTKIT_CRITICAL_PG"), os.Getenv("CONTENTKIT_LEGACY_MEDIA_SCHEMA")
	if dsn == "" || schema == "" {
		log.Fatal("owned disposable database and schema required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	check(err)
	defer pool.Close()
	roles := authkit.NewRoles()
	roles.Root.Declare(access.Names(access.Permissions()...)...)
	auth, err := authkit.New(ctx, authkit.Config{Database: authkit.DatabaseConfig{Schema: schema + "_auth", RiverSchema: schema + "_auth"}, Token: authkit.TokenConfig{Issuer: "https://legacy-media.critical.invalid", IssuedAudiences: []string{"critical-legacy"}}, Keys: authkit.KeysConfig{AllowEphemeralDevKeys: true}, Roles: roles}, authkit.Deps{Postgres: pool})
	check(err)
	defer auth.Close(ctx)
	store, err := medias3.New(medias3.Config{Bucket: os.Getenv("CONTENTKIT_LEGACY_MEDIA_BUCKET"), Endpoint: os.Getenv("CONTENTKIT_CRITICAL_S3_ENDPOINT"), AccessKeyID: os.Getenv("CONTENTKIT_CRITICAL_S3_USER"), SecretAccessKey: os.Getenv("CONTENTKIT_CRITICAL_S3_PASSWORD"), UsePathStyle: true})
	check(err)
	cfg := ck.Config{Tenant: "previous-media", Database: ck.DatabaseConfig{Schema: schema}, Content: kinds.Config{Languages: []string{"en"}, Kinds: map[string]kinds.Kind{"article": {Route: "articles", Presentation: "post", Versions: kinds.VersionRules{Languages: "one"}}}}, Media: &ck.MediaConfig{Config: media.Config{BaseURL: os.Getenv("CONTENTKIT_LEGACY_MEDIA_URL"), Kinds: []media.Kind{{Name: "article", KeepOriginals: true, ServeOriginals: true, Uploads: []media.Upload{{Path: "files/{name}", Types: []string{"text/plain"}, MaxBytes: 1024}}}}}, WorkerSchema: schema + "_media_worker", Delivery: ck.DeliveryConfig{Mode: string(media.DeliverURL), SigningKey: os.Getenv("CONTENTKIT_LEGACY_MEDIA_SIGNING"), TTL: time.Hour, Window: time.Second}}}
	engine, err := ck.New(ctx, cfg, ck.Deps{Postgres: pool, Auth: auth.Authenticator(), Store: store})
	check(err)
	defer engine.Close(ctx)
	// Bind the released host's enqueue-only contribution. Publishing needs its
	// producer, but no old River client is started or consumes a legacy job.
	_, err = riverhelpers.New(ctx, pool, &river.Config{Schema: schema + "_river"}, engine.RiverJobs())
	check(err)
	item, err := engine.Client().PostContents(ctx, catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: "Nested media from public release", Body: "The logical content identity survives its physical migration."}}})
	check(err)
	if len(item.Versions) != 1 {
		log.Fatal("legacy item did not create its version")
	}
	ref := media.RefBody{Kind: "article", ID: item.ID, Version: item.Versions[0].ID}
	_, err = engine.Client().IngestMedia(ctx, nil, ref, uuid.NewString(), []ck.MediaFile{{Path: "files/original.txt", Type: "text/plain", Body: bytes.NewReader([]byte(payload))}})
	check(err)
	// Finish placement via the released public component API. The fixture
	// does not start an old River worker or resurrect any retired job kind.
	config, err := cfg.MediaKinds()
	check(err)
	registry, err := media.NewRegistry(config)
	check(err)
	queue, err := workqueue.New(pool, registry, cfg.Media.WorkerSchema)
	check(err)
	journal, err := media.NewPGJournal(pool, schema, queue)
	check(err)
	manifests, err := media.NewManifests(store, registry, media.ManifestOptions{Locker: media.PGLocker(pool), Journal: journal})
	check(err)
	logical, err := registry.VersionRef(ref.Kind, ref.ID, ref.Version)
	check(err)
	placed, err := manifests.Place(ctx, logical)
	check(err)
	if placed != 1 {
		log.Fatalf("placed %d real legacy files, need1", placed)
	}
	item, err = engine.Client().PostContentPublish(ctx, item.ID, catalog.PublishInput{})
	check(err)
	read, err := engine.Client().GetMediaVersion(ctx, ref.Kind, ref.ID, ref.Version, ck.GetMediaVersionQuery{Editor: true})
	check(err)
	if len(read.Files) != 1 || read.Files[0].Staged || read.Files[0].URL == "" {
		log.Fatalf("legacy media not readable: %+v", read)
	}
	version := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/open-rails/contentkit" && dep.Replace == nil {
				version = dep.Version
			}
		}
	}
	check(json.NewEncoder(os.Stdout).Encode(struct {
		Version string           `json:"version"`
		Content catalog.Content  `json:"content"`
		Ref     media.RefBody    `json:"ref"`
		Read    media.ReadResult `json:"read"`
		Payload string           `json:"payload"`
	}{version, item, ref, read, payload}))
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
