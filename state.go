package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// ==========================================
// Installation snapshot
// ==========================================
//
// "cert uninstall" promises to restore the server to its pre-installation
// state. To keep that promise, every side effect on pre-existing state is
// recorded here while it happens:
//
//   - packages that did NOT exist before "cert init" installed them
//   - services that "cert init" enabled/started
//   - default site files that were moved aside
//   - directories that were created from scratch
//
// Files owned by the tool itself (site configs, certificates, cron job, …)
// need no snapshot: they carry the "Managed by cert-tool" marker or live
// under tool-owned paths.

// movedPath records a file that existed before installation and was moved.
type movedPath struct {
	From string `json:"from"` // original path
	To   string `json:"to"`   // where it was moved to
}

// installState is the snapshot of what installation changed.
type installState struct {
	InstalledAt       time.Time   `json:"installed_at"`
	PackagesInstalled []string    `json:"packages_installed,omitempty"` // absent before, installed by us
	NginxEnabledByUs  bool        `json:"nginx_enabled_by_us,omitempty"`
	NginxStartedByUs  bool        `json:"nginx_started_by_us,omitempty"`
	MovedPaths        []movedPath `json:"moved_paths,omitempty"`
	DirsCreated       []string    `json:"dirs_created,omitempty"` // did not exist before, created by us
}

const stateFileName = "state.json"

// statePath returns the snapshot location (next to the config file).
func statePath() string {
	return filepath.Join(filepath.Dir(getConfigFile()), stateFileName)
}

func loadState() *installState {
	state := &installState{}
	data, err := os.ReadFile(statePath())
	if err != nil {
		return state
	}
	_ = json.Unmarshal(data, state)
	return state
}

func (s *installState) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writePrivilegedFile(statePath(), data, 0644)
}

// recordDirCreated notes a directory that did not exist before we created it.
// Uninstall removes such directories wholesale; pre-existing ones only get
// tool-owned files removed from them. Callers must check existence BEFORE
// creating the directory (see ensureTrackedDir).
func (s *installState) recordDirCreated(dir string) {
	for _, d := range s.DirsCreated {
		if d == dir {
			return
		}
	}
	s.DirsCreated = append(s.DirsCreated, dir)
}

// recordMoved notes a pre-existing file we moved aside. Uninstall moves it
// back, so the server is left exactly as it was found.
func (s *installState) recordMoved(from, to string) {
	for _, m := range s.MovedPaths {
		if m.From == from {
			return
		}
	}
	s.MovedPaths = append(s.MovedPaths, movedPath{From: from, To: to})
}

// ensureTrackedDir creates a directory and records it in the install snapshot
// if it did not exist before.
func ensureTrackedDir(dir string, mode os.FileMode) error {
	_, statErr := os.Stat(dir)
	if err := ensureDir(dir, mode); err != nil {
		return err
	}
	if statErr != nil {
		state := loadState()
		state.recordDirCreated(dir)
		_ = state.save()
	}
	return nil
}

// recordMove notes that a pre-existing file was moved aside, so uninstall can
// restore it later.
func recordMove(from, to string) {
	state := loadState()
	state.recordMoved(from, to)
	_ = state.save()
}
