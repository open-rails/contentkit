package s3_test

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"

	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
	"github.com/open-rails/contentkit/media/layout"
	mediaS3 "github.com/open-rails/contentkit/media/s3"
)

func TestTemporaryWritesAreTaggedAndPermanentCopiesAreNot(t *testing.T) {
	env := s3test.Open(t)
	ctx := t.Context()
	cfg := env.Config
	cfg.CopyPartSize = 5 << 20
	store, err := mediaS3.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tagged := func(key string, want bool) {
		t.Helper()
		out, err := store.Client().GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: &cfg.Bucket, Key: &key})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, tag := range out.TagSet {
			if aws.ToString(tag.Key) == "contentkit-temp" && aws.ToString(tag.Value) == "true" {
				found = true
			}
		}
		if found != want {
			t.Fatalf("temporary tag on %s = %v, want %v", key, found, want)
		}
	}
	root := env.Tenant + "/gallery/" + cid(1) + "/"
	for _, size := range []int{1024, 6 << 20} {
		body := bytes.Repeat([]byte{42}, size)
		sum := sha256.Sum256(body)
		src := root + "temp/u-" + uuid.NewString()
		if _, err := store.Put(ctx, src, bytes.NewReader(body), int64(size), media.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		tagged(src, true)
		dst := root + "private/" + layout.BlobName(sum[:], uuid.NewString())
		if _, err := store.Copy(ctx, src, dst, media.CopyOptions{}); err != nil {
			t.Fatal(err)
		}
		tagged(dst, false)
		copyTemp := root + "temp/u-" + uuid.NewString()
		if _, err := store.Copy(ctx, dst, copyTemp, media.CopyOptions{}); err != nil {
			t.Fatal(err)
		}
		tagged(copyTemp, true)
	}

	body := []byte("browser upload")
	sum := sha256.Sum256(body)
	key := root + "temp/u-" + uuid.NewString()
	p, err := store.PresignPut(ctx, key, media.PresignPut{ContentType: "application/octet-stream", Size: int64(len(body)), SHA256: sum[:], TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if p.Header.Get("X-Amz-Tagging") != "contentkit-temp=true" {
		t.Fatalf("temporary upload omitted its signed tagging header: %v", p.Header)
	}
	for _, override := range []http.Header{
		{"X-Amz-Tagging": nil},
		{"X-Amz-Tagging": {"contentkit-temp=false"}},
	} {
		if resp := send(t, p, body, override); resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("temporary upload did not reject missing or changed expiry tag: %s", resp.Status)
		}
	}
	if resp := send(t, p, body, http.Header{}); resp.StatusCode != http.StatusOK {
		t.Fatalf("temporary presigned upload: %s", resp.Status)
	}
	tagged(key, true)

	key = root + "temp/u-" + uuid.NewString()
	upload, err := store.CreateMultipart(ctx, key, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	part, err := store.PutPart(ctx, key, upload, 1, bytes.NewReader(body), int64(len(body)), sum[:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteMultipart(ctx, key, upload, []media.Part{part}); err != nil {
		t.Fatal(err)
	}
	tagged(key, true)

	key = "unrelated/path/temp/not-a-contentkit-object"
	if _, err := store.Put(ctx, key, strings.NewReader("keep"), 4, media.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	tagged(key, false)
	// Current app/account roots and retained nested version roots use the same
	// expiry policy as the simple legacy item exercised above.
	for _, root := range []string{
		(layout.Scope{TenantID: uuid.NewString(), AppID: uuid.NewString()}).Prefix("gallery", cid(2), ""),
		(layout.Scope{TenantID: uuid.NewString()}).Prefix("user", cid(3), ""),
		layout.Prefix(env.Tenant, "gallery", cid(4), "version"),
	} {
		src := root + "temp/u-" + uuid.NewString()
		if _, err := store.Put(ctx, src, bytes.NewReader(body), int64(len(body)), media.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		tagged(src, true)
		dst := root + "private/" + layout.BlobName(sum[:], uuid.NewString())
		if _, err := store.Copy(ctx, src, dst, media.CopyOptions{}); err != nil {
			t.Fatal(err)
		}
		tagged(dst, false)
	}
}

func TestConfigureRetainsUnrelatedLifecycleRulesAndExpiresTemporaryObjects(t *testing.T) {
	env := s3test.Open(t)
	ctx := t.Context()
	client, bucket := env.Store.Client(), env.Store.Bucket()
	_, err := client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: &bucket,
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{{
			ID: aws.String("application-owned"), Status: types.ExpirationStatusEnabled,
			Filter:     &types.LifecycleRuleFilter{Prefix: aws.String("application-cache/")},
			Expiration: &types.LifecycleExpiration{Days: aws.Int32(12)},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := env.Store.Configure(ctx, 30); err != nil {
			t.Fatal(err)
		}
	}
	out, err := client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: &bucket})
	if err != nil {
		t.Fatal(err)
	}
	foreign, temporary := 0, 0
	for _, rule := range out.Rules {
		switch aws.ToString(rule.ID) {
		case "application-owned":
			foreign++
			if rule.Expiration == nil || aws.ToInt32(rule.Expiration.Days) != 12 {
				t.Fatal("changed another application's lifecycle rule")
			}
		case "contentkit-temp":
			temporary++
			if rule.Filter == nil || rule.Filter.Tag == nil || aws.ToString(rule.Filter.Tag.Key) != "contentkit-temp" ||
				aws.ToString(rule.Filter.Tag.Value) != "true" || rule.Expiration == nil || aws.ToInt32(rule.Expiration.Days) != 7 ||
				rule.NoncurrentVersionExpiration == nil || aws.ToInt32(rule.NoncurrentVersionExpiration.NoncurrentDays) != 1 {
				t.Fatalf("temporary lifecycle rule: %+v", rule)
			}
		}
	}
	if foreign != 1 || temporary != 1 {
		t.Fatalf("lifecycle configuration lost or duplicated rules: foreign=%d temporary=%d", foreign, temporary)
	}
}
