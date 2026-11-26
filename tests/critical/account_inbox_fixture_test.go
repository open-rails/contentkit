package critical_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/notify"
	ck "github.com/open-rails/contentkit"
)

// These fixture helpers call the app's inbox owner: its real publisher, its
// HTTP API and AuthKit's inbox storage through its app view. No response,
// identity, authorization decision or storage is mocked.
type inboxPublication struct{ Notifications []notify.Input }
type inboxPublished struct{ Items []notify.Item }
type inboxQuery struct{ Dismissed *bool }

func (a *application) publishInbox(ctx context.Context, input inboxPublication) (inboxPublished, error) {
	publisher, err := a.inbox.Notifications(authkit.NotificationProducer{Name: "application", Origin: notify.OriginScope{System: "critical", Scope: a.cfg.Scope.AppID.String()}})
	if err != nil {
		return inboxPublished{}, err
	}
	batch := make([]notify.Publication, len(input.Notifications))
	for i, value := range input.Notifications {
		batch[i] = notify.Publication{RecipientID: value.Recipient.ID, ID: value.Origin.ID, Revision: 1,
			Kind: value.Kind, Data: value.Data, CreatedAt: value.CreatedAt, ActionRequested: value.ActionRequested, Severity: value.Severity}
	}
	if err := publisher.Publish(ctx, batch); err != nil {
		return inboxPublished{}, err
	}
	var out inboxPublished
	for _, value := range input.Notifications {
		page, err := a.inboxStore.List(ctx, value.Recipient, notify.ListOptions{Limit: 100})
		if err != nil {
			return out, err
		}
		for _, item := range page.Items {
			if item.Origin.System == "critical" && item.Origin.ID == value.Origin.ID {
				out.Items = append(out.Items, item)
			}
		}
	}
	return out, nil
}

// inboxItems is recipient's inbox as the app's inbox owner shows it, read from
// AuthKit's storage: a purged account's is empty.
func (a *application) inboxItems(t *testing.T, recipient string) []notify.Item {
	t.Helper()
	page, err := a.inboxStore.List(t.Context(), notify.Recipient{Audience: notify.AudienceUser, ID: recipient}, notify.ListOptions{Limit: 100})
	must(t, err)
	return page.Items
}

type accountInboxClient struct {
	base      string
	authorize func(*http.Request)
}

// accountInbox is the HTTP inbox of token's account at the app's inbox owner,
// which admits only sessions it issued: the account signs in there once.
func (a *application) accountInbox(token string) *accountInboxClient {
	session := a.inboxSession(token)
	return &accountInboxClient{base: a.inboxServer.URL + a.inbox.APIBase() + "/me/notifications", authorize: func(r *http.Request) { authtest.Authorize(a.test, a.inbox, r, session) }}
}

func (a *application) inboxSession(token string) string {
	subject := tokenClaims(a.test, token).Subject
	if session, ok := a.inboxSessions.Load(subject); ok {
		return session.(string)
	}
	user, ok := a.world.people.Load(subject)
	if !ok {
		a.test.Fatalf("no fixture account %s to sign in at the inbox owner", subject)
	}
	session := authtest.SignIn(a.test, a.inbox, user.(authtest.User)).AccessToken
	a.inboxSessions.Store(subject, session)
	return session
}

// sessionClaims are a signed-in access token's account and session.
type sessionClaims struct {
	Subject   string `json:"sub"`
	SessionID string `json:"sid"`
}

// tokenClaims reads a signed-in access token's claims. A DPoP-bound token
// verifies only beside a proof, so fixtures read its payload instead.
func tokenClaims(t testing.TB, token string) sessionClaims {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("fixture access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	must(t, err)
	var claims sessionClaims
	must(t, json.Unmarshal(payload, &claims))
	if claims.Subject == "" || claims.SessionID == "" {
		t.Fatal("fixture access token names no account session")
	}
	return claims
}

func (c *accountInboxClient) call(ctx context.Context, method, path string, out any) error {
	r, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return err
	}
	c.authorize(r)
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var body iam.ErrorEnvelope
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			return fmt.Errorf("AuthKit notification response %d: %w", response.StatusCode, err)
		}
		return &ck.Error{Status: response.StatusCode, Code: body.Error.Code, Message: body.Error.Message}
	}
	return json.NewDecoder(response.Body).Decode(out)
}

func (c *accountInboxClient) List(ctx context.Context, query inboxQuery) (notify.Page, error) {
	path := ""
	if query.Dismissed != nil && *query.Dismissed {
		path = "?dismissed=true"
	}
	var out notify.Page
	err := c.call(ctx, http.MethodGet, path, &out)
	return out, err
}

func (c *accountInboxClient) SetRead(ctx context.Context, id string, read bool) (notify.Item, error) {
	return c.mutate(ctx, id, "read", read)
}

func (c *accountInboxClient) Get(ctx context.Context, id string) (notify.Item, error) {
	var out notify.Item
	err := c.call(ctx, http.MethodGet, "/"+url.PathEscape(id), &out)
	return out, err
}

func (c *accountInboxClient) SetDismissed(ctx context.Context, id string, dismissed bool) (notify.Item, error) {
	return c.mutate(ctx, id, "dismiss", dismissed)
}

func (c *accountInboxClient) mutate(ctx context.Context, id, action string, enabled bool) (notify.Item, error) {
	method := http.MethodDelete
	if enabled {
		method = http.MethodPut
	}
	var out notify.Item
	err := c.call(ctx, method, "/"+url.PathEscape(id)+"/"+action, &out)
	return out, err
}

func publicationFrom(input notify.Input, revision int64) notify.Publication {
	return notify.Publication{RecipientID: input.Recipient.ID, ID: input.Origin.ID, Revision: revision, Kind: input.Kind, Data: input.Data, CreatedAt: input.CreatedAt, ActionRequested: input.ActionRequested, Severity: input.Severity}
}
