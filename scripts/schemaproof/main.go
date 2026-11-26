// schemaproof proves that the working tree's PostgreSQL migrations build
// exactly what another revision's build. It applies both chains of each
// lineage to scratch databases through migratekit, as ContentKit applies them,
// and requires identical pg_dump output (schema and data, without owners).
//
//	go run ./scripts/schemaproof -ref REV -dsn URL [-pg-dump COMMAND] [-keep DIR]
//
// -dsn names a disposable PostgreSQL 18 server (with PGroonga) whose role may
// create databases; -pg-dump is the pg_dump 18 command, split on spaces, which
// must reach the server at the DSN's address.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/migratekit"
)

type chain struct{ dir, app string }

var (
	catalog  = chain{"internal/migrations/postgres", "contentkit"}
	accounts = chain{"internal/migrations/accounts", "contentkit-accounts"}
	worker   = chain{"internal/media/workqueue/migrations", "contentkit-media-worker"}
)

// A case applies its chains in order to one schema; river first applies
// River's migrations there, as workqueue.Migrate does.
type proofCase struct {
	name, schema string
	river        bool
	chains       []chain
}

var cases = []proofCase{
	{name: "catalog", schema: "contentkit", chains: []chain{catalog}},
	{name: "accounts", schema: "contentkit_accounts", chains: []chain{accounts}},
	{name: "shared", schema: "site", chains: []chain{catalog, accounts}},
	{name: "worker", schema: "media_worker", river: true, chains: []chain{worker}},
}

var pgDump []string

