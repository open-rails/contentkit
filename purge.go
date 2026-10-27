package contentkit

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/signal"
)

// forgetPurgedTx records required signal erasure with the catalog mutation.
// The ordinary content purge sweep drains these records after deleting content.
func (e *Engine) forgetPurgedTx(ctx context.Context, tx pgx.Tx, refs []contentref.ContentRef) error {
	if !e.purgeSignalsConfigured() || len(refs) == 0 {
		return nil
	}
	contents := make([]signal.ForgottenContent, 0, len(refs))
	for _, ref := range refs {
		if ref.TenantID != e.cfg.Tenant {
			return errors.New("contentkit: purge cleanup crossed catalog scope")
		}
		contents = append(contents, signal.ForgottenContent{Kind: ref.ContentKind, ContentID: ref.ContentID, VersionID: ref.Version()})
	}
	return e.insertForgottenTx(ctx, tx, contents)
}

func (e *Engine) purgeSignalsConfigured() bool {
	if e.hub != nil {
		return e.hub.SignalsConfigured()
	}
	// Explicit migrations have no Hub/Deps. Database also covers an injected
	// ClickHouse connection, whose required database remains in Config.
	return strings.TrimSpace(e.cfg.ClickHouse.Database) != "" || strings.TrimSpace(e.cfg.ClickHouse.Addr) != ""
}

func (e *Engine) insertForgottenTx(ctx context.Context, tx pgx.Tx, contents []signal.ForgottenContent) error {
	if len(contents) == 0 {
		return nil
	}
	if tx == nil {
		return errors.New("contentkit: signal erasure requires the catalog transaction")
	}
	table := pgx.Identifier{e.schema, "content_signal_purges"}.Sanitize()
	for _, ref := range contents {
		if strings.TrimSpace(ref.Kind) == "" || strings.TrimSpace(ref.ContentID) == "" {
			return errors.New("contentkit: signal erasure needs a kind and content ID")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+table+`
 (tenant_id,content_kind,content_id,content_version_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, e.cfg.Tenant, ref.Kind, ref.ContentID, ref.VersionID); err != nil {
			return err
		}
	}
	return nil
}

// purgeSignals is part of the content-deletion sweep, including when media is
// disabled. Rows survive the content itself and remain due until ClickHouse
// confirms erasure. Only these cleanup rows are locked over the remote call;
// a signal outage does not prevent the sweep from purging other content.
func (e *Engine) purgeSignals(ctx context.Context) error {
	if e.hub == nil || !e.hub.SignalsConfigured() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	table := pgx.Identifier{e.schema, "content_signal_purges"}.Sanitize()
	for {
		count := 0
		err := pgx.BeginFunc(ctx, e.pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT content_kind,content_id,content_version_id FROM `+table+`
 WHERE tenant_id=$1 ORDER BY content_kind,content_id,content_version_id LIMIT 200 FOR UPDATE SKIP LOCKED`, e.cfg.Tenant)
			if err != nil {
				return err
			}
			refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (signal.ForgottenContent, error) {
				var ref signal.ForgottenContent
				err := row.Scan(&ref.Kind, &ref.ContentID, &ref.VersionID)
				return ref, err
			})
			if err != nil || len(refs) == 0 {
				return err
			}
			count = len(refs)
			if err := e.hub.ForgetContents(ctx, refs); err != nil {
				return err
			}
			kinds, contents, versions := make([]string, count), make([]string, count), make([]string, count)
			for i, ref := range refs {
				kinds[i], contents[i], versions[i] = ref.Kind, ref.ContentID, ref.VersionID
			}
			_, err = tx.Exec(ctx, `DELETE FROM `+table+` p USING unnest($2::text[],$3::text[],$4::text[]) refs(kind,content,version)
 WHERE p.tenant_id=$1 AND p.content_kind=refs.kind AND p.content_id=refs.content AND p.content_version_id=refs.version`, e.cfg.Tenant, kinds, contents, versions)
			return err
		})
		if err != nil || count < 200 {
			return err
		}
	}
}
