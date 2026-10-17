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
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/ratelimit"
	"github.com/open-rails/contentkit/kinds"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
)

// DownloadPolicy is the host's download policy: whether a viewer may download
// a version now, and which pages of a page download it marks with its own
// watermark. ContentKit asks; the host decides.
type DownloadPolicy interface {
	// Decide rules on a download after the access check, before the quota.
	Decide(ctx context.Context, r DownloadRequest) (DownloadDecision, error)
	// Mark returns page's (from 1) marked bytes for actor and their
	// extension (".png"): a page Decide named.
	Mark(ctx context.Context, actor access.Actor, v Target, page int, b []byte) ([]byte, string, error)
}

// DownloadRequest is a download Decide rules on.
type DownloadRequest struct {
	Actor  access.Actor
	Tiers  []string // the tiers Actor holds
	Editor bool     // Actor edits the version
	Target
	Kind string
	// Language is the version's language; Title is its title, in Language
	// when it has one there (its content's id when it has none).
	Language, Title string
	// Groups are the version's groups, its own and its content's.
	Groups []DownloadGroup
	// Rule is the version's effective access rule.
	Rule access.Rule
	// Released is when the version went live; zero while it is not.
	Released time.Time
	// Pages are a page download's pages; 0 for a file.
	Pages int
}

// DownloadGroup is a group of a download's version, named in its language,
// else the site's default, else another, else by its slug, with its
// attributes.
type DownloadGroup struct {
	kinds.GroupRef
	Name       string
	Attributes map[string]any
}

// In reports whether the version is in group g.
func (r DownloadRequest) In(g kinds.GroupRef) bool {
	return slices.ContainsFunc(r.Groups, func(d DownloadGroup) bool { return d.GroupRef == g })
}

// DownloadDecision is a policy's ruling.
type DownloadDecision struct {
	Allowed bool
	// A refusal's stable code of the host's ("release_window") and, when it
	// is a matter of time, when the viewer may download.
	Reason      string
	AvailableAt *time.Time
	// Mark are the pages (from 1) of a page download to mark; files ignore it.
	Mark []int
	// Name is the download's file name without its extension; "" keeps
	// ContentKit's. ContentKit cleans and bounds it.
	Name string
}

// UnavailableError is a download the policy refused.
type UnavailableError struct {
	Reason      string
	AvailableAt *time.Time
}

func (e *UnavailableError) Error() string { return "versions: download unavailable (" + e.Reason + ")" }

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

