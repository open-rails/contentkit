package contentkit

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/contentkit/contentref"
	"github.com/open-rails/contentkit/internal/hub"
)

// forgottenContent names purged content: a whole work (VersionID empty) or
// one version of it.
type forgottenContent struct{ Kind, ContentID, VersionID string }

// forgetPurgedTx removes purged works from everyone's reading state and
// records the activity log's erasure with the catalog mutation. The ordinary
// content purge sweep drains those records after deleting content.
func (e *Engine) forgetPurgedTx(ctx context.Context, tx pgx.Tx, refs []contentref.ContentRef) error {
	if len(refs) == 0 {
		return nil
	}
	contents := make([]forgottenContent, 0, len(refs))
	var works []uuid.UUID
	for _, ref := range refs {
		if ref.TenantID != e.cfg.Scope.AppID.String() {
			return errors.New("contentkit: purge cleanup crossed catalog scope")
		}
		contents = append(contents, forgottenContent{Kind: ref.ContentKind, ContentID: ref.ContentID, VersionID: ref.Version()})
		if id, err := uuid.Parse(ref.ContentID); err == nil && ref.Version() == "" {
			works = append(works, id)
		}
	}
	if err := e.reading.ForgetContentsTx(ctx, tx, works); err != nil {
		return err
	}
	if !e.purgeSignalsConfigured() {
		return nil
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

func (e *Engine) insertForgottenTx(ctx context.Context, tx pgx.Tx, contents []forgottenContent) error {
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
 (tenant_id,content_kind,content_id,content_version_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, e.cfg.Scope.AppID.String(), ref.Kind, ref.ContentID, ref.VersionID); err != nil {
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
 WHERE tenant_id=$1 ORDER BY content_kind,content_id,content_version_id LIMIT 200 FOR UPDATE SKIP LOCKED`, e.cfg.Scope.AppID.String())
			if err != nil {
				return err
			}
			refs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (forgottenContent, error) {
				var ref forgottenContent
				err := row.Scan(&ref.Kind, &ref.ContentID, &ref.VersionID)
				return ref, err
			})
			if err != nil || len(refs) == 0 {
				return err
			}
			count = len(refs)
			store := e.hub.Signals()
			if store == nil {
				return hub.ErrSignalPlaneDown
			}
			// The activity log and its rollups are per work: a purged version
			// leaves its work's rows.
			var works []uuid.UUID
			for _, ref := range refs {
				if id, err := uuid.Parse(ref.ContentID); err == nil && ref.VersionID == "" {
					works = append(works, id)
				}
			}
			if err := store.ForgetContents(ctx, e.cfg.Scope.AppID.String(), works); err != nil {
				return err
			}
			kinds, contents, versions := make([]string, count), make([]string, count), make([]string, count)
			for i, ref := range refs {
				kinds[i], contents[i], versions[i] = ref.Kind, ref.ContentID, ref.VersionID
			}
			_, err = tx.Exec(ctx, `DELETE FROM `+table+` p USING unnest($2::text[],$3::text[],$4::text[]) refs(kind,content,version)
 WHERE p.tenant_id=$1 AND p.content_kind=refs.kind AND p.content_id=refs.content AND p.content_version_id=refs.version`, e.cfg.Scope.AppID.String(), kinds, contents, versions)
			return err
		})
		if err != nil || count < 200 {
			return err
		}
	}
}
