package boundaries_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/internal/httpapi"
)

const module = "github.com/open-rails/contentkit"

// typePackages hold the wire and data types hosts use; their implementations
// live in internal twins.
var typePackages = []string{"access", "catalog", "channels", "content", "contentref", "contenturl", "discovery",
	"entities", "importer", "kinds", "media", "media/versions", "signal"}

// public is every package a host, server/ or adapters/* may import. The
// gateway's Config takes token and layout values.
var public = append([]string{"", "media/s3", "media/avatar", "media/gateway", "media/token", "media/layout"}, typePackages...)

// leafHelpers are the internal packages a type package may import: pure
// value helpers with no storage.
var leafHelpers = []string{"internal/codes", "internal/normalize", "internal/edition"}

// Exported functions of type packages named New*, allowed: they build values,
// not stores.
var valueConstructors = map[string]bool{"contentref.New": true, "contentref.NewVersion": true, "contentref.NewID": true}

func root(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestPublicAPIReachesNoInternalType walks every route body and the root API
// (Engine and Client methods, Config, Deps, MigrationOptions): none may
// reach a type hosts cannot name.
func TestPublicAPIReachesNoInternalType(t *testing.T) {
	w := walker{seen: map[reflect.Type]bool{}}
	for _, s := range httpapi.Catalog() {
		at := s.Method + " " + s.FullPath()
		if s.Request != nil {
			w.walk(reflect.TypeOf(s.Request), at+" request")
		}
		for _, r := range s.Responses {
			if r.Body != nil {
				w.walk(reflect.TypeOf(r.Body), at+" response")
			}
		}
	}
	for _, v := range []any{(*contentkit.Engine)(nil), (*contentkit.Client)(nil), contentkit.Config{}, contentkit.Deps{}, contentkit.MigrationOptions{}} {
		w.walk(reflect.TypeOf(v), "contentkit")
	}
	slices.Sort(w.bad)
	for _, b := range slices.Compact(w.bad) {
		t.Error(b)
	}
}

type walker struct {
	seen map[reflect.Type]bool
	bad  []string
}

// hostless are foreign types the API never takes or returns: a shared
// transaction or a host's job fleet makes ContentKit embedded-only.
func hostless(pkg, name string) bool {
	return pkg == "github.com/jackc/pgx/v5" && name == "Tx" || pkg == "github.com/open-rails/helpers/river" ||
		pkg == "github.com/riverqueue/river" || strings.HasPrefix(pkg, "github.com/riverqueue/river/")
}

func foreign(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	p := t.PkgPath()
	return p != "" && p != module && !strings.HasPrefix(p, module+"/")
}

func (w *walker) walk(t reflect.Type, via string) {
	if t == nil || w.seen[t] || t == reflect.TypeFor[httpapi.Stream]() {
		return
	}
	w.seen[t] = true
	if p := t.PkgPath(); p != "" {
		if p != module && !strings.HasPrefix(p, module+"/") {
			if hostless(p, t.Name()) {
				w.bad = append(w.bad, via+" reaches "+p+"."+t.Name())
			}
			return
		}
		if strings.Contains(p+"/", "/internal/") {
			w.bad = append(w.bad, via+" reaches "+t.String()+" ("+p+")")
			return
		}
		via = t.String()
	}
	if foreign(t) {
		w.walk(t.Elem(), via)
		return
	}
	for i := range t.NumMethod() {
		if m := t.Method(i); m.IsExported() {
			w.walk(m.Type, via+"."+m.Name)
		}
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
		w.walk(t.Elem(), via)
	case reflect.Map:
		w.walk(t.Key(), via)
		w.walk(t.Elem(), via)
	case reflect.Func:
		for i := range t.NumIn() {
			w.walk(t.In(i), via)
		}
		for i := range t.NumOut() {
			w.walk(t.Out(i), via)
		}
	case reflect.Struct:
		for i := range t.NumField() {
			if f := t.Field(i); f.IsExported() {
				w.walk(f.Type, via+"."+f.Name)
			}
		}
	}
}

// TestTypePackagesHoldNoStores: a type package imports no database driver,
// job queue or internal package beyond leaf helpers, and exports no store,
// constructor, options or transaction method.
func TestTypePackagesHoldNoStores(t *testing.T) {
	dir := root(t)
	for _, p := range typePackages {
		for _, f := range parseDir(t, filepath.Join(dir, p), 0) {
			for _, imp := range imports(f.file) {
				switch {
				case strings.HasPrefix(imp, "github.com/jackc/pgx"), strings.HasPrefix(imp, "github.com/riverqueue/"):
					t.Errorf("%s imports %s", f.name, imp)
				case strings.HasPrefix(imp, module+"/internal/") && !slices.Contains(leafHelpers, strings.TrimPrefix(imp, module+"/")):
					t.Errorf("%s imports %s", f.name, imp)
				}
			}
			for _, d := range f.file.Decls {
				switch d := d.(type) {
				case *ast.FuncDecl:
					name := d.Name.Name
					if !ast.IsExported(name) {
						continue
					}
					if strings.HasSuffix(name, "Tx") {
						t.Errorf("%s exports %s", f.name, name)
					}
					if d.Recv == nil && strings.HasPrefix(name, "New") && !valueConstructors[filepath.Base(p)+"."+name] {
						t.Errorf("%s exports constructor %s", f.name, name)
					}
				case *ast.GenDecl:
					for _, s := range d.Specs {
						ts, ok := s.(*ast.TypeSpec)
						if !ok {
							continue
						}
						_, port := ts.Type.(*ast.InterfaceType)
						switch name := ts.Name.Name; {
						case name == "Options", name == "Service", name == "Runtime", name == "Router", name == "Registry":
							t.Errorf("%s exports type %s", f.name, name)
						case name == "Store" && !port:
							t.Errorf("%s exports type %s", f.name, name)
						}
					}
				}
			}
		}
	}
}

// TestServerAndAdaptersUsePublicSurface: the standalone server and the
// adapters build on what hosts may import, nothing more.
func TestServerAndAdaptersUsePublicSurface(t *testing.T) {
	dir := root(t)
	allowed := map[string]bool{}
	for _, p := range public {
		allowed[strings.TrimSuffix(module+"/"+p, "/")] = true
	}
	for _, tree := range []string{"server", "adapters"} {
		err := filepath.WalkDir(filepath.Join(dir, tree), func(path string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return err
			}
			if d.Name() == "node_modules" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			for _, f := range parseDir(t, path, parser.ImportsOnly) {
				for _, imp := range imports(f.file) {
					if strings.HasPrefix(imp, module) && !allowed[imp] && !strings.HasPrefix(imp, module+"/adapters/") && !strings.HasPrefix(imp, module+"/server") {
						t.Errorf("%s imports %s", f.name, imp)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

type parsed struct {
	name string
	file *ast.File
}

// parseDir parses dir's non-test Go files.
func parseDir(t *testing.T, dir string, mode parser.Mode) []parsed {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []parsed
	fset := token.NewFileSet()
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		path := filepath.Join(dir, n)
		f, err := parser.ParseFile(fset, path, nil, mode)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root(t), path)
		out = append(out, parsed{rel, f})
	}
	return out
}

func imports(f *ast.File) []string {
	var out []string
	for _, s := range f.Imports {
		p, _ := strconv.Unquote(s.Path.Value)
		out = append(out, p)
	}
	return out
}