func newQuotas(d Downloads, tiers []string, log *slog.Logger, now func() time.Time) (*quotas, error) {
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
		if !slices.Contains(tiers, t.Tier) || slices.Contains(q.tiers, t.Tier) {
			return nil, fmt.Errorf("versions: downloads: tier %q is repeated or none of the content model's tiers %v", t.Tier, tiers)
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
	if actor.Member() {
		bucket, key, l = "user", "user:"+actor.AccountID(), s.dl.user
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
// (kinds.Kind.Download), as the host's policy allows, and charges it to
// actor's quota: the version open (or actor edits it) with every segment
// released. ErrNotFound when the kind serves none, an UnavailableError when
// the policy refuses, ErrNoDownload when the version has nothing to serve,
// ErrNotReady while it is placed or produced, a QuotaError past the quota.
func (s *Service) Download(ctx context.Context, actor access.Actor, t Target) (*FileDownload, error) {
	v, k, err := s.downloadKind(ctx, t)
	if err != nil {
		return nil, err
	}
	ds, err := s.decisions(ctx, actor, v.Kind, []Target{t}, nil)
	if err != nil {
		return nil, err
	}
	d := ds[t]
	switch {
	case !d.res.Visible:
		return nil, ErrNotFound
	case !d.whole():
		return nil, ErrForbidden
	}
	item, m, err := s.downloadItem(ctx, k, t)
	if err != nil {
		return nil, err
	}
	z := private(k, k.Download)
	var pages []media.File
	if z != nil && z.Zip != "" {
		if pages, err = zipPages(k, z, m); err != nil {
			return nil, err
		}
		if len(pages) == 0 {
			return nil, ErrNoDownload
		}
	}
	title, code := s.title(ctx, v)
	g, err := s.ask(ctx, actor, v, title, d.res.Editor, len(pages))
	if err != nil {
		return nil, err
	}
	ttl := cmp.Or(s.o.Downloads.TTL, DefaultDownloadTTL)
	var f media.File
	var name string
	if z != nil && z.Zip != "" {
		name = fileName(".zip", g.name, archiveStem(title, z), v.Content)
		if len(g.mark) > 0 {
			if err := s.charge(ctx, actor); err != nil {
				return nil, err
			}
			exp := time.Unix(s.o.Now().Add(ttl).Unix(), 0)
			marks := joinPages(g.mark)
			q := url.Values{"for": {actor.AccountID()}, "mark": {marks}, "name": {name},
				"ticket": {s.m.Reader.SignTicket(archiveScope(s.o.Tenant, t, actor.AccountID(), marks, name), exp)}}
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
		name = fileName(cmp.Or(layout.DownloadExt(f.Type), path.Ext(f.Path)), g.name, title+" ["+code+"]", v.Content)
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

// Archive opens the marked archive named name a Download granted viewer for
// t, by its ticket: the pages zipped as they stream, the policy marking those
// mark names. A ticket that is expired or not for viewer, t, mark and name is
// ErrNotFound.
func (s *Service) Archive(ctx context.Context, t Target, viewer, mark, name, ticket string) (*Archive, error) {
	pages, ok := splitPages(mark)
	if s.m.Reader == nil || s.o.Policy == nil || viewer == "" || name == "" || !ok ||
		s.m.Reader.VerifyTicket(ticket, archiveScope(s.o.Tenant, t, viewer, mark, name)) != nil {
		return nil, ErrNotFound
	}
	_, k, err := s.downloadKind(ctx, t)
	if err != nil {
		return nil, err
	}
	item, m, err := s.downloadItem(ctx, k, t)
	if err != nil {
		return nil, err
	}
	z := private(k, k.Download)
	if z == nil || z.Zip == "" {
		return nil, ErrNotFound
	}
	files, err := zipPages(k, z, m)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, ErrNoDownload
	}
	a := &Archive{Name: name, ctx: ctx, s: s, v: t, who: access.Actor{ID: viewer, Kind: auth.SubjectUser}}
	for _, p := range pages {
		a.mark = append(a.mark, p-1)
	}
	for _, f := range files {
		key, err := item.Blob(f.Blob)
		if err != nil {
			return nil, err
		}
		a.keys = append(a.keys, key)
	}
	if _, err := s.m.Manifests.Store().Head(ctx, a.keys[0]); err != nil {
		return nil, err
	}
	return a, nil
}

// grant is a policy's grant of a download: the pages (from 1) to mark and
// the file name without its extension ("" for ContentKit's).
type grant struct {
	mark []int
	name string
}

// ask asks the host's policy about actor's download of v, titled title, of
// pages pages (0 for a file): no marks for a file or a signed-out viewer. An
// UnavailableError when it refuses.
func (s *Service) ask(ctx context.Context, actor access.Actor, v version, title string, editor bool, pages int) (grant, error) {
	if s.o.Policy == nil {
		return grant{}, nil
	}
	r := DownloadRequest{Actor: actor, Editor: editor, Target: v.Target, Kind: v.Kind, Language: v.Language, Title: title, Pages: pages}
	if !actor.Anonymous && actor.ID != "" && s.o.Gate != nil {
		f, err := s.o.Gate.Filter(ctx, actor)
		if err != nil {
			return grant{}, err
		}
		r.Tiers = slices.DeleteFunc(slices.Clone(s.o.Kinds.Tiers), func(t string) bool { return !f.Tier(t) })
	}
	var content access.Rule
	var override *access.Rule
	var released *time.Time
	if err := s.o.Pool.QueryRow(ctx, `SELECT c.access, v.access, GREATEST(c.published_at, v.published_at)
FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id
WHERE v.tenant_id = $1 AND v.content_id = $2 AND v.id = $3`, s.o.Tenant, v.Content, v.Version).Scan(&content, &override, &released); err != nil {
		return grant{}, err
	}
	r.Rule = access.Effective(content, override)
	if released != nil {
		r.Released = *released
	}
	var err error
	if r.Groups, err = s.downloadGroups(ctx, v); err != nil {
		return grant{}, err
	}
	d, err := s.o.Policy.Decide(ctx, r)
	if err != nil {
		return grant{}, err
	}
	if !d.Allowed {
		return grant{}, &UnavailableError{Reason: d.Reason, AvailableAt: d.AvailableAt}
	}
	g := grant{name: d.Name}
	for _, p := range d.Mark {
		if p >= 1 && p <= pages && !slices.Contains(g.mark, p) && actor.Member() {
			g.mark = append(g.mark, p)
		}
	}
	slices.Sort(g.mark)
	return g, nil
}

// downloadGroups are v's active groups, its own and its content's, each
// named in v's language, else the site's default, else another, else by its
// slug, with its attributes.
func (s *Service) downloadGroups(ctx context.Context, v version) ([]DownloadGroup, error) {
	rows, err := s.o.Pool.Query(ctx, `SELECT DISTINCT g.kind, g.slug, COALESCE((SELECT n.name FROM `+pgxIdent(s.o.Schema, "content_group_names")+` n
 WHERE n.tenant_id = g.tenant_id AND n.group_id = g.id AND n.kind = 'name' ORDER BY n.language = $4 DESC, n.language = $5 DESC, n.language LIMIT 1), g.slug),
 g.attributes
FROM `+pgxIdent(s.o.Schema, "content_memberships")+` m JOIN `+s.t.groups+` g ON g.tenant_id = m.tenant_id AND g.id = m.group_id AND g.state = 'active' AND NOT g.internal
WHERE m.tenant_id = $1 AND m.content_id = $2 AND (m.version_id IS NULL OR m.version_id = $3) AND m.state = 'active'
AND (m.active_from IS NULL OR m.active_from <= now()) AND (m.active_until IS NULL OR m.active_until > now())
ORDER BY g.kind, g.slug`, s.o.Tenant, v.Content, v.Version, v.Language, s.o.Kinds.Languages[0])
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DownloadGroup, error) {
		var g DownloadGroup
		return g, row.Scan(&g.Kind, &g.Slug, &g.Name, &g.Attributes)
	})
}

// archiveScope is the scope of viewer's ticket to t's archive marked at
// mark and named name; its ':' keeps it apart from every media item and
// download scope.
func archiveScope(tenant string, t Target, viewer, mark, name string) string {
	return "archive:" + tenant + "/" + t.Content + "/" + t.Version + "/" + viewer + "/" + mark + "/" + name
}

// joinPages is "3,5,7".
func joinPages(pages []int) string {
	out := make([]string, len(pages))
	for i, p := range pages {
		out[i] = strconv.Itoa(p)
	}
	return strings.Join(out, ",")
}

// splitPages parses joinPages.
func splitPages(s string) ([]int, bool) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		p, err := strconv.Atoi(f)
		if err != nil || p < 1 {
			return nil, false
		}
		out = append(out, p)
	}
	return out, len(out) > 0
}

// downloadKind is t's version and its kind's version view, which serves a
// download.
func (s *Service) downloadKind(ctx context.Context, t Target) (version, *media.Kind, error) {
	v, err := s.one(ctx, t)
	if err != nil {
		return version{}, nil, err
	}
	mk, err := s.reg.Kind(v.Kind)
	if err != nil || !mk.Versions || mk.Download == "" || s.m.Reader == nil {
		return version{}, nil, ErrNotFound
	}
	return v, mk, nil
}

// downloadItem is t's media item and manifest under k.
func (s *Service) downloadItem(ctx context.Context, k *media.Kind, t Target) (media.Item, *media.Manifest, error) {
	ref, err := s.ref(k.Name, t)
	if err != nil {
		return media.Item{}, nil, err
	}
	item, err := s.reg.Item(ref)
	if err != nil {
		return media.Item{}, nil, err
	}
	m, _, err := s.m.Manifests.Get(ctx, ref)
	if errors.Is(err, media.ErrNotFound) {
		return media.Item{}, nil, ErrNoDownload
	} else if err != nil {
		return media.Item{}, nil, err
	}
	if m.Meta[forkMeta] != nil {
		return media.Item{}, nil, ErrNotReady
	}
	return item, m, nil
}

// zipPages are the files the archive zips, in manifest order: each page's
// original or output under the zip's prefix. ErrNotReady while one is missing.
func zipPages(k *media.Kind, z *media.Private, m *media.Manifest) ([]media.File, error) {
	var out []media.File
	for _, f := range m.Files {
		if !f.IsUpload() || f.Unattached || f.Gone || !inPattern(k.Publish.Pages, f.Path) || !media.PageIncluded(f) {
			continue
		}
		if f.Fail() != nil || f.Blob == "" {
			return nil, ErrNotReady
		}
		if strings.HasPrefix(f.Path, z.Zip) {
			out = append(out, f)
			continue
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

// title is v's title (its own, else its content's in its language, else in
// another), else its content's id, and its version code.
func (s *Service) title(ctx context.Context, v version) (title, code string) {
	_ = s.o.Pool.QueryRow(ctx, `SELECT COALESCE(NULLIF(vt.title, ''), ct.title, ''), cv.code FROM `+s.t.items+` c
JOIN `+s.t.versions+` cv ON cv.tenant_id = c.tenant_id AND cv.id = $3
LEFT JOIN `+pgxIdent(s.o.Schema, "content_text")+` ct ON ct.tenant_id = c.tenant_id AND ct.content_id = c.id
LEFT JOIN `+pgxIdent(s.o.Schema, "content_version_text")+` vt ON vt.tenant_id = c.tenant_id AND vt.version_id = $3 AND vt.language = ct.language
WHERE c.tenant_id = $1 AND c.id = $2 ORDER BY ct.language = $4 DESC, ct.language LIMIT 1`, s.o.Tenant, v.Content, v.Version, v.Language).Scan(&title, &code)
	if strings.TrimSpace(title) == "" {
		title = v.Content
	}
	return title, code
}

// archiveStem is the zip's Download template with title, less ".zip".
func archiveStem(title string, z *media.Private) string {
	return strings.TrimSuffix(strings.ReplaceAll(cmp.Or(z.Download, "{title}.zip"), "{title}", title), ".zip")
}

// fileName is the first of names something is left of once cleaned and cut
// to fit ext, with ext.
func fileName(ext string, names ...string) string {
	for _, n := range names {
		if out := layout.DownloadName(media.CleanName(n), ext); out != "" {
			return out
		}
	}
	return ""
}

// WriteTo streams the archive within its request's context.
func (a *Archive) WriteTo(w io.Writer) (int64, error) {
	return 0, a.s.streamMarked(a.ctx, w, a)
}

// streamMarked zips the pages one at a time, the policy marking those its
// ticket names; the archive closes only once every mark succeeded.
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
			marked, ext, err := s.o.Policy.Mark(ctx, a.who, a.v, i+1, body)
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
