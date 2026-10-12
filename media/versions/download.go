package versions

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/ratelimit"
	"github.com/open-rails/contentkit/media"
)

// Watermarker marks a viewer's copy of a version's pages: the host's
// forensic watermark, applied when a download streams.
type Watermarker interface {
	// Pages are the pages (from 0) of v to mark for actor, of its pages;
	// none serves the version's prepared archive.
	Pages(ctx context.Context, actor access.Actor, v Target, pages int) ([]int, error)
	// Mark returns page i's marked bytes and their extension (".png").
	Mark(ctx context.Context, actor access.Actor, v Target, i int, page []byte) ([]byte, string, error)
}

// Downloads are the download quotas: an anonymous viewer's (by address), a
// signed-in one's, and the first of Tiers a viewer holds, which replaces
// User. A bucket without quotas is unlimited.
type Downloads struct {
	Anonymous []Quota     `json:"anonymous,omitempty" yaml:"anonymous"`
	User      []Quota     `json:"user,omitempty" yaml:"user"`
	Tiers     []TierQuota `json:"tiers,omitempty" yaml:"tiers"`
	// Redis shares the counts across replicas; nil keeps them per process.
	Redis redis.UniversalClient `json:"-" yaml:"-"`
}

// Quota admits at most Limit downloads in any Per.
type Quota struct {
	Limit int           `json:"limit" yaml:"limit"`
	Per   time.Duration `json:"per" yaml:"per"`
}

// TierQuota is the bucket of a tier's holders.
type TierQuota struct {
	Tier   string  `json:"tier" yaml:"tier"`
	Quotas []Quota `json:"quotas" yaml:"quotas"`
}

type quotas struct {
	anonymous, user *ratelimit.Limiter
	tiers           []string
	byTier          map[string]*ratelimit.Limiter
}

func newQuotas(d Downloads, log *slog.Logger, now func() time.Time) (*quotas, error) {
	build := func(name string, qs []Quota) (*ratelimit.Limiter, error) {
		if len(qs) == 0 {
			return nil, nil
		}
		ws := make([]ratelimit.Window, len(qs))
		for i, q := range qs {
			ws[i] = ratelimit.Window{Limit: q.Limit, Per: q.Per}
		}
		l, err := ratelimit.New(ratelimit.Options{Windows: ws, Redis: d.Redis, Prefix: "contentkit:downloads:" + name + ":", Logger: log, Now: now})
		if err != nil {
			return nil, fmt.Errorf("versions: downloads %s: %w", name, err)
		}
		return l, nil
	}
	q := &quotas{byTier: map[string]*ratelimit.Limiter{}}
	var err error
	if q.anonymous, err = build("anonymous", d.Anonymous); err != nil {
		return nil, err
	}
	if q.user, err = build("user", d.User); err != nil {
		return nil, err
	}
	for _, t := range d.Tiers {
		if t.Tier == "" || slices.Contains(q.tiers, t.Tier) {
			return nil, fmt.Errorf("versions: downloads: tier %q is empty or repeated", t.Tier)
		}
		l, err := build("tier:"+t.Tier, t.Quotas)
		if err != nil {
			return nil, err
		}
		q.tiers, q.byTier[t.Tier] = append(q.tiers, t.Tier), l
	}
	return q, nil
}

// QuotaError is a download refused by its viewer's quota.
type QuotaError struct {
	Bucket     string
	RetryAfter time.Duration
}

func (e *QuotaError) Error() string { return "versions: download quota reached (" + e.Bucket + ")" }

// charge takes one download from actor's bucket.
func (s *Service) charge(ctx context.Context, actor access.Actor) error {
	bucket, key, l := "anonymous", "ip:"+actor.IP, s.dl.anonymous
	if !actor.Anonymous && actor.ID != "" {
		bucket, key, l = "user", "user:"+actor.ID, s.dl.user
		if len(s.dl.tiers) > 0 && s.o.Gate != nil {
			f, err := s.o.Gate.Filter(ctx, actor)
			if err != nil {
				return err
			}
			if i := slices.IndexFunc(s.dl.tiers, f.Tier); i >= 0 {
				bucket, l = s.dl.tiers[i], s.dl.byTier[s.dl.tiers[i]]
			}
		}
	}
	if l == nil {
		return nil
	}
	ok, wait := l.Allow(ctx, key)
	if !ok {
		return &QuotaError{Bucket: bucket, RetryAfter: wait}
	}
	return nil
}

// Archive is a download ready to stream: the prepared archive, or the
// pages to zip with their marks.
type Archive struct {
	Name string // the attachment's file name
	Size int64  // the prepared archive's, 0 when zipped as it streams
	ctx  context.Context
	s    *Service
	src  io.ReadCloser
	keys []string
	mark []int
	v    Target
	who  access.Actor
}

