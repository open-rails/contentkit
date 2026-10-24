// Seed a disposable database through the previous public release, never by
// copying an old test or approximating its schema with hand-written DDL.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"runtime/debug"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/catalog"
	"github.com/open-rails/contentkit/kinds"
)

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("CONTENTKIT_CRITICAL_PG"))
	check(err)
	defer pool.Close()
	schema := os.Getenv("CONTENTKIT_PREVIOUS_SCHEMA")
	if schema == "" {
		log.Fatal("owned previous-release schema required")
	}
	roles := authkit.NewRoles()
	roles.Root.Declare(access.Names(access.Permissions()...)...)
	auth, err := authkit.New(ctx, authkit.Config{Database: authkit.DatabaseConfig{Schema: schema + "_auth", RiverSchema: schema + "_auth"}, Token: authkit.TokenConfig{Issuer: "https://previous.critical.invalid", IssuedAudiences: []string{"critical-previous"}}, Keys: authkit.KeysConfig{AllowEphemeralDevKeys: true}, Roles: roles}, authkit.Deps{Postgres: pool})
	check(err)
	defer auth.Close(ctx)
	cfg := ck.Config{Tenant: "previous-release", Database: ck.DatabaseConfig{Schema: schema}, Content: kinds.Config{Languages: []string{"en"}, Kinds: map[string]kinds.Kind{"article": {Route: "articles", Presentation: "post", Versions: kinds.VersionRules{Languages: "one"}}}}}
	engine, err := ck.New(ctx, cfg, ck.Deps{Postgres: pool, Auth: auth.Authenticator()})
	check(err)
	defer engine.Close(ctx)
	item, err := engine.Client().PostContents(ctx, catalog.ContentInput{Kind: "article", Text: map[string]catalog.Text{"en": {Title: "Previous public release", Body: "A durable previous-release manuscript"}}})
	check(err)
	item, err = engine.Client().PostContentPublish(ctx, item.ID, catalog.PublishInput{})
	check(err)
	version := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/open-rails/contentkit" && dep.Replace == nil {
				version = dep.Version
			}
		}
	}
	check(json.NewEncoder(os.Stdout).Encode(struct {
		Version string          `json:"version"`
		Content catalog.Content `json:"content"`
	}{version, item}))
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
