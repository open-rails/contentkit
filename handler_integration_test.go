package contentkit

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/internal/pgtest"
	"github.com/open-rails/contentkit/internal/testauth"
)

// staffAuthz grants every permission to the actor "staff" and none to anyone else.
type staffAuthz struct{}

func (staffAuthz) Can(_ context.Context, a access.Actor, _ string) (bool, error) {
	return a.ID == "staff" && !a.Anonymous, nil
}

// itemResolver shows gallery cid(1), owned by "owner", to everyone, and lets
// signed-in actors edit its media.
type itemResolver struct{}

func (itemResolver) Resolve(_ context.Context, refs []contentref.ContentRef, a access.Actor) (map[contentref.ContentKey]access.Resolution, error) {
	out := map[contentref.ContentKey]access.Resolution{}
	for _, r := range refs {
		if r.ContentKind == "gallery" && r.ContentID == cid(1) {
			out[r.Key()] = access.Resolution{Visible: true, Accessible: true, Owner: "owner", Editor: !a.Anonymous && a.ID != ""}
		}
	}
	return out, nil
}

func pngBytes(t *testing.T, w int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, w))
	for i := range w {
		img.Set(i, i, color.RGBA{R: 200, A: 255})
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestRuntimeRefusesReservedKindsIntegration(t *testing.T) {
	ctx := context.Background()
	pool := testPG(t)
	pgtest.EnsureExtensions(t, ctx, pool)
	schema := pgtest.EmptySchema(t, ctx, pool)
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	if err := Migrate(ctx, MigrateConfig{DB: db, Schema: schema}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range httpapi.Reserved() {
		_, err := NewRuntime(ctx, RuntimeConfig{EmbeddedConfig: EmbeddedConfig{PG: pool, PGSchema: schema, Tenant: "site"},
			Auth:    testauth.Auth{Authz: staffAuthz{}},
			Content: content.Options{Resolver: itemResolver{}, ContentKinds: []string{kind}}})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", kind)) {
			t.Fatalf("content kind %q: %v", kind, err)
		}
	}
}

func ptr[T any](v T) *T { return &v }