// Download opens t's download for actor: every page released and the
// version open (or actor edits it). ErrNotReady while its pages or archive
// are still processing; a QuotaError past actor's quota, charged only once
// the archive can be served.
func (s *Service) Download(ctx context.Context, actor access.Actor, t Target) (*Archive, error) {
	v, err := s.one(ctx, t)
	if err != nil {
		return nil, err
	}
	k, err := s.published(v)
	if err != nil {
		return nil, err
	}
	z := private(k, k.Publish.Download)
	if z == nil {
		return nil, ErrNotFound
	}
	ds, err := s.decisions(ctx, actor, v.Kind, []Target{t}, nil)
	if err != nil {
		return nil, err
	}
	switch d := ds[t]; {
	case !d.res.Visible:
		return nil, ErrNotFound
	case !d.whole():
		return nil, ErrForbidden
	}
	ref, err := s.ref(k.Name, t)
	if err != nil {
		return nil, err
	}
	item, err := s.reg.Item(ref)
	if err != nil {
		return nil, err
	}
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if errors.Is(err, media.ErrNotFound) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if m.Meta[forkMeta] != nil {
		return nil, ErrNotReady
	}
	pages, err := zipPages(k, z, m)
	if err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, ErrNotFound
	}
	a := &Archive{Name: s.archiveName(ctx, v, z), ctx: ctx, s: s, v: t, who: actor}
	if s.o.Watermark != nil && !actor.Anonymous {
		if a.mark, err = s.o.Watermark.Pages(ctx, actor, t, len(pages)); err != nil {
			return nil, err
		}
	}
	if len(a.mark) > 0 {
		for _, f := range pages {
			key, err := item.Blob(f.Blob)
			if err != nil {
				return nil, err
			}
			a.keys = append(a.keys, key)
		}
		if _, err := s.m.Manifests.Store().Head(ctx, a.keys[0]); err != nil {
			return nil, err
		}
	} else {
		f, ok := m.Get(z.To)
		if !ok || f.Blob == "" || k.ZipStale(m, z) {
			return nil, ErrNotReady
		}
		key, err := item.Blob(f.Blob)
		if err != nil {
			return nil, err
		}
		body, obj, err := s.m.Manifests.Store().Get(ctx, key, media.GetOptions{})
		if err != nil {
			return nil, err
		}
		if obj.Size != f.Size || obj.Size <= 0 {
			body.Close()
			return nil, fmt.Errorf("versions: archive %s is %d bytes, its manifest says %d", key, obj.Size, f.Size)
		}
		a.src, a.Size = body, obj.Size
	}
	if err := s.charge(ctx, actor); err != nil {
		if a.src != nil {
			a.src.Close()
		}
		return nil, err
	}
	return a, nil
}

// zipPages are the files the archive zips, in manifest order: each page's
// output under the zip's prefix. ErrNotReady while one is missing.
func zipPages(k *media.Kind, z *media.Private, m *media.Manifest) ([]media.File, error) {
	var out []media.File
	for _, f := range m.Files {
		if !f.IsUpload() || f.Unattached || !inPattern(k.Publish.Pages, f.Path) {
			continue
		}
		if f.Fail() != nil || f.Blob == "" {
			return nil, ErrNotReady
		}
		found := false
		for _, p := range k.PrivateFor(f.Path) {
			if !strings.HasPrefix(p.To, z.Zip) {
				continue
			}
			if slices.Contains(f.Pending, p.Name) {
				return nil, ErrNotReady
			}
			for _, o := range m.Outputs(f.Path, p.Name) {
				if o.Blob != "" && !o.Gone {
					out, found = append(out, o), true
				}
			}
		}
		if !found {
			return nil, ErrNotReady
		}
	}
	return out, nil
}

// archiveName is the download's name: the zip's Download template with the
// version's title, else the title.
func (s *Service) archiveName(ctx context.Context, v version, z *media.Private) string {
	var title string
	_ = s.o.Pool.QueryRow(ctx, `SELECT COALESCE(NULLIF(vt.title, ''), ct.title, '') FROM `+s.t.items+` c
LEFT JOIN `+pgxIdent(s.o.Schema, "content_text")+` ct ON ct.tenant_id = c.tenant_id AND ct.content_id = c.id
LEFT JOIN `+pgxIdent(s.o.Schema, "content_version_text")+` vt ON vt.tenant_id = c.tenant_id AND vt.version_id = $3 AND vt.language = ct.language
WHERE c.tenant_id = $1 AND c.id = $2 ORDER BY ct.language = $4 DESC, ct.language LIMIT 1`, s.o.Tenant, v.Content, v.Version, v.Language).Scan(&title)
	title = media.CleanName(title)
	if title == "" {
		title = v.Content
	}
	name := title + ".zip"
	if z.Download != "" {
		name = strings.ReplaceAll(z.Download, "{title}", title)
	}
	return name
}

// WriteTo streams the archive within Download's context.
func (a *Archive) WriteTo(w io.Writer) (int64, error) {
	if a.src != nil {
		defer a.src.Close()
		return io.Copy(w, a.src)
	}
	return 0, a.s.streamMarked(a.ctx, w, a)
}

// Close releases an archive not streamed.
func (a *Archive) Close() error {
	if a.src != nil {
		return a.src.Close()
	}
	return nil
}

// streamMarked zips the pages one at a time, marking those the watermarker
// chose; the archive closes only once every mark succeeded.
func (s *Service) streamMarked(ctx context.Context, w io.Writer, a *Archive) error {
	zw := zip.NewWriter(w)
	now := time.Now()
	for i, key := range a.keys {
		rc, _, err := s.m.Manifests.Store().Get(ctx, key, media.GetOptions{})
		if err != nil {
			return err
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		name := fmt.Sprintf("%03d.webp", i+1)
		if slices.Contains(a.mark, i) {
			marked, ext, err := s.o.Watermark.Mark(ctx, a.who, a.v, i, body)
			if err != nil {
				return fmt.Errorf("versions: mark page %d: %w", i, err)
			}
			body, name = marked, strings.TrimSuffix(name, path.Ext(name))+ext
		}
		entry, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, Modified: now})
		if err != nil {
			return err
		}
		if _, err := entry.Write(body); err != nil {
			return err
		}
	}
	return zw.Close()
}
