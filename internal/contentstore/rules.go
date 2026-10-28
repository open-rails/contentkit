package contentstore

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/contentkit/access"
	content "github.com/open-rails/contentkit/content"
	"github.com/open-rails/contentkit/internal/httpapi"
	"golang.org/x/text/unicode/norm"
)

const (
	maxBannedWord = 100 // runes
)

const (
	rulesTTL = 30 * time.Second
)

const (
	maxRuleBatch = 500
)

// rules caches the banned words; a local write reloads them at once, other
// replicas within rulesTTL. A failed reload keeps the previous set.
type rules struct {
	rt      *Runtime
	mu      sync.Mutex
	fetched time.Time
	loaded  bool
	mask    *regexp.Regexp
	reject  *regexp.Regexp
}

func newRules(rt *Runtime) *rules { return &rules{rt: rt} }

// boundary keeps a banned word from matching inside a longer word.
const boundary = `[^\p{L}\p{N}_]`

func (r *rules) load(ctx context.Context) (mask, reject *regexp.Regexp, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded && time.Since(r.fetched) < rulesTTL {
		return r.mask, r.reject, nil
	}
	s := r.rt.store
	rows, err := s.pool.Query(ctx, `SELECT word, action FROM `+s.t.bannedWords+` WHERE tenant_id = $1 ORDER BY word`, s.tenant)
	if err == nil {
		words := map[content.WordAction][]string{}
		for rows.Next() {
			var w string
			var a content.WordAction
			if err = rows.Scan(&w, &a); err != nil {
				break
			}
			words[a] = append(words[a], regexp.QuoteMeta(foldWord(w)))
		}
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
		if err == nil {
			compile := func(ws []string) *regexp.Regexp {
				if len(ws) == 0 {
					return nil
				}
				return regexp.MustCompile(`(?:^|` + boundary + `)(?:` + strings.Join(ws, "|") + `)(?:$|` + boundary + `)`)
			}
			r.mask, r.reject, r.loaded = compile(words[content.WordMask]), compile(words[content.WordReject]), true
		}
	}
	r.fetched = time.Now()
	if err != nil && !r.loaded {
		return nil, nil, err
	}
	if err != nil {
		r.rt.log.WarnContext(ctx, "content: banned words reload failed; keeping the previous set", "err", err)
	}
	return r.mask, r.reject, nil
}

func (r *rules) invalidate() {
	r.mu.Lock()
	r.fetched = time.Time{}
	r.mu.Unlock()
}

// foldWord is the form words and text match in: compatibility-folded, lower
// case, without zero-width characters.
func foldWord(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\u200B', '\u200C', '\u200D', '\uFEFF':
			return -1
		}
		return r
	}, norm.NFKC.String(s))
	return strings.ToLower(strings.TrimSpace(s))
}

var tokenRe = regexp.MustCompile(`\s+|\S+`)

// apply refuses text containing a reject word and masks every
// whitespace-separated word containing a mask word, keeping the rest of the
// text as written.
func (r *rules) apply(ctx context.Context, text string) (string, error) {
	mask, reject, err := r.load(ctx)
	if err != nil {
		return "", err
	}
	if reject != nil && reject.MatchString(foldWord(text)) {
		return "", content.RejectedError{Reason: "content violates policy"}
	}
	return masked(text, mask), nil
}

// maskOnly masks the mask words in text without refusing it: imported history
// is kept, only cleaned.
func (r *rules) maskOnly(ctx context.Context, text string) (string, error) {
	mask, _, err := r.load(ctx)
	if err != nil {
		return "", err
	}
	return masked(text, mask), nil
}

func masked(text string, mask *regexp.Regexp) string {
	if mask == nil {
		return text
	}
	var b strings.Builder
	for _, tok := range tokenRe.FindAllString(text, -1) {
		if strings.TrimSpace(tok) != "" && mask.MatchString(foldWord(tok)) {
			tok = strings.Repeat("*", utf8.RuneCountInString(tok))
		}
		b.WriteString(tok)
	}
	return b.String()
}

// checkIPBan refuses a text write from a banned address, unless the actor
// moderates comments.
func (rt *Runtime) checkIPBan(ctx context.Context, actor access.Actor) error {
	addr, err := netip.ParseAddr(actor.IP)
	if err != nil {
		return nil
	}
	n := content.BanNotice{Scope: content.ScopeIP}
	err = rt.store.pool.QueryRow(ctx, `SELECT reason, until FROM `+rt.store.t.ipBans+`
		WHERE tenant_id = $1 AND network >>= $2::inet AND (until IS NULL OR until > now())
		ORDER BY until DESC NULLS FIRST LIMIT 1`, rt.tenant, addr.Unmap().String()).Scan(&n.Reason, &n.Until)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if actor.Person() && rt.requirePerm(ctx, actor, access.CommentsModerate) == nil {
		return nil
	}
	return &content.BannedError{BanNotice: n}
}

