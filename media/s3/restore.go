package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/open-rails/contentkit/media"
)

type version struct {
	id       string
	modified time.Time
	latest   bool
	marker   bool
}

// Snapshot reads an item's manifest and exact referenced object versions at at.
// It changes no objects or delete markers. Apply it with media.Jobs.Restore after
// restoring the host's records and reapplying any subsequent privacy erasures.
func (s *Store) Snapshot(ctx context.Context, item media.Item, at time.Time) (media.Snapshot, error) {
	var snap media.Snapshot
	if at.IsZero() {
		return snap, errors.New("s3: snapshot needs a historical time")
	}
	history := map[string][]version{}
	prefix := item.Prefix()
	p := s3.NewListObjectVersionsPaginator(s.client, &s3.ListObjectVersionsInput{Bucket: &s.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return snap, mapErr("list versions", prefix, err)
		}
		for _, v := range page.Versions {
			key := aws.ToString(v.Key)
			history[key] = append(history[key], version{aws.ToString(v.VersionId), aws.ToTime(v.LastModified), aws.ToBool(v.IsLatest), false})
		}
		for _, v := range page.DeleteMarkers {
			key := aws.ToString(v.Key)
			history[key] = append(history[key], version{aws.ToString(v.VersionId), aws.ToTime(v.LastModified), aws.ToBool(v.IsLatest), true})
		}
	}
	selectVersion := func(key string) (string, error) {
		versions := history[key]
		slices.SortStableFunc(versions, func(a, b version) int {
			if cmp := b.modified.Compare(a.modified); cmp != 0 {
				return cmp
			}
			if a.latest && !b.latest {
				return -1
			}
			if b.latest && !a.latest {
				return 1
			}
			return 0
		})
		for _, v := range versions {
			if v.modified.After(at) {
				continue
			}
			if v.marker {
				break
			}
			if v.id == "" || v.id == "null" {
				return "", errors.New("s3: snapshot requires immutable bucket versions")
			}
			return v.id, nil
		}
		return "", fmt.Errorf("s3: snapshot %s: %w", key, media.ErrNotFound)
	}
	var err error
	snap.Ref = item.Ref()
	snap.VersionID, err = selectVersion(item.ManifestKey())
	if err != nil {
		return snap, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: aws.String(item.ManifestKey()), VersionId: &snap.VersionID})
	if err != nil {
		return snap, mapErr("get snapshot", item.ManifestKey(), err)
	}
	body, err := io.ReadAll(io.LimitReader(out.Body, media.MaxManifestBytes+1))
	out.Body.Close()
	if err != nil {
		return snap, err
	}
	snap.Manifest, err = media.DecodeManifest(body)
	if err != nil {
		return snap, err
	}
	if snap.Manifest.Deleted {
		return snap, media.ErrNotFound
	}
	var keys []string
	for _, name := range snap.Manifest.Blobs() {
		key, err := item.Blob(name)
		if err != nil {
			return snap, err
		}
		keys = append(keys, key)
	}
	for _, name := range snap.Manifest.StagedNames() {
		key, err := item.Staged(name)
		if err != nil {
			return snap, err
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range slices.Compact(keys) {
		id, err := selectVersion(key)
		if err != nil {
			return snap, err
		}
		snap.Objects = append(snap.Objects, media.ObjectVersion{Key: key, VersionID: id})
	}
	return snap, nil
}

// CopyVersion copies an immutable historical object to a newly allocated key.
// Large objects use the same bounded multipart path as ordinary worker copies.
func (s *Store) CopyVersion(ctx context.Context, v media.ObjectVersion, dst string) (media.Object, error) {
	if v.VersionID == "" || v.VersionID == "null" {
		return media.Object{}, errors.New("s3: copy needs an immutable version")
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &v.Key, VersionId: &v.VersionID,
		ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		return media.Object{}, mapErr("head version", v.Key, err)
	}
	head := media.Object{Key: v.Key, Size: aws.ToInt64(out.ContentLength), ETag: aws.ToString(out.ETag),
		ContentType: aws.ToString(out.ContentType), CacheControl: aws.ToString(out.CacheControl), Metadata: out.Metadata}
	return s.copy(ctx, s.bucket+"/"+v.Key+"?versionId="+url.QueryEscape(v.VersionID), dst, head)
}
