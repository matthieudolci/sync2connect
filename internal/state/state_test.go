package state

import (
	"os"
	"strings"
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

func TestEnsureWritable(t *testing.T) {
	if err := EnsureWritable(t.TempDir() + "/new/dir"); err != nil {
		t.Fatalf("writable dir rejected: %v", err)
	}

	if os.Getuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	err := EnsureWritable(dir)
	if err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("expected not-writable error, got %v", err)
	}
}
