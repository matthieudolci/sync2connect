// Package state persists sync progress (the last successful sync time per
// route) as a small JSON file in the state directory.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type routeState struct {
	LastSync time.Time `json:"last_sync"`
}

type fileData struct {
	Routes map[string]routeState `json:"routes"`
}

// Store reads and writes sync state. It is safe for concurrent use.
type Store struct {
	path string

	mu   sync.Mutex
	data fileData
}

// EnsureWritable creates dir if needed and verifies the process can write
// to it, so misconfigured permissions surface at startup instead of after
// an OAuth flow has already consumed the user's authorization.
func EnsureWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	probe := filepath.Join(dir, ".writable")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return fmt.Errorf("state directory %s is not writable by uid %d "+
			"(in Docker: `chown -R 65532:65532` the mounted volume, or run the container with `user: \"$(id -u)\"`): %w",
			dir, os.Getuid(), err)
	}
	os.Remove(probe)
	return nil
}

// Open loads (or initializes) the state file inside dir, creating the
// directory if needed.
func Open(dir string) (*Store, error) {
	if err := EnsureWritable(dir); err != nil {
		return nil, err
	}
	s := &Store{
		path: filepath.Join(dir, "state.json"),
		data: fileData{Routes: map[string]routeState{}},
	}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading state: %w", err)
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("parsing state %s: %w", s.path, err)
	}
	if s.data.Routes == nil {
		s.data.Routes = map[string]routeState{}
	}
	return s, nil
}

// LastSync returns the recorded last sync time for the route key, or the
// zero time when the route has never synced.
func (s *Store) LastSync(key string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Routes[key].LastSync
}

// SetLastSync records a successful sync and writes the file atomically.
func (s *Store) SetLastSync(key string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Routes[key] = routeState{LastSync: t}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("writing state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replacing state: %w", err)
	}
	return nil
}
