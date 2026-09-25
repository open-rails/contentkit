package boundaries_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const m = "github.com/open-rails/contentkit"

type pkg struct {
	ImportPath string
	Standard   bool
	Imports    []string
	Deps       []string
	CgoFiles   []string
	Error      *struct{ Err string }
}

type rule struct {
	pkgs []string
	only []string // if set, every non-std dependency must match one of these
	deny []string
}

var rules = []rule{
	{pkgs: []string{m + "/contentref", m + "/media/token"}, only: []string{}},
	{pkgs: []string{m + "/access"}, only: []string{m + "/contentref"}},
	{pkgs: []string{m + "/media/layout"}, only: []string{m + "/media/token"}},
	// The access worker: stdlib, token/layout and the SigV4 signer. No DB,
	// River, S3 client, CGO or content/search/signal.
	{pkgs: []string{m + "/cmd/media-access", m + "/media/accessworker"}, only: []string{
		m + "/media/token", m + "/media/layout", m + "/media/accessworker",
		"github.com/aws/aws-sdk-go-v2/aws/...", "github.com/aws/aws-sdk-go-v2/internal/...", "github.com/aws/smithy-go/...",
	}},
	{pkgs: []string{m + "/content"}, deny: []string{
		"github.com/aws/...", m + "/media/image/...", m + "/media/video/...", m + "/media/s3/...",
		m + "/signal/...", m + "/taxonomy/...",
	}},
	{pkgs: []string{m + "/media"}, deny: []string{
		"github.com/aws/...", m + "/media/image/...", m + "/media/video/...", m + "/media/s3/...",
		m + "/content/...", m + "/search/...", m + "/signal/...", m + "/taxonomy/...",
	}},
	// The host's side of the media worker: no libvips, ffmpeg or S3 client.
	{pkgs: []string{m + "/media/workqueue"}, deny: []string{
		"github.com/aws/...", m + "/media/image/...", m + "/media/video/...", m + "/media/worker/...", m + "/media/s3/...",
	}},
	{pkgs: []string{m + "/search", m + "/signal", m + "/taxonomy"}, deny: []string{
		m + "/media/...", m + "/content/...",
	}},
}

// importers restricts which module packages may import a third-party tree directly.
var importers = map[string][]string{
	"github.com/aws/aws-sdk-go-v2/service/s3/...": {m + "/media/s3", m + "/media/internal/..."},
	"github.com/davidbyttow/govips/...":           {m + "/media/image"},
}

func match(path, pattern string) bool {
	if base, ok := strings.CutSuffix(pattern, "/..."); ok {
		return path == base || strings.HasPrefix(path, base+"/")
	}
	return path == pattern
}

func matchAny(path string, patterns []string) bool {
	for _, p := range patterns {
		if match(path, p) {
			return true
		}
	}
	return false
}

func load(t *testing.T) map[string]*pkg {
	t.Helper()
	cmd := exec.Command("go", "list", "-e", "-deps", "-json=ImportPath,Standard,Imports,Deps,CgoFiles,Error", "./...")
	cmd.Dir = "../.."
	// go list reports CgoFiles without invoking a C compiler.
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1", "GOFLAGS=")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	pkgs := map[string]*pkg{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p pkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if p.Error != nil {
			t.Errorf("%s: %s", p.ImportPath, p.Error.Err)
		}
		pkgs[p.ImportPath] = &p
	}
	return pkgs
}

func TestBoundaries(t *testing.T) {
	pkgs := load(t)
	get := func(path string) *pkg {
		p := pkgs[path]
		if p == nil {
			t.Fatalf("rule names unknown package %s", path)
		}
		return p
	}

	for _, r := range rules {
		for _, path := range r.pkgs {
			for _, dep := range get(path).Deps {
				if pkgs[dep].Standard {
					continue
				}
				if r.only != nil && !matchAny(dep, r.only) {
					t.Errorf("%s depends on %s (allowed: stdlib %v)", path, dep, r.only)
				}
				if matchAny(dep, r.deny) {
					t.Errorf("%s depends on %s", path, dep)
				}
			}
		}
	}

	var cgo []string
	for path, p := range pkgs {
		if !p.Standard && len(p.CgoFiles) > 0 {
			cgo = append(cgo, path)
		}
	}
	if len(cgo) == 0 {
		t.Fatal("no CGO packages found; is govips still a dependency of media/image?")
	}

	for path, p := range pkgs {
		if !strings.HasPrefix(path, m) {
			continue
		}
		// Only media/image, and the media worker built on it, may pull in CGO;
		// nothing depends on the root hub.
		if path != m+"/media/image" && path != m+"/media/worker" && path != m+"/cmd/media-worker" {
			for _, dep := range p.Deps {
				if matchAny(dep, cgo) {
					t.Errorf("%s depends on CGO package %s", path, dep)
				}
			}
		}
		for _, dep := range p.Deps {
			if dep == m {
				t.Errorf("%s depends on the root package", path)
			}
		}
		for _, imp := range p.Imports {
			for target, allowed := range importers {
				if match(imp, target) && !matchAny(path, allowed) {
					t.Errorf("%s imports %s (only %v may)", path, imp, allowed)
				}
			}
		}
	}
}
