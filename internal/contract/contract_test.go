package contract

import (
	"os"
	"testing"

	"github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/httpapi"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/taxonomy"
)

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
		content.CodeNotConfigured, content.CodeTenantMismatch, content.CodeInternal,
		taxonomy.CodeInvalidRequest, taxonomy.CodeNotFound, taxonomy.CodeConflict, taxonomy.CodeInternal} {
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
