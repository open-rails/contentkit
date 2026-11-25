package versionstore

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/internal/clientaddr"
	entitlement "github.com/open-rails/contentkit/internal/entitlement"
	mediastore "github.com/open-rails/contentkit/internal/mediastore"
	"github.com/open-rails/contentkit/internal/ratelimit"
	"github.com/open-rails/contentkit/media"
	"github.com/open-rails/contentkit/media/layout"
	versions "github.com/open-rails/contentkit/media/versions"
	"github.com/redis/go-redis/v9"
)

type quotas struct {
	anonymous, user *ratelimit.Limiter
	tiers           []string
	byTier          map[string]*ratelimit.Limiter
}

// newQuotas builds d's limiters; rdb shares the counts across replicas, nil
// keeps them per process.
func newQuotas(d versions.Downloads, rdb redis.UniversalClient, tiers []string, log *slog.Logger, now func() time.Time) (*quotas, error) {
	build := func(name string, qs []versions.Quota) (*ratelimit.Limiter, error) {
		if len(qs) == 0 {
			return nil, nil
		}
		ws := make([]ratelimit.Window, len(qs))
		for i, q := range qs {
			ws[i] = ratelimit.Window{Limit: q.Limit, Per: q.Per}
		}
		l, err := ratelimit.New(ratelimit.Options{Windows: ws, Redis: rdb, Prefix: "contentkit:downloads:" + name + ":", Logger: log, Now: now})
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

// charge takes one download from actor's bucket.
func (s *Service) charge(ctx context.Context, actor access.Actor) error {
	bucket, key, l := "anonymous", "ip:"+clientaddr.Key(actor.IP), s.dl.anonymous
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
		return &versions.QuotaError{Bucket: bucket, RetryAfter: wait}
	}
	return nil
}

// Archive is an authorized ZIP of a version's released pages, streamed from
// their originals (or a page's oversize download rendition) in reading
// order. Its bytes depend only on its entries: every request for the same
// revision gets the same bytes, so an unmarked ZIP has a known Size and can
// be cached. Marked pages are personalized: such a ZIP is never cached.
type Archive struct {
	Name     string // the attachment's file name
	Type     string // application/zip
	Size     int64  // the ZIP's length; 0 when pages are marked
	Revision string // the entries' revision: the ZIP's ETag
	ctx      context.Context
	s        *Service
	entries  []zipEntry
	mark     []int // pages from 0
	v        versions.Target
	who      access.Actor
}

// Marked reports a personalized ZIP.
func (a *Archive) Marked() bool { return len(a.mark) > 0 }

// zipEntry is one page of a ZIP.
type zipEntry struct {
	Name string
	Key  string
	Blob string
	Size int64
	Type string
}

// Download grants t's download to actor, what its kind's download serves
// (kinds.Kind.Download), as the host's policy allows, and charges it to
// actor's quota: a ZIP of the released pages the viewer may read, or the
// version's original, released into private/ and served by the gateway
// under the item's token, named {title}{ext}. ErrNotFound when the kind
// serves none, an UnavailableError when the policy refuses, ErrNoDownload
// when the version has nothing to serve, ErrNotReady before its release, a
// QuotaError past the quota.
func (s *Service) Download(ctx context.Context, actor access.Actor, t versions.Target) (*versions.FileDownload, error) {
	v, k, err := s.downloadKind(ctx, t)
	if err != nil {
		return nil, err
	}
	ds, err := s.decisions(ctx, actor, v.Kind, []versions.Target{t}, nil)
	if err != nil {
		return nil, err
	}
	d := ds[t]
	if !d.res.Visible {
		return nil, ErrNotFound
	}
	pages := k.Publish != nil && k.Download == k.Publish.Pages
	if pages && downloadablePages(d, math.MaxInt) == 0 || !pages && !d.whole() {
		return nil, ErrForbidden
	}
	view, m, err := s.releasedView(ctx, k, t)
	if errors.Is(err, mediastore.ErrInvalidManifest) {
		return nil, ErrNoDownload // unreadable: nothing is charged
	} else if err != nil {
		return nil, err
	}
	ds, err = s.decisions(ctx, actor, v.Kind, []versions.Target{t}, map[versions.Target]pagesSnapshot{t: view.Pages})
	if err != nil {
		return nil, err
	}
	if d = ds[t]; !d.res.Visible {
		return nil, ErrNotFound
	}
	title, _ := s.title(ctx, v)
	ttl := cmp.Or(s.o.Downloads.TTL, versions.DefaultDownloadTTL)
	if pages {
		entries, err := s.zipEntries(k, t, view, m)
		if err != nil {
			return nil, err
		}
		entries = entries[:downloadablePages(d, len(entries))]
		if len(entries) == 0 {
			return nil, ErrNoDownload
		}
		g, err := s.ask(ctx, actor, v, title, d.res.Editor, len(entries))
		if err != nil {
			return nil, err
		}
		name := fileName(".zip", g.name, title, v.Content)
		if err := s.charge(ctx, actor); err != nil {
			return nil, err
		}
		u, exp := s.transferURL(actor, versions.ArchiveRequest{Target: t, Editor: d.res.Editor, Mark: joinPages(g.mark), Name: name, Revision: archiveRevision(entries), Pages: len(entries)}, ttl)
		return &versions.FileDownload{URL: u, Name: name, Type: "application/zip", ExpiresAt: exp}, nil
	}
	var f media.File
	ok := false
	for _, file := range m.Files {
		if file.IsUpload() && uploadStem(file.Path) == k.Download {
			f, ok = file, true
			break
		}
	}
	if !ok || f.Unattached || f.Gone || f.Fail() != nil || zipUpload(f) {
		return nil, ErrNoDownload
	}
	if layout.BlobArea(f.Blob) != layout.AreaPrivate {
		return nil, ErrNotReady // released by the next move
	}
	g, err := s.ask(ctx, actor, v, title, d.res.Editor, 0)
	if err != nil {
		return nil, err
	}
	name := fileName(cmp.Or(path.Ext(f.Path), layout.DownloadExt(f.Type)), g.name, title, v.Content)
	if err := s.charge(ctx, actor); err != nil {
		return nil, err
	}
	ref, err := s.ref(k.Name, t)
	if err != nil {
		return nil, err
	}
	item, err := s.reg.Item(ref)
	if err != nil {
		return nil, err
	}
	u, exp, err := s.m.Reader.DownloadURL(item, f, name, ttl)
	if err != nil {
		return nil, err
	}
	return &versions.FileDownload{URL: u, Name: name, Type: f.Type, Size: f.Size, ExpiresAt: exp}, nil
}

func (s *Service) transferURL(actor access.Actor, r versions.ArchiveRequest, ttl time.Duration) (string, time.Time) {
	r.Viewer, r.AccountSet, r.Kind = actor.ID, actor.Account != nil, actor.Kind
	if actor.Account != nil {
		r.Account = actor.Account.ID
	}
	r.Claims = archiveTokenClaims(actor, s.o.Kinds.TokenClaims)
	exp := time.Unix(s.o.Now().Add(ttl).Unix(), 0)
	q := url.Values{"for": {r.Viewer}, "account": {r.Account}, "account_set": {strconv.FormatBool(r.AccountSet)}, "kind": {string(r.Kind)}, "editor": {strconv.FormatBool(r.Editor)}, "mark": {r.Mark}, "name": {r.Name}, "revision": {r.Revision}, "pages": {strconv.Itoa(r.Pages)}, "ticket": {s.m.Reader.SignTicket(archiveScope(s.o.Tenant, r), exp)}}
	if len(r.Claims) > 0 {
		encoded, _ := json.Marshal(r.Claims)
		q.Set("claims", string(encoded))
	}
	return "archive?" + q.Encode(), exp
}

// downloadablePages counts the released pages the viewer may download.
func downloadablePages(d decision, total int) int {
	if !d.res.Visible || !d.res.Accessible && !d.res.Editor {
		return 0
	}
	return total
}

// Archive opens the ZIP a Download granted. The ticket binds its viewer,
// marks, name and exact ordered revision. Access, release and policy are
// checked again.
func (s *Service) Archive(ctx context.Context, request versions.ArchiveRequest) (*Archive, error) {
	t, viewer, mark, name, revision := request.Target, request.Viewer, request.Mark, request.Name, request.Revision
	marked, ok := splitPages(mark)
	if mark == "" {
		ok = true
	}
	if s.m.Reader == nil || name == "" || revision == "" || !ok || request.File {
		return nil, ErrNotFound
	}
	if s.m.Reader.VerifyTicket(request.Ticket, archiveScope(s.o.Tenant, request)) != nil {
		return nil, ErrNotFound
	}
	v, k, err := s.downloadKind(ctx, t)
	if err != nil {
		return nil, err
	}
	if k.Publish == nil || k.Download != k.Publish.Pages {
		return nil, ErrNotFound
	}
	view, m, err := s.releasedView(ctx, k, t)
	if err != nil {
		return nil, err
	}
	actor := access.Actor{ID: viewer, Kind: request.Kind, Anonymous: viewer == ""}
	if request.AccountSet {
		actor.Account = &access.AccountMembership{ID: request.Account}
	}
	if !actor.Anonymous && len(request.Claims) > 0 {
		// The ticket above verifies these claims for this target and revision.
		actor.Verified = archiveClaimProof{identity: auth.Identity{Subject: viewer, SubjectKind: request.Kind}, claims: slices.Clone(request.Claims)}
	}
	ds, err := s.decisions(ctx, actor, k.Name, []versions.Target{t}, map[versions.Target]pagesSnapshot{t: view.Pages})
	if err != nil {
		return nil, err
	}
	d := ds[t]
	if request.Editor {
		// A plain attachment navigation cannot replay an SPA's bearer header.
		// The HMAC delegates the already-authorized edit/read capability only
		// for this target and revision. Soft deletion still revokes the URL.
		var active bool
		if err := s.o.Pool.QueryRow(ctx, `SELECT c.deleted_at IS NULL AND v.deleted_at IS NULL
FROM `+s.t.items+` c JOIN `+s.t.versions+` v ON v.tenant_id=c.tenant_id AND v.content_id=c.id
WHERE c.tenant_id=$1 AND c.id=$2 AND v.id=$3`, s.o.Tenant, t.Content, t.Version).Scan(&active); err != nil {
			return nil, err
		}
		if !active {
			return nil, ErrNotFound
		}
		d.res.Visible, d.res.Editor = true, true
	}
	if !d.res.Visible {
		return nil, ErrNotFound
	}
	entries, err := s.zipEntries(k, t, view, m)
	if err != nil {
		return nil, err
	}
	allowed := downloadablePages(d, len(entries))
	if request.Pages == 0 {
		if !d.whole() {
			return nil, ErrForbidden
		}
		request.Pages = len(entries)
	}
	if request.Pages < 0 || request.Pages > allowed {
		return nil, ErrForbidden
	}
	entries = entries[:request.Pages]
	if archiveRevision(entries) != revision {
		return nil, ErrNotFound // obtain a fresh grant after any source edit
	}
	title, _ := s.title(ctx, v)
	g, err := s.ask(ctx, actor, v, title, d.res.Editor, len(entries))
	if err != nil {
		return nil, err
	}
	if !slices.Equal(g.mark, marked) {
		return nil, ErrForbidden
	}
	a := &Archive{Name: name, Type: "application/zip", Revision: revision, ctx: ctx, s: s, v: t, who: actor, entries: entries}
	for _, p := range marked {
		a.mark = append(a.mark, p-1)
	}
	if !a.Marked() {
		if a.Size, err = zipSize(entries); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// zipEntries are the ZIP's entries: each released page's original in
// reading order, or its oversize download rendition (Publish.Download)
// when it has one, named as uploaded.
func (s *Service) zipEntries(k *mediastore.RegisteredKind, t versions.Target, view *releaseView, m *mediastore.Manifest) ([]zipEntry, error) {
	ref, err := s.ref(k.Name, t)
	if err != nil {
		return nil, err
	}
	item, err := s.reg.Item(ref)
	if err != nil {
		return nil, err
	}
	var out []zipEntry
	for _, p := range view.Pages.Pages {
		f, ok := m.Get(p.Path)
		if !ok || f.Blob == "" || f.Gone || f.Key() != p.Source {
			return nil, ErrNotReady
		}
		src := f
		if k.Publish.Download != "" {
			if outs := m.Outputs(f.Path, k.Publish.Download); len(outs) == 1 && outs[0].Blob != "" {
				src = outs[0]
			}
		}
		key, err := item.Blob(src.Blob)
		if err != nil {
			return nil, err
		}
		name := path.Base(f.Path)
		if src.Preset != "" {
			name = strings.TrimSuffix(name, path.Ext(name)) + path.Ext(src.Path)
		}
		out = append(out, zipEntry{Name: name, Key: key, Blob: src.Blob, Size: src.Size, Type: src.Type})
	}
	return out, nil
}

// archiveRevision covers every input that changes the ZIP's bytes: the
// recipe (stored entries, no timestamps) and each entry's name and bytes.
func archiveRevision(entries []zipEntry) string {
	type entry struct{ Blob, Name string }
	list := make([]entry, len(entries))
	for i, e := range entries {
		list[i] = entry{e.Blob, e.Name}
	}
	body, _ := json.Marshal(struct {
		Recipe string
		Files  []entry
	}{"reader-zip-3", list})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// zipHeader is every entry's header: stored, untimed, so the bytes depend
// only on the entries.
func zipHeader(name string) *zip.FileHeader {
	return &zip.FileHeader{Name: name, Method: zip.Store}
}

// zipSize is the ZIP's exact length, computed by writing its structure over
// zeros of each entry's size: lengths never depend on the bytes.
func zipSize(entries []zipEntry) (int64, error) {
	var n countingWriter
	zw := zip.NewWriter(&n)
	for _, e := range entries {
		w, err := zw.CreateHeader(zipHeader(e.Name))
		if err != nil {
			return 0, err
		}
		if _, err := io.CopyN(w, zeros{}, e.Size); err != nil {
			return 0, err
		}
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	return int64(n), nil
}

type countingWriter int64

func (c *countingWriter) Write(p []byte) (int, error) {
	*c += countingWriter(len(p))
	return len(p), nil
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// WriteTo streams the archive within its request's context.
func (a *Archive) WriteTo(w io.Writer) (int64, error) {
	out := &archiveWriter{ctx: a.ctx, Writer: w}
	err := a.s.streamZip(a.ctx, out, a)
	return out.n, err
}

// WriteRange streams bytes [start, end] of an unmarked archive: the ZIP is
// produced and the bytes before start are skipped.
func (a *Archive) WriteRange(w io.Writer, start, end int64) (int64, error) {
	out := &archiveWriter{ctx: a.ctx, Writer: &rangeWriter{w: w, skip: start, left: end - start + 1}}
	err := a.s.streamZip(a.ctx, out, a)
	if errors.Is(err, errRangeDone) {
		err = nil
	}
	return out.n, err
}

var errRangeDone = errors.New("versions: range written")

type rangeWriter struct {
	w          io.Writer
	skip, left int64
}

func (r *rangeWriter) Write(p []byte) (int, error) {
	n := len(p)
	if r.skip > 0 {
		k := min(int64(len(p)), r.skip)
		r.skip -= k
		p = p[k:]
	}
	if len(p) > 0 && r.left > 0 {
		k := min(int64(len(p)), r.left)
		if _, err := r.w.Write(p[:k]); err != nil {
			return 0, err
		}
		r.left -= k
	}
	if r.left <= 0 {
		return n, errRangeDone
	}
	return n, nil
}

// streamZip zips the pages one at a time, the policy marking those its
// ticket names; the archive closes only once every mark succeeded.
func (s *Service) streamZip(ctx context.Context, w io.Writer, a *Archive) error {
	zw := zip.NewWriter(w)
	for i, e := range a.entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		rc, obj, err := s.m.Manifests.Store().Get(ctx, e.Key, media.GetOptions{})
		if err != nil {
			return err
		}
		name := e.Name
		var body io.Reader = rc
		if slices.Contains(a.mark, i) {
			raw, err := io.ReadAll(rc)
			if err != nil {
				rc.Close()
				return err
			}
			marked, ext, err := s.o.Policy.Mark(ctx, a.who, a.v, i+1, raw)
			if err != nil {
				rc.Close()
				return fmt.Errorf("versions: mark page %d: %w", i, err)
			}
			body, name = bytes.NewReader(marked), strings.TrimSuffix(name, path.Ext(name))+ext
		} else if obj.Size != e.Size {
			rc.Close()
			return fmt.Errorf("versions: page %s holds %d bytes, its manifest %d", e.Key, obj.Size, e.Size)
		}
		entry, err := zw.CreateHeader(zipHeader(name))
		if err == nil {
			_, err = io.Copy(entry, body)
		}
		closeErr := rc.Close()
		if err != nil || closeErr != nil {
			return errors.Join(err, closeErr)
		}
	}
	return zw.Close()
}

type archiveWriter struct {
	ctx context.Context
	io.Writer
	n int64
}

func (w *archiveWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.Writer.Write(p)
	w.n += int64(n)
	return n, err
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
	r := versions.DownloadRequest{Actor: actor, Editor: editor, Target: v.Target, Kind: v.Kind, Language: v.Language, Title: title, Pages: pages}
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
	if err := s.o.Pool.QueryRow(ctx, `SELECT c.access, coalesce(v.released_access,v.access),
GREATEST(coalesce(c.published_at,c.publish_at), coalesce(v.published_at,v.publish_at))
FROM `+s.t.versions+` v JOIN `+s.t.items+` c ON c.tenant_id = v.tenant_id AND c.id = v.content_id
WHERE v.tenant_id = $1 AND v.content_id = $2 AND v.id = $3`, s.o.Tenant, v.Content, v.Version).Scan(&content, &override, &released); err != nil {
		return grant{}, err
	}
	r.Rule = entitlement.Effective(content, override)
	if released != nil {
		r.Released = *released
	}
	var err error
	if r.Entities, err = s.downloadEntities(ctx, v); err != nil {
		return grant{}, err
	}
	d, err := s.o.Policy.Decide(ctx, r)
	if err != nil {
		return grant{}, err
	}
	if !d.Allowed {
		return grant{}, &versions.UnavailableError{Reason: d.Reason, AvailableAt: d.AvailableAt}
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

// downloadEntities are v's active entities, its own and its content's, each
// named in v's language, else the site's default, else another, else by its
// slug, with its attributes.
func (s *Service) downloadEntities(ctx context.Context, v version) ([]versions.DownloadEntity, error) {
	rows, err := s.o.Pool.Query(ctx, `SELECT DISTINCT g.kind, g.slug, COALESCE((SELECT n.name FROM `+pgxIdent(s.o.Schema, "content_entity_names")+` n
 WHERE n.tenant_id = g.tenant_id AND n.entity_id = g.id AND n.kind = 'name' ORDER BY n.language = $4 DESC, n.language = $5 DESC, n.language LIMIT 1), g.slug),
 g.attributes
FROM `+pgxIdent(s.o.Schema, "content_memberships")+` m JOIN `+s.t.entities+` g ON g.tenant_id = m.tenant_id AND g.id = m.entity_id AND g.state = 'active' AND NOT g.internal
WHERE m.tenant_id = $1 AND m.content_id = $2 AND (m.version_id IS NULL OR m.version_id = $3) AND m.state = 'active'
ORDER BY g.kind, g.slug`, s.o.Tenant, v.Content, v.Version, v.Language, s.o.Kinds.Languages[0])
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (versions.DownloadEntity, error) {
		var g versions.DownloadEntity
		return g, row.Scan(&g.Kind, &g.Slug, &g.Name, &g.Attributes)
	})
}

// archiveScope binds identity, tenant membership, delegated editor authority
// and every output selector without ambiguous delimiter concatenation.
func archiveScope(tenant string, r versions.ArchiveRequest) string {
	claims := r.Claims
	if len(claims) == 0 {
		claims = nil
	}
	b, _ := json.Marshal([]any{tenant, r.Content, r.Version, r.Viewer, r.Account, r.AccountSet, r.Kind, r.Editor, r.Mark, r.Name, r.Revision, r.Pages, claims})
	if r.File {
		return "file-download:" + string(b)
	}
	return "archive:" + string(b)
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

// downloadKind is t's version and its kind, which must serve a download.
func (s *Service) downloadKind(ctx context.Context, t versions.Target) (version, *mediastore.RegisteredKind, error) {
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

// title is this version's localized title, then its original title, then
// its content ID. A download never takes a sibling edition's title.
func (s *Service) title(ctx context.Context, v version) (title, code string) {
	_ = s.o.Pool.QueryRow(ctx, `SELECT COALESCE(NULLIF(vt.title, ''), cv.original_title, ''), cv.code FROM `+s.t.versions+` cv
LEFT JOIN `+pgxIdent(s.o.Schema, "content_version_text")+` vt ON vt.tenant_id = cv.tenant_id AND vt.version_id = cv.id
WHERE cv.tenant_id = $1 AND cv.content_id = $2 AND cv.id = $3
ORDER BY vt.language = $4 DESC NULLS LAST, vt.language LIMIT 1`, s.o.Tenant, v.Content, v.Version, v.Language).Scan(&title, &code)
	if strings.TrimSpace(title) == "" {
		title = v.Content
	}
	return title, code
}

// fileName is the first of names something is left of once cleaned and cut
// to fit ext, with ext.
func fileName(ext string, names ...string) string {
	for _, n := range names {
		if out := layout.DownloadName(mediastore.CleanName(n), ext); out != "" {
			return out
		}
	}
	return ""
}
