package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"

	sqlite3 "github.com/mattn/go-sqlite3"

	"dustkeeper/internal/errs"
	"dustkeeper/internal/scope"
)

// version is the phase-one release identifier.
const version = "1.0.0-phase1"

// Linker-injected build facts. Section 3.1 requires `version --json` to report
// the commit and the build tags, because a bug report from a packaged binary is
// otherwise impossible to reproduce. build/build.sh sets all three with -X; a
// plain `go build` leaves them at their defaults, which is stated as such rather
// than guessed.
var (
	commit     = "unknown"
	buildTags  = "none"
	buildStamp = "unknown"
)

// driverVersion reads the go-sqlite3 version out of the module metadata baked
// into the binary, so the reported value is the one actually linked.
func driverVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, dep := range info.Deps {
		if dep.Path == "github.com/mattn/go-sqlite3" {
			return dep.Version
		}
	}
	return "unknown"
}

// goVersion reports the toolchain the binary was built with. It is printed by
// `version` so a bug report from a packaged binary is reproducible.
func goVersion() string { return runtime.Version() }

// sqliteVersion reports the linked SQLite library. The binding vendors it, so
// this is a fact about the binary, not about the host.
func sqliteVersion() string {
	v, _, _ := sqlite3.Version()
	return v
}

func platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// lockInstance takes the per-state-directory execution lock and returns a
// release function.
//
// A second task never queues: it exits 3 so a cron job and a human command do
// not silently interleave.
func lockInstance(stateDir string) (func(), error) {
	lock, err := scope.InstanceLock(stateDir)
	if err != nil {
		return nil, err
	}
	return func() { _ = lock.Release() }, nil
}

// instanceFile is the small identity record next to the database.
type instanceFile struct {
	FormatVersion int    `json:"format_version"`
	InstanceID    string `json:"instance_id"`
	CreatedAtNS   int64  `json:"created_at_ns"`
	StateDir      string `json:"state_dir"`
	DBFile        string `json:"db_file"`
}

// writeInstanceFile persists the identity marker. It is advisory: authority
// lives in the database, and a missing marker never grants anything.
func writeInstanceFile(path string, f instanceFile) error {
	body, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	body = append(body, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	return nil
}

// ensureDir creates a private directory and verifies the mode.
func ensureDir(path string, perm os.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return errs.Wrap(errs.CodeAccessDenied, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return errs.Wrap(errs.CodeAccessDenied, err)
	}
	if !info.IsDir() {
		return errs.Newf(errs.CodeInvalidArgument, "%s exists and is not a directory", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, perm); err != nil {
			return errs.Wrap(errs.CodeAccessDenied, err)
		}
	}
	return nil
}

// displayPath renders a path for humans without resolving it. It never
// round-trips into a syscall.
func displayPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
