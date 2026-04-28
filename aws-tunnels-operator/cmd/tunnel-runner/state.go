package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// State is the operational state the tunnel-runner writes to its state file.
type State string

const (
	StateStarting     State = "starting"
	StateRunning      State = "running"
	StateReconnecting State = "reconnecting"
	StateAuthRequired State = "auth_required"
	StateError        State = "error"
)

// StateWriter persists the current state to a file so liveness probes and
// external tooling can observe the tunnel lifecycle without querying the process.
type StateWriter struct {
	path string
	log  *slog.Logger
}

// newStateWriter creates a StateWriter, ensuring the target directory exists.
func newStateWriter(dir string, log *slog.Logger) (*StateWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir %q: %w", dir, err)
	}
	return &StateWriter{path: filepath.Join(dir, "state"), log: log}, nil
}

// Set atomically writes state to the file and logs the transition.
func (s *StateWriter) Set(state State, detail string) {
	if err := os.WriteFile(s.path, []byte(string(state)+"\n"), 0o644); err != nil {
		s.log.Warn("could not write state file", "path", s.path, "err", err)
	}
	if detail != "" {
		s.log.Info("state", "state", state, "detail", detail)
	} else {
		s.log.Info("state", "state", state)
	}
}
