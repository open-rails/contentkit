// Produce the real deployed schema and data through public ContentKit0.126.1.
// No old workers run; exact released periodic envelopes are admitted by River.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"runtime/debug"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/entities"
	"github.com/open-rails/contentkit/kinds"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

const tenant = "previous-entity-jobs"

type projection struct {
	Windows bool `json:"windows,omitempty"`
}

func (projection) Kind() string { return "contentkit_entity_projection" }

type fixture struct {
	Version                         string
	Contents                        []catalog.Content
	Entities                        []entities.Entity
	Dated                           []entities.Membership
	Projection, Foreign, Historical int64
}

func model() kinds.Config {
	return kinds.Config{Languages: []string{"en"}, EntityKinds: map[string]kinds.EntityKind{
		"tag": {Route: "tags"}, "rail": {Route: "rails", Windows: true},
	}, Kinds: map[string]kinds.Kind{"article": {Route: "articles", Presentation: "post", Entities: []string{"tag", "rail"}, Versions: kinds.VersionRules{Languages: "one", Hidden: true}}}}
}

func main() {
	ctx := context.Background()
	dsn, schema := os.Getenv("CONTENTKIT_CRITICAL_PG"), os.Getenv("CONTENTKIT_PREVIOUS_ENTITY_SCHEMA")
	if dsn == "" || schema == "" {
		log.Fatal("owned disposable database and schema are required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	check(err)
	defer pool.Close()
	store, err := entities.New(entities.Options{Pool: pool, Schema: schema, Tenant: tenant, Kinds: model()})
	check(err)
	if os.Getenv("CONTENTKIT_PREVIOUS_ENTITY_ACTION") == "remove-dated" {
		var old fixture
		check(json.NewDecoder(os.Stdin).Decode(&old))
		// Explicitly remove the two obsolete scheduling decisions through the old
		// public API. Never silently turn future/expired associations permanent.
		check(store.RemoveMemberships(ctx, old.Dated))
		check(json.NewEncoder(os.Stdout).Encode(map[string]int{"removed": len(old.Dated)}))
		return
	}
	roles := authkit.NewRoles()
	roles.Root.Declare(access.Names(access.Permissions()...)...)
	auth, err := authkit.New(ctx, authkit.Config{Database: authkit.DatabaseConfig{Schema: schema + "_auth", RiverSchema: schema + "_auth"}, Token: authkit.TokenConfig{Issuer: "https://previous-entities.invalid", IssuedAudiences: []string{"previous-entities"}}, Keys: authkit.KeysConfig{AllowEphemeralDevKeys: true}, Roles: roles}, authkit.Deps{Postgres: pool})
	check(err)
	defer auth.Close(ctx)
	engine, err := ck.New(ctx, ck.Config{Tenant: tenant, Database: ck.DatabaseConfig{Schema: schema}, Content: model()}, ck.Deps{Postgres: pool, Auth: auth.Authenticator()})
	check(err)
	defer engine.Close(ctx)
	client := engine.Client()
	var out fixture
	out.Entities, err = client.PostEntities(ctx, []entities.EntityInput{
		{Kind: "tag", Slug: "ordinary", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Ordinary tag"}}},
		{Kind: "rail", Slug: "future", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Future rail"}}},
		{Kind: "rail", Slug: "expired", Names: []entities.Name{{Language: "en", Kind: "name", Name: "Expired rail"}}},
	})
	check(err)
	for _, title := range []string{"Timeless member", "Future member", "Expired member"} {
		item, err := client.PostContents(ctx, catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: title, Body: "Retained public manuscript: " + title}}})
		check(err)
		item, err = client.PostContentPublish(ctx, item.ID, catalog.PublishInput{})
		check(err)
		out.Contents = append(out.Contents, item)
	}
	_, err = client.PostEntityMembers(ctx, string(out.Entities[0].ID), []entities.Membership{{ContentID: out.Contents[0].ID}})
	check(err)
	future, expired := time.Now().Add(24*time.Hour), time.Now().Add(-24*time.Hour)
	out.Dated = []entities.Membership{
		{EntityID: out.Entities[1].ID, ContentID: out.Contents[1].ID, ActiveFrom: &future},
		{EntityID: out.Entities[2].ID, ContentID: out.Contents[2].ID, ActiveUntil: &expired},
	}
	check(store.AddMemberships(ctx, out.Dated))
	check(store.Recount(ctx)) // The released implementation genuinely populated its old count table.
	queue, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: schema + "_river"})
	check(err)
	insert := func(name string) int64 {
		job, err := queue.Insert(ctx, projection{}, &river.InsertOpts{Queue: name})
		check(err)
		return job.Job.ID
	}
	out.Projection, out.Foreign, out.Historical = insert("contentkit"), insert("another-app"), insert("contentkit")
	_, err = queue.JobCancel(ctx, out.Historical)
	check(err)
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/open-rails/contentkit" && dep.Replace == nil {
				out.Version = dep.Version
			}
		}
	}
	check(json.NewEncoder(os.Stdout).Encode(out))
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
