package state

import (
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !store.LastSync("withings->garmin").IsZero() {
		t.Fatal("fresh store should have zero last sync")
	}

	when := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	if err := store.SetLastSync("withings->garmin", when); err != nil {
		t.Fatal(err)
	}

	// Reopen and verify persistence.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.LastSync("withings->garmin"); !got.Equal(when) {
		t.Fatalf("LastSync = %v, want %v", got, when)
	}
	if !reopened.LastSync("other->route").IsZero() {
		t.Fatal("unknown route should be zero")
	}
}

func TestOpenCreatesDir(t *testing.T) {
	dir := t.TempDir() + "/nested/state"
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
}