func (rt *Runtime) bannedWords(ctx context.Context) ([]content.BannedWord, error) {
	s := rt.store
	rows, err := s.pool.Query(ctx, `SELECT word, action, created_by, created_at FROM `+s.t.bannedWords+` WHERE tenant_id = $1 ORDER BY word`, s.tenant)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (content.BannedWord, error) {
		var w content.BannedWord
		return w, row.Scan(&w.Word, &w.Action, &w.CreatedBy, &w.CreatedAt)
	})
}

// addBannedWords adds words, or changes the action of ones already banned.
func (rt *Runtime) addBannedWords(ctx context.Context, by string, in []content.BannedWordInput) error {
	if len(in) == 0 || len(in) > maxRuleBatch {
		return badRequest("1-%d words", maxRuleBatch)
	}
	words, actions := make([]string, len(in)), make([]string, len(in))
	for i, w := range in {
		words[i] = foldWord(w.Word)
		if words[i] == "" || utf8.RuneCountInString(words[i]) > maxBannedWord {
			return badRequest("a banned word is 1-%d characters", maxBannedWord)
		}
		if w.Action == "" {
			w.Action = content.WordMask
		}
		if w.Action != content.WordMask && w.Action != content.WordReject {
			return badRequest("action %q is mask or reject", w.Action)
		}
		actions[i] = string(w.Action)
	}
	s := rt.store
	tx, err := s.beginMutation(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := rt.lockErasureSubject(ctx, tx, by); err != nil {
		return err
	}
	if by, err = rt.provenance(ctx, tx, by); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.bannedWords+` (tenant_id, word, action, created_by)
		SELECT DISTINCT ON (w) $1, w, a, $4 FROM unnest($2::text[], $3::text[]) AS x(w, a)
		ON CONFLICT (tenant_id, word) DO UPDATE SET action = EXCLUDED.action`, s.tenant, words, actions, by); err != nil {
		return err
	}
	err = tx.Commit(ctx)
	rt.rules.invalidate()
	return err
}

func (rt *Runtime) removeBannedWords(ctx context.Context, words []string) error {
	if len(words) == 0 || len(words) > maxRuleBatch {
		return badRequest("1-%d words", maxRuleBatch)
	}
	for i := range words {
		words[i] = foldWord(words[i])
	}
	s := rt.store
	_, err := s.pool.Exec(ctx, `DELETE FROM `+s.t.bannedWords+` WHERE tenant_id = $1 AND word = ANY($2)`, s.tenant, words)
	rt.rules.invalidate()
	return err
}

func (rt *Runtime) ipBans(ctx context.Context) ([]content.IPBan, error) {
	s := rt.store
	rows, err := s.pool.Query(ctx, `SELECT network::text, reason, until, banned_by, banned_at, coalesce(until <= now(), false)
		FROM `+s.t.ipBans+` WHERE tenant_id = $1 ORDER BY banned_at DESC, network`, s.tenant)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (content.IPBan, error) {
		var b content.IPBan
		return b, row.Scan(&b.Network, &b.Reason, &b.Until, &b.BannedBy, &b.BannedAt, &b.Expired)
	})
}

