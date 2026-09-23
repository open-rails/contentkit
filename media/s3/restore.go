package s3

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/open-rails/contentkit/media"
)

// RestoreReport lists what Restore changed, by key.
type RestoreReport struct {
	Reverted  []string // manifests, public slots and slot originals set to their version at T
	Removed   []string // such keys that did not exist at T
	Undeleted []string // referenced blobs and originals whose delete markers were removed
	Missing   []string // referenced at T but no version is left
}

type version struct {
	id       string
	modified time.Time
	latest   bool
	marker   bool
}

// Restore returns the folders under prefix to time at, on a versioned bucket
// (see Configure): manifests, public slots and slot originals take their
// version at T, then the blobs and originals those manifests reference lose
// the delete markers the sweep or a folder deletion left. Restore the host
// database to T first, and re-apply erasures made after T afterwards.
func (s *Store) Restore(ctx context.Context, prefix string, at time.Time) (RestoreReport, error) {
	var rep RestoreReport
	history := map[string][]version{}
	p := s3.NewListObjectVersionsPaginator(s.client, &s3.ListObjectVersionsInput{Bucket: &s.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return rep, mapErr("list versions", prefix, err)
		}
		for _, v := range page.Versions {
			k := aws.ToString(v.Key)
			history[k] = append(history[k], version{aws.ToString(v.VersionId), aws.ToTime(v.LastModified), aws.ToBool(v.IsLatest), false})
		}
		for _, m := range page.DeleteMarkers {
			k := aws.ToString(m.Key)
			history[k] = append(history[k], version{aws.ToString(m.VersionId), aws.ToTime(m.LastModified), aws.ToBool(m.IsLatest), true})
		}
	}
	for _, vs := range history {
		sort.SliceStable(vs, func(i, j int) bool {
			if !vs[i].modified.Equal(vs[j].modified) {
				return vs[i].modified.After(vs[j].modified)
			}
			return vs[i].latest && !vs[j].latest
		})
	}

	refs := map[string]bool{}
	for _, key := range slices.Sorted(maps.Keys(history)) {
		k, ok := media.ParseKey(key)
		if !ok || !pointInTime(k) {
			continue
		}
		vs := history[key]
		var then *version
		for i := range vs {
			if !vs[i].modified.After(at) {
				then = &vs[i]
				break
			}
		}
		now := vs[0]
		switch {
		case then != nil && !then.marker:
			if now.id != then.id {
				src := s.bucket + "/" + key + "?versionId=" + then.id
				if _, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: &s.bucket, Key: &key, CopySource: &src}); err != nil {
					return rep, mapErr("restore", key, err)
				}
				rep.Reverted = append(rep.Reverted, key)
			}
			if k.Area == media.AreaManifest {
				if err := s.collectRefs(ctx, key, then.id, k, refs); err != nil {
					return rep, err
				}
			}
		case !now.marker:
			if err := s.Delete(ctx, key); err != nil {
				return rep, err
			}
			rep.Removed = append(rep.Removed, key)
		}
	}

	for _, key := range slices.Sorted(maps.Keys(refs)) {
		vs := history[key]
		live := -1
		for i, v := range vs {
			if !v.marker {
				live = i
				break
			}
		}
		if live < 0 {
			rep.Missing = append(rep.Missing, key)
			continue
		}
		if live == 0 {
			continue
		}
		for _, m := range vs[:live] {
			if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key, VersionId: aws.String(m.id)}); err != nil {
				return rep, mapErr("undelete", key, err)
			}
		}
		rep.Undeleted = append(rep.Undeleted, key)
	}
	return rep, nil
}

// pointInTime keys are overwritten in place, so they return to their version
// at T; content-addressed blobs and originals never change and are undeleted.
func pointInTime(k media.Key) bool {
	return k.Area == media.AreaManifest || k.Area == media.AreaPublic ||
		(k.Area == media.AreaOriginals && !media.ValidBlobName(k.Name))
}

func (s *Store) collectRefs(ctx context.Context, key, versionID string, k media.Key, refs map[string]bool) error {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key, VersionId: &versionID})
	if err != nil {
		return mapErr("get version", key, err)
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		return err
	}
	var man media.Manifest
	if err := json.Unmarshal(body, &man); err != nil {
		return fmt.Errorf("s3: restore: decode %s@%s: %w", key, versionID, err)
	}
	folder := strings.Join([]string{k.Tenant, k.Kind, k.ID}, "/") + "/"
	for _, n := range man.Blobs() {
		refs[folder+media.AreaBlobs+"/"+n] = true
	}
	for _, n := range man.Originals() {
		refs[folder+media.AreaOriginals+"/"+n] = true
	}
	return nil
}
