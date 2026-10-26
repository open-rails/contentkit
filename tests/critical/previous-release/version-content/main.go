package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/kinds"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("CONTENTKIT_CRITICAL_PG"))
	if err != nil {
		return err
	}
	defer pool.Close()
	schema := os.Getenv("CONTENTKIT_PREVIOUS_TEXT_SCHEMA")
	if schema == "" {
		return fmt.Errorf("missing owned schema")
	}
	model := kinds.Config{Languages: []string{"en", "zh"}, Kinds: map[string]kinds.Kind{"post": {Route: "posts", Presentation: "post", Versions: kinds.VersionRules{Languages: "one"}}}}
	if err := ck.MigratePostgres(ctx, pool, ck.Config{Tenant: "version-owned-migration", Database: ck.DatabaseConfig{Schema: schema}, Content: model}); err != nil {
		return err
	}
	table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
	ids := map[string]string{"content": contentref.NewID(), "en": contentref.NewID(), "zh": contentref.NewID()}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO `+table("content_items")+`(tenant_id,id,kind,original_title,original_language) VALUES('version-owned-migration',$1,'post','原題','ja')`, ids["content"]); err != nil {
		return err
	}
	for i, lang := range []string{"en", "zh"} {
		if _, err = tx.Exec(ctx, `INSERT INTO `+table("content_versions")+`(tenant_id,id,content_id,position,languages,state,access) VALUES('version-owned-migration',$1,$2,$3,$4,'draft','{"level":"public"}')`, ids[lang], ids["content"], i, []string{lang}); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO `+table("content_text")+`(tenant_id,content_id,language,title,slug,description,body,sort_key,aliases) VALUES('version-owned-migration',$1,$2,$3,$4,$5,$6,$7,$8)`, ids["content"], lang, "Parent "+lang, "parent-"+lang, "Summary "+lang, "Body "+lang, "Order "+lang, []string{"Alias " + lang}); err != nil {
			return err
		}
	}
	// Retain translations even when no edition declares their language.
	if _, err = tx.Exec(ctx, `INSERT INTO `+table("content_text")+`(tenant_id,content_id,language,title,slug,description,body,sort_key,aliases) VALUES('version-owned-migration',$1,'ja','原題','original','元の概要','元の本文','original-order',ARRAY['別名'])`, ids["content"]); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO `+table("content_version_text")+`(tenant_id,version_id,language,title,description) VALUES('version-owned-migration',$1,'en','Edition override','Edition summary')`, ids["en"]); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	ids["version"] = "v0.129.0"
	return json.NewEncoder(os.Stdout).Encode(ids)
}
