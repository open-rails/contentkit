package critical_test

import (
	"bytes"
	"crypto/sha256"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	ck "github.com/open-rails/contentkit"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// This proves the actual temporary tagging and permanent-copy boundaries on
// the selected S3 backend. It does not pretend that seven days elapsed or that
// a bucket lifecycle policy was installed by opening a reader/runtime.
func TestTemporaryPublicationStorage(t *testing.T) {
	scope := ck.Scope{TenantID: ck.NewTenantID(), AppID: ck.NewAppID()}
	storage := newMediaStorage(t, scope)
	prefix := "tenant/" + scope.TenantID.String() + "/app/" + scope.AppID.String() + "/gallery/" + uuid.NewString() + "/"
	payload := []byte("temporary upload retained with original filename metadata")
	sum := sha256.Sum256(payload)
	temp := prefix + "tmp/u-" + uuid.NewString()
	presign, err := storage.store.PresignPut(t.Context(), temp, media.PresignPut{ContentType: "text/plain", Size: int64(len(payload)), SHA256: sum[:], TTL: time.Minute})
	must(t, err)
	if presign.Header.Get("X-Amz-Tagging") != "contentkit-temp=true" {
		t.Fatal("version tmp presign omitted the signed expiry tag")
	}
	request, err := http.NewRequestWithContext(t.Context(), presign.Method, presign.URL, bytes.NewReader(payload))
	must(t, err)
	request.Header = presign.Header.Clone()
	response, err := http.DefaultClient.Do(request)
	must(t, err)
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, err)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("signed tmp upload: HTTP%d %s", response.StatusCode, body)
	}
	checkTag := func(key string, temporary bool) {
		t.Helper()
		tags, err := storage.management.GetObjectTagging(t.Context(), &awss3.GetObjectTaggingInput{Bucket: &storage.bucket, Key: &key})
		must(t, err)
		found := false
		for _, tag := range tags.TagSet {
			found = found || aws.ToString(tag.Key) == "contentkit-temp" && aws.ToString(tag.Value) == "true"
		}
		if found != temporary {
			t.Fatalf("temporary tag on %s = %v, want %v", key, found, temporary)
		}
	}
	checkTag(temp, true)
	for _, area := range []string{layout.AreaOriginals, layout.AreaFuture, layout.AreaPublic, layout.AreaPrivate} {
		key := prefix + area + "/" + layout.BlobName(sum[:], uuid.NewString())
		_, err := storage.store.Copy(t.Context(), temp, key, media.CopyOptions{})
		must(t, err)
		checkTag(key, false)
	}
	multipart := prefix + "tmp/u-" + uuid.NewString()
	upload, err := storage.store.CreateMultipart(t.Context(), multipart, "text/plain")
	must(t, err)
	part, err := storage.store.PutPart(t.Context(), multipart, upload, 1, bytes.NewReader(payload), int64(len(payload)), sum[:])
	must(t, err)
	_, err = storage.store.CompleteMultipart(t.Context(), multipart, upload, []media.Part{part})
	must(t, err)
	checkTag(multipart, true)
	abandoned := prefix + "tmp/u-" + uuid.NewString()
	abandonedID, err := storage.store.CreateMultipart(t.Context(), abandoned, "text/plain")
	must(t, err)
	_, err = storage.store.PutPart(t.Context(), abandoned, abandonedID, 1, bytes.NewReader(payload), int64(len(payload)), sum[:])
	must(t, err)
	must(t, storage.store.AbortMultipart(t.Context(), abandoned, abandonedID))
	remaining, err := storage.management.ListMultipartUploads(t.Context(), &awss3.ListMultipartUploadsInput{Bucket: &storage.bucket, Prefix: &prefix})
	must(t, err)
	if len(remaining.Uploads) != 0 {
		t.Fatal("owned multipart abort left unfinished bytes")
	}
	if media.TemporaryRetention != 7*24*time.Hour {
		t.Fatal("temporary retention no longer matches the seven-day contract")
	}
	_, err = storage.store.PresignPut(t.Context(), prefix+"tmp/u-"+uuid.NewString(), media.PresignPut{ContentType: "text/plain", Size: int64(len(payload)), SHA256: sum[:], TTL: media.TemporaryRetention + time.Second})
	if err == nil || !strings.Contains(err.Error(), "ttl") {
		t.Fatalf("temporary upload capability outlives seven-day retention: %v", err)
	}
}
