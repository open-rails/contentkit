package critical_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/notify"
	"github.com/open-rails/authkit/userinfo"
	ck "github.com/open-rails/contentkit"
	ckbilling "github.com/open-rails/contentkit/adapters/openrails"
	"github.com/open-rails/contentkit/media/layout"
	notification "github.com/open-rails/contentkit/migration/legacyinbox"
	notice "github.com/open-rails/contentkit/notification/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/openrailstest/stripemock"
)

type billingInboxWorld struct {
	w                        *world
	a                        *application
	b                        *realBilling
	source                   *notice.Source
	mapper                   notice.RecipientMapper
	store                    *notification.Store
	schema, token, recipient string
	page                     *billing.EventPage
}

func newBillingInboxWorld(t *testing.T) *billingInboxWorld {
	t.Helper()
	i := &billingInboxWorld{schema: "inbox_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]}
	t.Cleanup(func() {
		if i.w == nil {
			return
		}
		// A separate administrative connection outlives world-owned pools.
		pool, err := pgxpool.New(context.Background(), i.w.pool.Config().ConnString())
		must(t, err)
		defer pool.Close()
		_, err = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{i.schema}.Sanitize()+" CASCADE")
		must(t, err)
	})
	i.w = newWorld(t, func(c *ck.Config) { c.Accounts = &ck.AccountsConfig{Schema: i.schema} })
	i.a = i.w.apps[0]
	i.b = attachBilling(t, i.w, i.a)
	user, token := i.w.person(t, 0)
	i.token, i.recipient = token, user.ID
	product, err := i.b.merchant.CreateProduct(t.Context(), billing.CreateProductParams{Key: "inbox-purchase", DisplayName: "Inbox purchase"})
	must(t, err)
	price, err := i.b.merchant.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD"})
	must(t, err)
	total, reusable := int64(10_000_000), true
	input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: &total, Reusable: &reusable, Payment: &billing.OrderPaymentParams{Token: i.b.stripe.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
	body, err := json.Marshal(input)
	must(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, i.b.server.URL+"/billing/v1/me/orders", bytes.NewReader(body))
	must(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "inbox-source-purchase")
	authtest.Authorize(t, i.w.tenant[0], request, i.token)
	response, err := i.b.server.Client().Do(request)
	must(t, err)
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	must(t, err)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("purchase: %d %s", response.StatusCode, raw)
	}
	var order billing.Order
	must(t, json.Unmarshal(raw, &order))
	if order.Status != billing.OrderComplete {
		t.Fatalf("source order did not settle: %+v", order)
	}
	i.page, err = i.b.merchant.ListEvents(t.Context(), billing.EventQuery{Limit: 100})
	must(t, err)
	notices := 0
	for _, event := range i.page.Events {
		if event.Source == "notification" && event.Type == "one_off_purchase_completed" {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("settled order must emit one retained purchase notice, got %d: %+v", notices, i.page.Events)
	}
	i.source, err = ckbilling.NewNotificationSource(i.b.merchant, "critical-billing")
	must(t, err)
	i.mapper = ckbilling.SameAccounts(i.a.deps.Users, authtest.Issuer)
	// This profile exists only to qualify the explicit legacy handoff. Runtime
	// billing publication no longer attaches a ContentKit polling worker.
	i.a.server.Close()
	must(t, i.a.engine.Close(t.Context()))
	i.a.engine, err = ck.New(t.Context(), i.a.cfg, i.a.deps)
	must(t, err)
	i.a.server = mountEngine(t, i.a.engine)
	i.store = i.newStore(t, i.w.pool)
	must(t, i.store.BindMerchant(t.Context(), i.a.cfg.Scope.TenantID.String(), i.source.Origin()))
	return i
}

func (i *billingInboxWorld) newStore(t *testing.T, pool *pgxpool.Pool) *notification.Store {
	t.Helper()
	must(t, notification.Migrate(t.Context(), pool, i.schema))
	directory, ok := i.a.deps.Users.(interface {
		GetTx(context.Context, pgx.Tx, []string) (map[string]userinfo.User, error)
	})
	if !ok {
		t.Fatal("real AuthKit directory lacks transaction-bound lookup")
	}
	store, err := notification.New(notification.Options{Pool: pool, Schema: i.w.schema, Tenant: i.a.cfg.Scope.AppID.String(), AppID: i.a.cfg.Scope.AppID.String(), Shared: &notification.Config{Schema: i.schema, Namespace: (layout.Scope{TenantID: i.a.cfg.Scope.TenantID.String()}).Partition(), SharedOrigins: []notify.OriginScope{i.source.Origin()}}, AccountExists: func(ctx context.Context, id string) (bool, error) {
		users, err := i.a.deps.Users.Get(ctx, []string{id})
		return users[id].ID == id, err
	}, AccountExistsTx: func(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
		users, err := directory.GetTx(ctx, tx, []string{id})
		return users[id].ID == id, err
	}})
	must(t, err)
	return store
}

func inboxConsumeOptions() notice.ConsumeOptions {
	return notice.ConsumeOptions{Limit: 100, IncludeOptional: true, Filter: func(e notice.Event) bool { return e.EventType == "one_off_purchase_completed" }}
}

func (i *billingInboxWorld) receipts(t *testing.T) []notify.Item {
	t.Helper()
	page, err := i.store.List(t.Context(), notify.Recipient{Audience: notify.AudienceUser, ID: i.recipient}, notify.ListOptions{})
	must(t, err)
	return page.Items
}

func (i *billingInboxWorld) requireSourceUnchanged(t *testing.T) {
	t.Helper()
	page, err := i.b.merchant.ListEvents(t.Context(), billing.EventQuery{Limit: 100})
	must(t, err)
	if !reflect.DeepEqual(i.page, page) {
		t.Fatal("destination consumption changed the retained source page")
	}
}

// Both consumers read the real saved cursor before either starts its write.
// This controls scheduling only; publication and CAS remain the real Store.
type inboxReadBarrier struct {
	*notification.Store
	read           atomic.Int32
	ready, release chan struct{}
}

func (b *inboxReadBarrier) EventCursor(ctx context.Context, origin notify.OriginScope) (string, error) {
	cursor, err := b.Store.EventCursor(ctx, origin)
	if err != nil {
		return "", err
	}
	if b.read.Add(1) == 2 {
		close(b.ready)
	}
	select {
	case <-b.release:
		return cursor, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestBillingInboxAtomicReplay(t *testing.T) {
	t.Run("CK-J25-S06_two_consumers_commit_one_receipt_and_cursor", func(t *testing.T) {
		i := newBillingInboxWorld(t)
		barrier := &inboxReadBarrier{Store: i.store, ready: make(chan struct{}), release: make(chan struct{})}
		var release sync.Once
		t.Cleanup(func() { release.Do(func() { close(barrier.release) }) })
		type outcome struct {
			result notice.ConsumeResult
			err    error
		}
		done := make(chan outcome, 2)
		for range 2 {
			go func() {
				result, err := i.source.Consume(t.Context(), barrier, i.mapper, inboxConsumeOptions())
				done <- outcome{result, err}
			}()
		}
		select {
		case <-barrier.ready:
		case <-time.After(10 * time.Second):
			t.Fatal("both consumers did not reach the real cursor barrier")
		}
		release.Do(func() { close(barrier.release) })
		delivered := 0
		for range 2 {
			out := <-done
			must(t, out.err)
			delivered += out.result.Delivered
		}
		if delivered != 1 {
			t.Fatalf("concurrent import committed %d notices", delivered)
		}
		cursor, err := i.store.EventCursor(t.Context(), i.source.Origin())
		must(t, err)
		if cursor != i.page.NextCursor {
			t.Fatal("receipt and complete source-page continuation did not commit together")
		}
		receipts := i.receipts(t)
		if len(receipts) != 1 || receipts[0].Kind != "one_off_purchase_completed" {
			t.Fatalf("wrong imported inbox: %+v", receipts)
		}
		fresh, err := ckbilling.NewNotificationSource(i.b.merchant, "critical-billing")
		must(t, err)
		again, err := fresh.Consume(t.Context(), i.newStore(t, i.w.pool), i.mapper, inboxConsumeOptions())
		must(t, err)
		if again.Delivered != 0 || !reflect.DeepEqual(receipts, i.receipts(t)) {
			t.Fatal("fresh consumer replay changed the inbox")
		}
		i.requireSourceUnchanged(t)
	})
}

// An owned PostgreSQL wire proxy withholds only the real COMMIT acknowledgement.
// No database/business response is invented; the server has already committed.
type inboxCommitProxy struct {
	listener          net.Listener
	mu                sync.Mutex
	connections       []net.Conn
	once              sync.Once
	committed, closed chan struct{}
}

func newInboxCommitProxy(t *testing.T, upstream string) *inboxCommitProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	p := &inboxCommitProxy{listener: listener, committed: make(chan struct{}), closed: make(chan struct{})}
	t.Cleanup(p.close)
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			server, err := net.DialTimeout("tcp", upstream, 5*time.Second)
			if err != nil {
				client.Close()
				continue
			}
			p.mu.Lock()
			p.connections = append(p.connections, client, server)
			p.mu.Unlock()
			go func() {
				defer client.Close()
				defer server.Close()
				go func() { _, _ = io.Copy(server, client) }()
				for {
					header := make([]byte, 5)
					if _, err := io.ReadFull(server, header); err != nil {
						return
					}
					length := int(binary.BigEndian.Uint32(header[1:])) - 4
					if length < 0 || length > 16<<20 {
						return
					}
					body := make([]byte, length)
					if _, err := io.ReadFull(server, body); err != nil {
						return
					}
					if header[0] == 'C' && string(body) == "COMMIT\x00" {
						p.once.Do(func() { close(p.committed) })
						<-p.closed
						return
					}
					if _, err := client.Write(append(header, body...)); err != nil {
						return
					}
				}
			}()
		}
	}()
	return p
}
func (p *inboxCommitProxy) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.closed:
		return
	default:
		close(p.closed)
	}
	p.listener.Close()
	for _, connection := range p.connections {
		connection.Close()
	}
}

