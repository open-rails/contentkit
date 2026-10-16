package contract

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/media"
)

func TestByteContainersMatchJSONEncoding(t *testing.T) {
	m := &model{enums: &enumIndex{}}
	c := &Checker{m: m}
	for _, tc := range []struct {
		name, json, schema, typescript string
		value                          any
	}{
		{"fixed byte array", `[1,2,3]`, `{"type":"array","items":{"type":"integer"}}`, "number[]", [3]byte{1, 2, 3}},
		{"byte slice", `"AQID"`, `{"type":"string"}`, "string", []byte{1, 2, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.value)
			if err != nil || string(body) != tc.json {
				t.Fatalf("JSON encoding: %s, %v", body, err)
			}
			typ := reflect.TypeOf(tc.value)
			if err := c.validateJSON(typ, body); err != nil {
				t.Fatalf("actual JSON rejected: %v", err)
			}
			schema, err := marshal(m.schema(typ))
			if err != nil || string(schema) != tc.schema {
				t.Fatalf("schema: %s, %v", schema, err)
			}
			if got := m.tsType(typ); got != tc.typescript {
				t.Fatalf("TypeScript: %s, want %s", got, tc.typescript)
			}
		})
	}
}

func TestGeneratedContractIsFresh(t *testing.T) {
	if err := Verify(os.DirFS("../..")); err != nil {
		t.Fatal(err)
	}
}

// Every code a module answers is registered with the status the module
// answers it with.
func TestModuleCodesAreRegistered(t *testing.T) {
	statuses := map[string]int{}
	for _, c := range []string{media.CodeInvalid, media.CodeForbidden, media.CodeNotFound, media.CodeConflict, media.CodeIncomplete,
		media.CodeNotUploaded, media.CodeTooManyFiles, media.CodeTooLarge, media.CodeQuota, media.CodeType, media.CodeChecksum,
		media.CodeRate, media.CodeUnavailable, media.CodeImageTooSmall, media.CodeImageTooLarge, media.CodeImageUnreadable,
		media.CodeAnimationNotAllowed, media.CodeAnimationTooLong, media.CodeAnimationUnsupported, media.CodeVideoTooLong,
		media.CodeVideoTooLarge, media.CodeVideoOverBudget, media.CodeVideoAspectUnsupported} {
		statuses[c] = (&media.UploadError{Code: c}).Status()
	}
	for _, c := range []string{content.CodeInvalidRequest, content.CodeUnauthorized, content.CodeForbidden, content.CodeNotFound,
		content.CodeConflict, content.CodeModerationRejected, content.CodeRateLimited, content.CodeCommentBanned,
		content.CodeNotConfigured, content.CodeTenantMismatch, content.CodeInternal} {
		if _, ok := statuses[c]; !ok {
			statuses[c] = 0
		}
	}
	for code, status := range statuses {
		reg, ok := httpapi.LookupErrorCode(code)
		switch {
		case !ok:
			t.Errorf("%q is not registered", code)
		case status != 0 && reg.Status != status:
			t.Errorf("%q: module answers %d, registry says %d", code, status, reg.Status)
		}
	}
}

// Every permission is a full root:<resource>:<action> name, and every staff
// route checks one of them: no bare or unlisted name exists.
func TestPermissionsAreRootNames(t *testing.T) {
	listed := map[string]bool{}
	for _, p := range access.Permissions() {
		parts := strings.Split(string(p), ":")
		if len(parts) != 3 || parts[0] != "root" || parts[1] == "" || parts[2] == "" || listed[string(p)] {
			t.Errorf("permission %q", p)
		}
		listed[string(p)] = true
	}
	used := map[string]bool{}
	for _, r := range httpapi.Catalog() {
		if r.Auth != httpapi.Staff {
			continue
		}
		used[r.Perm] = true
		if !listed[r.Perm] {
			t.Errorf("%s %s checks %q, which access.Permissions() does not list", r.Method, r.FullPath(), r.Perm)
		}
	}
	// The catalog decides these per item, beside creators and group editors,
	// and avatar writes beside their person.
	used[string(access.ContentWrite)], used[string(access.ContentViewRestricted)], used[string(access.AvatarsWrite)] = true, true, true
	for p := range listed {
		if !used[p] {
			t.Errorf("no route checks %q", p)
		}
	}
}
