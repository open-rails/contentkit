package versions

import (
	"archive/zip"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/authkit/auth"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/ratelimit"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// Watermarker marks one page of a viewer's page download: the host's
// forensic watermark. Which pages of which versions is the kind's
// kinds.Watermark; ContentKit asks only for the marking.
type Watermarker interface {
	// Mark returns page's (from 1) marked bytes for actor and their extension
	// (".png").
	Mark(ctx context.Context, actor access.Actor, v Target, page int, b []byte) ([]byte, string, error)
}

// Downloads are the download quotas: an anonymous viewer's (by address), a
// signed-in one's, and the first of Tiers a viewer holds, which replaces
// User. A bucket without quotas is unlimited.
type Downloads struct {
	Anonymous []Quota     `json:"anonymous,omitempty" yaml:"anonymous"`
	User      []Quota     `json:"user,omitempty" yaml:"user"`
	Tiers     []TierQuota `json:"tiers,omitempty" yaml:"tiers"`
	// TTL is how long a file download's URL opens its file; 0 is DefaultDownloadTTL.
	TTL time.Duration `json:"ttl,omitempty" yaml:"ttl"`
	// Redis shares the counts across replicas; nil keeps them per process.
	Redis redis.UniversalClient `json:"-" yaml:"-"`
}

// DefaultDownloadTTL is how long a file download's URL opens its file: long
// enough to start the transfer, which then runs to its end.
const DefaultDownloadTTL = 5 * time.Minute

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

// FileDownload is a granted download. URL serves it as an attachment named
// Name until ExpiresAt (a transfer begun runs to its end): the media host's
// URL of the file (the original, a rendition, a prepared zip), or, for a zip
// the kind's watermark marks for its viewer, this module's archive route,
// relative to the download route ("archive?..."). Size is 0 when the zip is
// marked as it streams.
type FileDownload struct {
	URL       string    `json:"url"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Size      int64     `json:"size,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Archive is a marked page archive ready to stream.
type Archive struct {
	Name string // the attachment's file name
	ctx  context.Context
	s    *Service
	keys []string
	mark []int // pages from 0
	v    Target
	who  access.Actor
}

// Download grants t's download to actor, what its kind's download serves
// (kinds.Kind.Download), and charges it to actor's quota: the version open
// (or actor edits it) with every segment released. ErrNotFound when the kind
// serves none, ErrNoDownload when the version has nothing to serve,
// ErrNotReady while it is placed or produced, a QuotaError past the quota.
func (s *Service) Download(ctx context.Context, actor access.Actor, t Target) (*FileDownload, error) {
	v, k, item, m, err := s.downloadable(ctx, t)
	if err != nil {
		return nil, err
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
	case !d.res.Editor:
		if ok, err := s.holds(ctx, actor, s.o.Kinds.Kinds[v.Kind].Download); err != nil {
			return nil, err
		} else if !ok {
			return nil, ErrForbidden
		}
	}
	title, code := s.title(ctx, v)
	ttl := cmp.Or(s.o.Downloads.TTL, DefaultDownloadTTL)
	var f media.File
	var name string
	if z := private(k, k.Download); z != nil && z.Zip != "" {
		pages, err := zipPages(k, z, m)
		if err != nil {
			return nil, err
		}
		if len(pages) == 0 {
			return nil, ErrNoDownload
		}
		name = archiveName(title, z)
		mark, err := s.marked(ctx, actor, v, len(pages))
		if err != nil {
			return nil, err
		}
		if len(mark) > 0 {
			if err := s.charge(ctx, actor); err != nil {
				return nil, err
			}
			exp := time.Unix(s.o.Now().Add(ttl).Unix(), 0)
			q := url.Values{"for": {actor.ID}, "ticket": {s.m.Reader.SignTicket(archiveScope(s.o.Tenant, t, actor.ID), exp)}}
			return &FileDownload{URL: "archive?" + q.Encode(), Name: name, Type: "application/zip", ExpiresAt: exp}, nil
		}
		var ok bool
		if f, ok = m.Get(z.To); !ok || f.Blob == "" || k.ZipStale(m, z) {
			return nil, ErrNotReady
		}
	} else {
		var state media.DownloadState
		switch f, state = k.DownloadFile(m); state {
		case media.DownloadNone:
			return nil, ErrNoDownload
		case media.DownloadPending:
			return nil, ErrNotReady
		}
		name = media.CleanName(title + " [" + code + "]" + cmp.Or(layout.DownloadExt(f.Type), path.Ext(f.Path)))
	}
	if area, _, _ := layout.SplitBlobRef(f.Blob); area != layout.AreaDownload {
		return nil, ErrNoDownload // stored before its kind put it in download/
	}
	if err := s.charge(ctx, actor); err != nil {
		return nil, err
	}
	u, exp, err := s.m.Reader.DownloadURL(item, f, name, ttl)
	if err != nil {
		return nil, err
	}
	return &FileDownload{URL: u, Name: name, Type: f.Type, Size: f.Size, ExpiresAt: exp}, nil
}

// Archive opens the marked archive a Download handed viewer for t, by its
// ticket: the pages zipped as they stream, the kind's watermark on those it
// marks. A ticket that is expired or not viewer's for t is ErrNotFound.
func (s *Service) Archive(ctx context.Context, t Target, viewer, ticket string) (*Archive, error) {
	if s.m.Reader == nil || viewer == "" || s.m.Reader.VerifyTicket(ticket, archiveScope(s.o.Tenant, t, viewer)) != nil {
		return nil, ErrNotFound
	}
	v, k, item, m, err := s.downloadable(ctx, t)
	if err != nil {
		return nil, err
	}
	z := private(k, k.Download)
	if z == nil || z.Zip == "" {
		return nil, ErrNotFound
	}
	pages, err := zipPages(k, z, m)
	if err != nil {
		return nil, err
	}
	title, _ := s.title(ctx, v)
	who := access.Actor{ID: viewer, Kind: auth.SubjectUser}
	a := &Archive{Name: archiveName(title, z), ctx: ctx, s: s, v: t, who: who}
	if a.mark, err = s.marked(ctx, who, v, len(pages)); err != nil {
		return nil, err
	}
	for _, f := range pages {
		key, err := item.Blob(f.Blob)
		if err != nil {
			return nil, err
		}
		a.keys = append(a.keys, key)
	}
	if len(a.keys) == 0 {
		return nil, ErrNoDownload
	}
	if _, err := s.m.Manifests.Store().Head(ctx, a.keys[0]); err != nil {
		return nil, err
	}
	return a, nil
}

// holds reports whether actor holds one of the download's tiers, or it
// names none.
func (s *Service) holds(ctx context.Context, actor access.Actor, d *kinds.Download) (bool, error) {
	if d == nil || len(d.Tiers) == 0 {
		return true, nil
	}
	if actor.Anonymous || actor.ID == "" || s.o.Gate == nil {
		return false, nil
	}
	f, err := s.o.Gate.Filter(ctx, actor)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(d.Tiers, f.Tier), nil
}

// archiveScope is the scope of viewer's ticket to t's marked archive; its
// ':' keeps it apart from every media item and download scope.
func archiveScope(tenant string, t Target, viewer string) string {
	return "archive:" + tenant + "/" + t.Content + "/" + t.Version + "/" + viewer
}

// downloadable is t's version, its kind's version view serving a download,
// its item and manifest.
func (s *Service) downloadable(ctx context.Context, t Target) (version, *media.Kind, media.Item, *media.Manifest, error) {
	v, err := s.one(ctx, t)
	if err != nil {
		return version{}, nil, media.Item{}, nil, err
	}
	mk, err := s.reg.Kind(v.Kind)
	if err != nil || mk.VersionKind() == nil || mk.VersionKind().Download == "" || s.m.Reader == nil {
		return version{}, nil, media.Item{}, nil, ErrNotFound
	}
	k := mk.VersionKind()
	ref, err := s.ref(k.Name, t)
	if err != nil {
		return version{}, nil, media.Item{}, nil, err
	}
	item, err := s.reg.Item(ref)
	if err != nil {
		return version{}, nil, media.Item{}, nil, err
	}
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if errors.Is(err, media.ErrNotFound) {
		return version{}, nil, media.Item{}, nil, ErrNoDownload
	} else if err != nil {
		return version{}, nil, media.Item{}, nil, err
	}
	if m.Meta[forkMeta] != nil {
		return version{}, nil, media.Item{}, nil, ErrNotReady
	}
	return v, k, item, m, nil
}

// marked are the pages (from 0) of v's pages its kind's watermark marks for
// actor: none signed out, without a Watermarker, or for a version outside the
// policy's tiers or groups.
func (s *Service) marked(ctx context.Context, actor access.Actor, v version, pages int) ([]int, error) {
	d := s.o.Kinds.Kinds[v.Kind].Download
	if s.o.Watermark == nil || actor.Anonymous || actor.ID == "" || d == nil || d.Watermark == nil {
		return nil, nil
	}
	w := d.Watermark
	var content access.Rule
	var override *access.Rule
	var groupKinds, slugs []string
	for _, g := range w.Groups {
		groupKinds, slugs = append(groupKinds, g.Kind), append(slugs, g.Slug)
	}
	var in bool
	if err := s.o.Pool.QueryRow(ctx, `SELECT c.access, v.access, cardinality($4::text[]) = 0 OR EXISTS (
SELECT 1 FROM `+pgxIdent(s.o.Schema, "content_memberships")+` m JOIN `+s.t.groups+` g ON g.tenant_id = m.tenant_id AND g.id = m.group_id
JOIN unnest($4::text[], $5::text[]) AS want(kind, slug) ON want.kind = g.kind AND want.slug = g.slug
WHERE m.tenant_id = $1 AND m.content_id = $2 AND (m.version_id IS NULL OR m.version_id = $3) AND m.state = 'active' AND g.state = 'active'
AND (m.active_from IS NULL OR m.active_from <= now()) AND (m.active_until IS NULL OR m.active_until > now()))
FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id
WHERE v.tenant_id = $1 AND v.content_id = $2 AND v.id = $3`, s.o.Tenant, v.Content, v.Version, groupKinds, slugs).Scan(&content, &override, &in); err != nil {
		return nil, err
	}
	rule := access.Effective(content, override)
	if !in || len(w.Tiers) > 0 && (rule.Level != access.TierLevel || rule.Free(s.o.Now()) ||
		!slices.ContainsFunc(rule.Tiers, func(t string) bool { return slices.Contains(w.Tiers, t) })) {
		return nil, nil
	}
	var out []int
	for _, p := range w.Pages {
		if p <= pages {
			out = append(out, p-1)
		}
	}
	slices.Sort(out)
	return out, nil
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

// title is v's title (its own, else its content's in its language) and its
// version code.
func (s *Service) title(ctx context.Context, v version) (title, code string) {
	_ = s.o.Pool.QueryRow(ctx, `SELECT COALESCE(NULLIF(vt.title, ''), ct.title, ''), cv.code FROM `+s.t.items+` c
JOIN `+s.t.versions+` cv ON cv.tenant_id = c.tenant_id AND cv.id = $3
LEFT JOIN `+pgxIdent(s.o.Schema, "content_text")+` ct ON ct.tenant_id = c.tenant_id AND ct.content_id = c.id
LEFT JOIN `+pgxIdent(s.o.Schema, "content_version_text")+` vt ON vt.tenant_id = c.tenant_id AND vt.version_id = $3 AND vt.language = ct.language
WHERE c.tenant_id = $1 AND c.id = $2 ORDER BY ct.language = $4 DESC, ct.language LIMIT 1`, s.o.Tenant, v.Content, v.Version, v.Language).Scan(&title, &code)
	if title = media.CleanName(title); title == "" {
		title = v.Content
	}
	return title, code
}

// archiveName is the zip's Download template with title, else title.zip.
func archiveName(title string, z *media.Private) string {
	if z.Download != "" {
		return strings.ReplaceAll(z.Download, "{title}", title)
	}
	return title + ".zip"
}

// WriteTo streams the archive within its request's context.
func (a *Archive) WriteTo(w io.Writer) (int64, error) {
	return 0, a.s.streamMarked(a.ctx, w, a)
}

// streamMarked zips the pages one at a time, marking those the kind's
// watermark names; the archive closes only once every mark succeeded.
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
			marked, ext, err := s.o.Watermark.Mark(ctx, a.who, a.v, i+1, body)
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