func TestBillingInboxCommitFailure(t *testing.T) {
	for _, boundary := range []string{"before-commit", "after-commit-before-ack"} {
		t.Run("CK-J25-S07/"+boundary, func(t *testing.T) {
			i := newBillingInboxWorld(t)
			config := i.w.pool.Config().Copy()
			config.MaxConns = 1
			var proxy *inboxCommitProxy
			if boundary == "after-commit-before-ack" {
				if config.ConnConfig.TLSConfig != nil {
					t.Fatal("owned commit proxy requires the disposable plaintext PostgreSQL fixture")
				}
				proxy = newInboxCommitProxy(t, net.JoinHostPort(config.ConnConfig.Host, fmt.Sprint(config.ConnConfig.Port)))
				address := proxy.listener.Addr().(*net.TCPAddr)
				config.ConnConfig.Host = "127.0.0.1"
				config.ConnConfig.Port = uint16(address.Port)
			}
			pool, err := pgxpool.NewWithConfig(t.Context(), config)
			must(t, err)
			t.Cleanup(func() {
				if proxy != nil {
					proxy.close()
				}
				pool.Close()
			})
			store := i.newStore(t, pool)
			var backend int32
			must(t, pool.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&backend))
			var lock *pgxpool.Conn
			if boundary == "before-commit" {
				lock, err = i.w.pool.Acquire(t.Context())
				must(t, err)
				_, err = lock.Exec(t.Context(), "SELECT pg_advisory_lock(hashtextextended($1,0))", i.schema)
				must(t, err)
				defer func() {
					_, _ = lock.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtextextended($1,0))", i.schema)
					lock.Release()
				}()
				q := pgx.Identifier{i.schema}.Sanitize()
				_, err = i.w.pool.Exec(t.Context(), `CREATE FUNCTION `+q+`.hold_cursor_commit() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN PERFORM pg_advisory_xact_lock(hashtextextended(TG_TABLE_SCHEMA,0)); RETURN NEW; END$$;
CREATE CONSTRAINT TRIGGER hold_cursor_commit AFTER UPDATE ON `+q+`.notification_event_cursors DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION `+q+`.hold_cursor_commit()`)
				must(t, err)
			}
			done := make(chan error, 1)
			go func() { _, err := i.source.Consume(t.Context(), store, i.mapper, inboxConsumeOptions()); done <- err }()
			if boundary == "before-commit" {
				waitFor(t, func() bool {
					var waiting bool
					err := i.w.pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')", backend).Scan(&waiting)
					must(t, err)
					return waiting
				})
				var terminated bool
				must(t, i.w.pool.QueryRow(t.Context(), "SELECT pg_terminate_backend($1)", backend).Scan(&terminated))
				if !terminated {
					t.Fatal("owned consumer database transaction was not terminated")
				}
				q := pgx.Identifier{i.schema}.Sanitize()
				_, err = i.w.pool.Exec(t.Context(), "DROP TRIGGER hold_cursor_commit ON "+q+".notification_event_cursors; DROP FUNCTION "+q+".hold_cursor_commit()")
				must(t, err)
			} else {
				select {
				case <-proxy.committed:
				case <-time.After(10 * time.Second):
					t.Fatal("real database COMMIT acknowledgement was not reached")
				}
				proxy.close()
			}
			if err := <-done; err == nil {
				t.Fatal("interrupted consumer unexpectedly received successful acknowledgement")
			}
			cursor, err := i.store.EventCursor(t.Context(), i.source.Origin())
			must(t, err)
			before := i.receipts(t)
			if boundary == "before-commit" && (cursor != "" || len(before) != 0) {
				t.Fatal("failed transaction left a cursor or receipt")
			}
			if boundary == "after-commit-before-ack" && (cursor != i.page.NextCursor || len(before) != 1) {
				t.Fatal("committed transaction lost its receipt or continuation")
			}
			fresh, err := ckbilling.NewNotificationSource(i.b.merchant, "critical-billing")
			must(t, err)
			result, err := fresh.Consume(t.Context(), i.newStore(t, i.w.pool), i.mapper, inboxConsumeOptions())
			must(t, err)
			after := i.receipts(t)
			if len(after) != 1 {
				t.Fatalf("recovery produced %d receipts", len(after))
			}
			want := 1
			if boundary == "after-commit-before-ack" {
				want = 0
				if after[0].ID != before[0].ID {
					t.Fatal("recovery replaced a committed receipt")
				}
			}
			if result.Delivered != want {
				t.Fatalf("recovery delivered %d, want%d", result.Delivered, want)
			}
			i.requireSourceUnchanged(t)
		})
	}
}
