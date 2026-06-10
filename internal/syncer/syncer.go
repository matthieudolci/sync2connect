// Package syncer runs measurements from a source provider to a destination
// provider, tracking progress in the state store so each run only transfers
// new or updated data.
package syncer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/matthieudolci/sync2connect/internal/provider"
	"github.com/matthieudolci/sync2connect/internal/state"
)

// watermarkOverlap is re-fetched on every run to tolerate clock skew between
// this machine and the source provider's servers. Destinations dedupe by
// measurement timestamp, so the overlap is harmless.
const watermarkOverlap = 5 * time.Minute

// Route syncs one source to one destination.
type Route struct {
	Source      provider.Source
	Destination provider.Destination
	State       *state.Store
	// InitialLookback bounds the first sync when no state exists yet.
	InitialLookback time.Duration
	Log             *slog.Logger
}

// Key identifies the route in the state store.
func (r *Route) Key() string {
	return r.Source.Name() + "->" + r.Destination.Name()
}

// Run performs one sync pass: fetch everything since the last successful
// run, push it, and record the new watermark.
func (r *Route) Run(ctx context.Context) error {
	log := r.Log.With("route", r.Key())

	since := r.State.LastSync(r.Key())
	if since.IsZero() {
		since = time.Now().Add(-r.InitialLookback)
		log.Info("no previous sync state, using initial lookback", "since", since.Format(time.RFC3339))
	} else {
		since = since.Add(-watermarkOverlap)
	}

	// The watermark is taken before fetching so measurements that arrive
	// while we run are picked up next time.
	start := time.Now()

	measurements, err := r.Source.FetchBody(ctx, since)
	if err != nil {
		return fmt.Errorf("fetching from %s: %w", r.Source.Name(), err)
	}
	if len(measurements) == 0 {
		log.Info("no new measurements")
	} else {
		if err := r.Destination.PushBody(ctx, measurements); err != nil {
			return fmt.Errorf("pushing to %s: %w", r.Destination.Name(), err)
		}
		log.Info("synced measurements", "count", len(measurements),
			"oldest", measurements[0].Timestamp.Format(time.RFC3339),
			"newest", measurements[len(measurements)-1].Timestamp.Format(time.RFC3339))
	}

	if err := r.State.SetLastSync(r.Key(), start); err != nil {
		return fmt.Errorf("saving sync state: %w", err)
	}
	return nil
}
