// Package contract renders ContentKit's route catalog (internal/httpapi) and
// error-code registry as the files other tools read: api/openapi.json, the
// Go client's methods (client_gen.go), the browser SDK's generated route
// table, wire types and error codes, and docs/api/routes.md. They are committed: `go generate ./internal/contract`
// rewrites them and TestGeneratedContractIsFresh fails when one is stale.
package contract

//go:generate go run ./gen -root ../..

import (
	"io/fs"
	"os"
	"path/filepath"

	"github.com/open-rails/contentkit/internal/httpapi"

	// The modules register their routes.
	_ "github.com/open-rails/contentkit"
	_ "github.com/open-rails/contentkit/internal/catalogstore"
	_ "github.com/open-rails/contentkit/internal/contentstore"
	_ "github.com/open-rails/contentkit/internal/discoverystore"
	_ "github.com/open-rails/contentkit/internal/entitystore"
	_ "github.com/open-rails/contentkit/internal/importstore"
	_ "github.com/open-rails/contentkit/internal/mediastore"
	_ "github.com/open-rails/contentkit/internal/urlstore"
	_ "github.com/open-rails/contentkit/internal/versionstore"
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
	client, err := m.goClient()
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		OpenAPIFile:               openapi,
		GoClientFile:              client,
		GoErrorCodesFile:          goErrorCodes(),
		RoutesDoc:                 m.routesMD(),
		SDKDir + "routes.ts":      m.routesTS(),
		SDKDir + "wire.ts":        m.wireTS(),
		SDKDir + "error-codes.ts": errorCodesTS(),
	}, nil
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
