package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/internal/s3test"
)

func TestDropPreservesOtherPrefixes(t *testing.T) {
	env := s3test.Open(t)
	ctx := context.Background()
	c, bucket := env.Store.Client(), env.Store.Bucket()
	if env.Created {
		_, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
			Bucket: &bucket, VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	status, err := env.VersioningStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantVersions := 1
	if status == types.BucketVersioningStatusEnabled {
		wantVersions = 2
	}
	prefix, other := env.Tenant+"/fixture/", env.Tenant+"/keep/"
	fixture := &uploadStore{Store: env.Store}
	var fixtureID, otherID string
	checksum := sha256.Sum256([]byte("keep"))
	for _, key := range []string{prefix + "manifest.json", other + "manifest.json"} {
		for range 2 {
			if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: &key, Body: bytes.NewReader([]byte("keep"))}); err != nil {
				t.Fatal(err)
			}
		}
		uploadKey := key + ".upload"
		create := env.Store.CreateMultipart
		if key == prefix+"manifest.json" {
			create = fixture.CreateMultipart
		}
		id, err := create(ctx, uploadKey, "application/octet-stream")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = env.Store.AbortMultipart(ctx, uploadKey, id) })
		if key == prefix+"manifest.json" {
			fixtureID = id
		} else {
			otherID = id
		}
		if _, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: &bucket, Key: &uploadKey, UploadId: &id,
			PartNumber: aws.Int32(1), Body: bytes.NewReader([]byte("keep")), ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(checksum[:]))}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: aws.String(prefix + "manifest.json")}); err != nil {
		t.Fatal(err)
	}
	if err := drop(fixture, prefix, false); err != nil {
		t.Fatal(err)
	}
	versions, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: &bucket, Prefix: &prefix})
	if err != nil || len(versions.Versions) != 0 || len(versions.DeleteMarkers) != 0 {
		t.Fatalf("fixture versions remain: %+v, %v", versions, err)
	}
	if _, err := env.Store.ListParts(ctx, prefix+"manifest.json.upload", fixtureID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("fixture upload remains: %v", err)
	}
	parts, err := env.Store.ListParts(ctx, other+"manifest.json.upload", otherID)
	if err != nil || len(parts) != 1 {
		t.Fatalf("neighboring upload was not preserved: %+v, %v", parts, err)
	}
	kept, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: &bucket, Prefix: &other})
	if err != nil || len(kept.Versions) != wantVersions {
		t.Fatalf("neighboring versions were not preserved: %+v, %v", kept, err)
	}
}
