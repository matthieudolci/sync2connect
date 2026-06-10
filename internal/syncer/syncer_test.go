package syncer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/matthieudolci/sync2connect/internal/model"
	"github.com/matthieudolci/sync2connect/internal/state"
)

type fakeSource struct {
	since        time.Time
	measurements []model.BodyMeasurement
	err          error
}

func (s *fakeSource) Name() string { return "src" }
func (s *fakeSource) FetchBody(ctx context.Context, since time.Time) ([]model.BodyMeasurement, error) {
	s.since = since
	return s.measurements, s.err
}

type fakeDest struct {
	pushed [][]model.BodyMeasurement
	err    error
}

func (d *fakeDest) Name() string { return "dst" }
func (d *fakeDest) PushBody(ctx context.Context, ms []model.BodyMeasurement) error {
	d.pushed = append(d.pushed, ms)
	return d.err
}

func newRoute(t *testing.T, src *fakeSource, dst *fakeDest) *Route {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Route{
		Source:          src,
		Destination:     dst,
		State:           store,
		InitialLookback: 24 * time.Hour,
		Log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestRunSyncsAndAdvancesWatermark(t *testing.T) {
	src := &fakeSource{measurements: []model.BodyMeasurement{
		{Timestamp: time.Now().Add(-2 * time.Hour), WeightKg: 80},
		{Timestamp: time.Now().Add(-1 * time.Hour), WeightKg: 80.2},
	}}
	dst := &fakeDest{}
	route := newRoute(t, src, dst)

	before := time.Now()
	if err := route.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	// First run uses the initial lookback.
	wantSince := before.Add(-24 * time.Hour)
	if src.since.Before(wantSince.Add(-time.Minute)) || src.since.After(wantSince.Add(time.Minute)) {
		t.Fatalf("first since = %v, want ~%v", src.since, wantSince)
	}
	if len(dst.pushed) != 1 || len(dst.pushed[0]) != 2 {
		t.Fatalf("pushed = %+v", dst.pushed)
	}

	// Second run starts from the previous run's watermark minus the
	// clock-skew overlap.
	if err := route.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantSince = before.Add(-watermarkOverlap)
	if src.since.Before(wantSince.Add(-time.Minute)) || src.since.After(time.Now()) {
		t.Fatalf("second since = %v, want ~%v", src.since, wantSince)
	}
}

func TestRunNoMeasurementsStillAdvances(t *testing.T) {
	src := &fakeSource{}
	dst := &fakeDest{}
	route := newRoute(t, src, dst)

	if err := route.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(dst.pushed) != 0 {
		t.Fatal("nothing should be pushed")
	}
	if route.State.LastSync(route.Key()).IsZero() {
		t.Fatal("watermark should advance even with no data")
	}
}

func TestRunFetchErrorKeepsWatermark(t *testing.T) {
	src := &fakeSource{err: errors.New("boom")}
	route := newRoute(t, src, &fakeDest{})

	if err := route.Run(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if !route.State.LastSync(route.Key()).IsZero() {
		t.Fatal("watermark must not advance on fetch failure")
	}
}

func TestRunPushErrorKeepsWatermark(t *testing.T) {
	src := &fakeSource{measurements: []model.BodyMeasurement{{Timestamp: time.Now(), WeightKg: 80}}}
	dst := &fakeDest{err: errors.New("upload failed")}
	route := newRoute(t, src, dst)

	if err := route.Run(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if !route.State.LastSync(route.Key()).IsZero() {
		t.Fatal("watermark must not advance on push failure")
	}
}
