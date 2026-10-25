package critical_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/notify"
	ck "github.com/open-rails/contentkit"
)

// These fixture helpers call the account owner's real publisher and HTTP API.
// They preserve the former integration assertions after ContentKit's inbox API
// removal; no response, identity, authorization decision or storage is mocked.
type inboxPublication struct{ Notifications []notify.Input }
type inboxPublished struct{ Items []notify.Item }
type inboxQuery struct{ Dismissed *bool }

func (a *application) publishInbox(ctx context.Context, input inboxPublication) (inboxPublished, error) {
	publisher, err := a.inbox.Notifications("application")
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
		page, err := a.inbox.ListNotifications(ctx, value.Recipient.ID, notify.ListOptions{Limit: 100})
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

type accountInboxClient struct {
	base, token string
	authorize   func(*http.Request)
}

func (a *application) accountInbox(token string) *accountInboxClient {
	return &accountInboxClient{base: a.inboxServer.URL + a.inbox.APIBase() + "/me/notifications", token: token, authorize: func(r *http.Request) { authtest.Authorize(a.test, a.tenant, r, token) }}
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
