// Package activitylog stamps the rows the API logs to ClickHouse's activity
// log: the subject, the site, the access tier, the local UTC offset and the
// time, all from the request.
package activitylog

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/contentkit/access"
	"github.com/open-rails/contentkit/contenturl"
	"github.com/open-rails/contentkit/internal/entitlement"
	"github.com/open-rails/contentkit/internal/gate"
	"github.com/open-rails/contentkit/internal/signalstore"
	"github.com/open-rails/contentkit/internal/subject"
	"github.com/open-rails/contentkit/kinds"
)

// Log stamps and queues one app's activity rows.
type Log struct {
	App   uuid.UUID
	Kinds kinds.Config
	Gate  *entitlement.Gate
	// Write queues rows; nil records nothing (no signal plane).
	Write func(rows ...signalstore.Row)
	Now   func() time.Time
}

// Row is a row stamped from r for actor, in language when it is configured
// (else the request's Accept-Language, else the site's first); false when
// the request has no subject or nothing is recorded.
func (l *Log) Row(r *http.Request, actor access.Actor, language string) (signalstore.Row, bool) {
	if l == nil || l.Write == nil {
		return signalstore.Row{}, false
	}
	who, ok := subject.FromRequest(l.App, actor, r)
	if !ok {
		return signalstore.Row{}, false
	}
	now := l.now()
	domain, _ := l.Kinds.DomainFor(gate.Domain(r.Context()))
	return signalstore.Row{Subject: who, At: now, Domain: domain.Name, Language: l.language(r, language),
		Access: l.access(actor), Offset: subject.Offset(r, now)}, true
}

// Add queues rows.
func (l *Log) Add(rows ...signalstore.Row) {
	if l != nil && l.Write != nil && len(rows) > 0 {
		l.Write(rows...)
	}
}

func (l *Log) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

func (l *Log) language(r *http.Request, given string) string {
	given = strings.ToLower(strings.TrimSpace(given))
	if slices.Contains(l.Kinds.Languages, given) {
		return given
	}
	if lang := contenturl.AcceptLanguage(l.Kinds.Languages, r.Header.Get("Accept-Language")); lang != "" {
		return lang
	}
	if len(l.Kinds.Languages) > 0 {
		return l.Kinds.Languages[0]
	}
	return ""
}

// access is anonymous, account, or the account's claimed tiers.
func (l *Log) access(actor access.Actor) string {
	if !actor.Member() {
		return signalstore.AccessAnonymous
	}
	if l.Gate != nil {
		if tiers := l.Gate.ClaimedTiers(actor); len(tiers) > 0 {
			return strings.Join(tiers, ",")
		}
	}
	return signalstore.AccessAccount
}