func main() {
	ref := flag.String("ref", "", "revision whose migration chains are the reference")
	dsn := flag.String("dsn", "", "disposable PostgreSQL 18 server")
	keep := flag.String("keep", "", "directory to keep the dumps in")
	command := flag.String("pg-dump", "pg_dump", "the pg_dump 18 command, split on spaces")
	flag.Parse()
	if pgDump = strings.Fields(*command); len(pgDump) == 0 {
		pgDump = []string{"pg_dump"}
	}
	if err := run(context.Background(), *ref, strings.TrimSpace(*dsn), *keep); err != nil {
		fmt.Fprintln(os.Stderr, "schemaproof:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, ref, dsn, keep string) error {
	if ref == "" || dsn == "" {
		return errors.New("-ref and -dsn are required")
	}
	out := keep
	if out == "" {
		var err error
		if out, err = os.MkdirTemp("", "schemaproof-"); err != nil {
			return err
		}
		defer os.RemoveAll(out)
	} else if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	old, tree := map[string]fs.FS{}, map[string]fs.FS{}
	for _, c := range []chain{catalog, accounts, worker} {
		files, err := chainAt(ctx, ref, c.dir)
		if err != nil {
			return err
		}
		old[c.dir], tree[c.dir] = files, os.DirFS(c.dir)
		was, _ := fs.Glob(files, "*.up.sql")
		now, _ := fs.Glob(tree[c.dir], "*.up.sql")
		fmt.Printf("%s: %d migrations at %s, %d in the working tree\n", c.dir, len(was), ref, len(now))
	}
	failed := false
	for _, pc := range cases {
		want, err := dump(ctx, dsn, pc, old)
		if err != nil {
			return fmt.Errorf("%s at %s: %w", pc.name, ref, err)
		}
		got, err := dump(ctx, dsn, pc, tree)
		if err != nil {
			return fmt.Errorf("%s in the working tree: %w", pc.name, err)
		}
		wantFile, gotFile := filepath.Join(out, pc.name+".reference.sql"), filepath.Join(out, pc.name+".candidate.sql")
		if err := os.WriteFile(wantFile, want, 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(gotFile, got, 0o644); err != nil {
			return err
		}
		if !bytes.Equal(want, got) {
			diff, _ := exec.CommandContext(ctx, "diff", "-u", wantFile, gotFile).CombinedOutput()
			os.Stdout.Write(diff)
			fmt.Printf("%s (schema %s): the dumps differ\n", pc.name, pc.schema)
			failed = true
			continue
		}
		fmt.Printf("%s (schema %s): identical, %d lines (%d bytes)\n", pc.name, pc.schema, bytes.Count(got, []byte("\n")), len(got))
	}
	if failed {
		return errors.New("the working tree builds a different schema")
	}
	return nil
}

// chainAt reads ref's migration files in dir from git.
func chainAt(ctx context.Context, ref, dir string) (fs.FS, error) {
	list, err := exec.CommandContext(ctx, "git", "ls-tree", "--name-only", ref, dir+"/").Output()
	if err != nil {
		return nil, fmt.Errorf("list %s at %s: %w", dir, ref, err)
	}
	files := fstest.MapFS{}
	for _, name := range strings.Fields(string(list)) {
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		body, err := exec.CommandContext(ctx, "git", "show", ref+":"+name).Output()
		if err != nil {
			return nil, fmt.Errorf("read %s at %s: %w", name, ref, err)
		}
		files[path.Base(name)] = &fstest.MapFile{Data: body}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s has no migrations at %s", dir, ref)
	}
	return files, nil
}

// dump applies a case's chains from sources to a scratch database and returns
// its normalised pg_dump.
func dump(ctx context.Context, dsn string, pc proofCase, sources map[string]fs.FS) ([]byte, error) {
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer admin.Close(context.WithoutCancel(ctx))
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	name := "schemaproof_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return nil, err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	}()
	target, err := url.Parse(dsn)
	if err != nil {
		return nil, err
	}
	target.Path = "/" + name
	pool, err := pgxpool.New(ctx, target.String())
	if err != nil {
		return nil, err
	}
	err = apply(ctx, pool, pc, sources)
	pool.Close()
	if err != nil {
		return nil, err
	}
	args := append(pgDump[1:len(pgDump):len(pgDump)], "--dbname="+target.String(), "--schema="+pc.schema, "--no-owner")
	raw, err := exec.CommandContext(ctx, pgDump[0], args...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("%s: %w: %s", strings.Join(pgDump, " "), err, bytes.TrimSpace(exit.Stderr))
		}
		return nil, err
	}
	return normalise(raw), nil
}

func apply(ctx context.Context, pool *pgxpool.Pool, pc proofCase, sources map[string]fs.FS) error {
	if pc.river {
		if err := riverhelpers.ApplyMigrations(ctx, pool, riverhelpers.MigrationOptions{Schema: pc.schema}); err != nil {
			return err
		}
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	for _, c := range pc.chains {
		steps, err := migratekit.Load(sources[c.dir], ".")
		if err != nil {
			return fmt.Errorf("load %s: %w", c.dir, err)
		}
		if err := migratekit.NewPostgres(db, c.app).WithSchema(pc.schema).ApplyMigrations(ctx, steps); err != nil {
			return fmt.Errorf("apply %s: %w", c.dir, err)
		}
	}
	return nil
}

// normalise drops what pg_dump varies between databases or runs: its header
// comments, session settings and the \restrict key; in rows, instants and UUIDs.
func normalise(raw []byte) []byte {
	var out bytes.Buffer
	rows := false
	for line := range strings.Lines(string(raw)) {
		bare := strings.TrimSuffix(line, "\n")
		switch {
		case rows && bare == `\.`:
			rows = false
		case rows:
			fields := strings.Split(bare, "\t")
			for i, f := range fields {
				switch {
				case instant.MatchString(f):
					fields[i] = "<instant>"
				case uuidValue.MatchString(f):
					fields[i] = "<uuid>"
				}
			}
			line = strings.Join(fields, "\t") + "\n"
		case strings.HasPrefix(bare, "COPY ") && strings.HasSuffix(bare, " FROM stdin;"):
			rows = true
		case bare == "--",
			strings.HasPrefix(bare, "-- PostgreSQL database dump"),
			strings.HasPrefix(bare, "-- Dumped "),
			strings.HasPrefix(bare, "SET "),
			strings.HasPrefix(bare, "SELECT pg_catalog.set_config('search_path'"),
			strings.HasPrefix(bare, `\restrict `),
			strings.HasPrefix(bare, `\unrestrict `):
			continue
		}
		out.WriteString(line)
	}
	return out.Bytes()
}

var (
	instant   = regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d(\.\d+)?\+00$`)
	uuidValue = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)
