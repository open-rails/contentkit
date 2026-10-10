// Package contract renders ContentKit's route catalog (internal/httpapi) and
// error-code registry as the files other tools read: api/openapi.json, the
// browser SDK's generated route table, wire types and error codes, and
// docs/api/routes.md. They are committed: `go generate ./internal/contract`
// rewrites them and TestGeneratedContractIsFresh fails when one is stale.
package contract

//go:generate go run ./gen -root ../..

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-rails/contentkit/internal/httpapi"

	// The modules register their routes.
	_ "github.com/open-rails/contentkit/content"
	_ "github.com/open-rails/contentkit/contenturl"
	_ "github.com/open-rails/contentkit/media"
	_ "github.com/open-rails/contentkit/taxonomy"
)

// Where each generated file lives, relative to the repository.
const (
	OpenAPIFile = "api/openapi.json"
	RoutesDoc   = "docs/api/routes.md"
	SDKDir      = "sdk/ui/src/client/generated/"
)

// Files renders every generated file by repository path. fsys is the
// repository: enum values are read from the source that declares them.
func Files(fsys fs.FS) (map[string][]byte, error) {
	m, err := newModel(fsys, httpapi.Catalog())
	if err != nil {
		return nil, err
	}
	openapi, err := m.openAPI()
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		OpenAPIFile:               openapi,
		RoutesDoc:                 m.routesMD(),
		SDKDir + "routes.ts":      m.routesTS(),
		SDKDir + "wire.ts":        m.wireTS(),
		SDKDir + "error-codes.ts": errorCodesTS(),
	}, nil
}

// ErrStale is a committed generated file that no longer matches the catalog.
var ErrStale = errors.New("generated contract files are stale")

// Verify fails when a generated file in fsys differs from what the catalog
// renders, and names each one.
func Verify(fsys fs.FS) error {
	files, err := Files(fsys)
	if err != nil {
		return err
	}
	var stale []string
	for name, want := range files {
		if got, err := fs.ReadFile(fsys, name); err != nil || !bytes.Equal(got, want) {
			stale = append(stale, name)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	sort.Strings(stale)
	return fmt.Errorf("%w; run `go generate ./internal/contract` and review the change:\n  %s", ErrStale, strings.Join(stale, "\n  "))
}

// Write renders every generated file into the repository at root.
func Write(root string) error {
	files, err := Files(os.DirFS(root))
	if err != nil {
		return err
	}
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			return err
		}
	}
	return nil
}