// network parses an address or a CIDR network to its canonical network.
func network(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, badRequest("network %q is not an address or a CIDR network", s)
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func (rt *Runtime) banIPs(ctx context.Context, by string, in []content.IPBanInput) error {
	if len(in) == 0 || len(in) > maxRuleBatch {
		return badRequest("1-%d bans", maxRuleBatch)
	}
	nets, reasons, untils := make([]string, len(in)), make([]string, len(in)), make([]*time.Time, len(in))
	for i, b := range in {
		p, err := network(b.Network)
		if err != nil {
			return err
		}
		reason := strings.TrimSpace(b.Reason)
		if utf8.RuneCountInString(reason) > maxBanReason {
			return badRequest("reason exceeds %d characters", maxBanReason)
		}
		if b.Until != nil && !b.Until.After(time.Now()) {
			return badRequest("until must be in the future")
		}
		nets[i], reasons[i], untils[i] = p.String(), reason, b.Until
	}
	s := rt.store
	tx, err := s.beginMutation(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := rt.lockErasureSubject(ctx, tx, by); err != nil {
		return err
	}
	if by, err = rt.provenance(ctx, tx, by); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.t.ipBans+` (tenant_id, network, reason, until, banned_by)
		SELECT DISTINCT ON (n) $1, n::cidr, r, u, $5 FROM unnest($2::text[], $3::text[], $4::timestamptz[]) AS x(n, r, u)
		ON CONFLICT (tenant_id, network) DO UPDATE SET reason = EXCLUDED.reason, until = EXCLUDED.until,
		 banned_by = EXCLUDED.banned_by, banned_at = now()`, s.tenant, nets, reasons, untils, by); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (rt *Runtime) liftIPBans(ctx context.Context, networks []string) error {
	if len(networks) == 0 || len(networks) > maxRuleBatch {
		return badRequest("1-%d networks", maxRuleBatch)
	}
	nets := make([]string, len(networks))
	for i, n := range networks {
		p, err := network(n)
		if err != nil {
			return err
		}
		nets[i] = p.String()
	}
	s := rt.store
	_, err := s.pool.Exec(ctx, `DELETE FROM `+s.t.ipBans+` WHERE tenant_id = $1 AND network = ANY($2::cidr[])`, s.tenant, nets)
	return err
}

func ruleRoute(method, path, doc string, request any, reply httpapi.Reply, serve func(*Runtime, access.Actor, *http.Request) (any, error)) httpapi.Route[*Runtime] {
	return httpapi.Route[*Runtime]{Spec: httpapi.Spec{Method: method, Path: path, Resource: "moderation", Auth: httpapi.Staff, Perm: string(access.CommentsModerate),
		Doc: doc, Request: request, Responses: []httpapi.Reply{reply}},
		Serve: func(rt *Runtime) http.HandlerFunc {
			return func(w http.ResponseWriter, req *http.Request) {
				actor := rt.actor(req.Context())
				if err := rt.requirePerm(req.Context(), actor, access.CommentsModerate); err != nil {
					writeErr(w, err)
					return
				}
				out, err := serve(rt, actor, req)
				if err != nil {
					writeErr(w, err)
					return
				}
				writeJSON(w, reply.Status, out)
			}
		}}
}

var ruleRoutes = []httpapi.Route[*Runtime]{
	ruleRoute(httpapi.GET, "/moderation/banned-words", "The banned words and what each does.", nil, httpapi.OK([]content.BannedWord{}),
		func(rt *Runtime, _ access.Actor, req *http.Request) (any, error) {
			return rt.bannedWords(req.Context())
		}),
	ruleRoute(httpapi.POST, "/moderation/banned-words", "Bans words, or changes what banned ones do: mask (the default) or reject.",
		[]content.BannedWordInput{}, httpapi.OK([]content.BannedWord{}), func(rt *Runtime, actor access.Actor, req *http.Request) (any, error) {
			var in []content.BannedWordInput
			if err := decodeJSON(req, &in); err != nil {
				return nil, err
			}
			if err := rt.addBannedWords(req.Context(), actor.ID, in); err != nil {
				return nil, err
			}
			return rt.bannedWords(req.Context())
		}),
	ruleRoute(httpapi.DELETE, "/moderation/banned-words", "Unbans words.", []string{}, httpapi.NoContent,
		func(rt *Runtime, _ access.Actor, req *http.Request) (any, error) {
			var in []string
			if err := decodeJSON(req, &in); err != nil {
				return nil, err
			}
			return nil, rt.removeBannedWords(req.Context(), in)
		}),
	ruleRoute(httpapi.GET, "/moderation/ip-bans", "The IP bans, newest first, expired ones included.", nil, httpapi.OK([]content.IPBan{}),
		func(rt *Runtime, _ access.Actor, req *http.Request) (any, error) { return rt.ipBans(req.Context()) }),
	ruleRoute(httpapi.POST, "/moderation/ip-bans", "Bans addresses or networks from commenting, or replaces their bans.",
		[]content.IPBanInput{}, httpapi.OK([]content.IPBan{}), func(rt *Runtime, actor access.Actor, req *http.Request) (any, error) {
			var in []content.IPBanInput
			if err := decodeJSON(req, &in); err != nil {
				return nil, err
			}
			if err := rt.banIPs(req.Context(), actor.ID, in); err != nil {
				return nil, err
			}
			return rt.ipBans(req.Context())
		}),
	ruleRoute(httpapi.DELETE, "/moderation/ip-bans", "Lifts the bans of addresses or networks.", []string{}, httpapi.NoContent,
		func(rt *Runtime, _ access.Actor, req *http.Request) (any, error) {
			var in []string
			if err := decodeJSON(req, &in); err != nil {
				return nil, err
			}
			return nil, rt.liftIPBans(req.Context(), in)
		}),
}
