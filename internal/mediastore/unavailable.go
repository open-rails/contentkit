package mediastore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	media "github.com/open-rails/contentkit/media"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// UnavailableSnooze is how long a job waits after the store was unreachable.
var UnavailableSnooze = 30 * time.Second

// MaxOutageSnoozes caps the snoozes of one job (a day at UnavailableSnooze);
// past it, outage errors spend attempts again.
var MaxOutageSnoozes = 2880

// unavailableCheck bounds the fresh bucket check that confirms an outage.
const unavailableCheck = 5 * time.Second

// SnoozeUnavailable turns a real store outage into a River snooze, which does
// not count as an attempt, so an outage longer than the retry budget never
// discards a job. It snoozes only when the job's own context is still live
// (a job that outran its timeout spends its attempt) and a fresh, bounded
// store.Check fails too (one bad object on a healthy bucket spends its
// attempt). Only actual outage snoozes count toward MaxOutageSnoozes;
// successful video checkpoint yields do not consume this budget. Other errors pass through.
func SnoozeUnavailable(ctx context.Context, store media.Store, job *rivertype.JobRow, err error) error {
	if store == nil || !errors.Is(err, media.ErrUnavailable) || ctx.Err() != nil || outageSnoozes(job) >= MaxOutageSnoozes {
		return err
	}
	checkCtx, cancel := context.WithTimeout(ctx, unavailableCheck)
	defer cancel()
	if store.Check(checkCtx) == nil {
		return err
	}
	if setErr := river.MetadataSet(ctx, "contentkit_outage_snoozes", outageSnoozes(job)+1); setErr != nil {
		return errors.Join(err, setErr)
	}
	return river.JobSnooze(UnavailableSnooze)
}

func outageSnoozes(job *rivertype.JobRow) int {
	var meta struct {
		Snoozes int `json:"contentkit_outage_snoozes"`
	}
	if job != nil {
		_ = json.Unmarshal(job.Metadata, &meta)
	}
	return meta.Snoozes
}
