// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/google/uuid"
)

// RootDirName is the VM directory under the user's Application Support/Wardyn.
// The .noindex suffix keeps Spotlight out of it.
const RootDirName = "macos-vm.noindex"

// DefaultRoot is ~/Library/Application Support/Wardyn/macos-vm.noindex.
func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "Wardyn", RootDirName), nil
}

// Layout is the VM directory tree:
//
//	<root>/bases/<slot>/   disk.img aux.img machine-id manifest.json (slot 0 unless the per-slot fallback is on)
//	<root>/cache/          <build>.ipsw(.part)
//	<root>/runs/<run-key>/ disk.img aux.img machine-id run.json helper.log
//	<root>/prepare.log
//	<root>/setup-code.json (only while macOS waits for its setup by hand)
//
// A run's sockets live outside the home, in its socket directory (SockDir).
type Layout struct{ Root string }

func (l Layout) BaseDir(slot int) string { return filepath.Join(l.Root, "bases", strconv.Itoa(slot)) }
func (l Layout) CacheDir() string        { return filepath.Join(l.Root, "cache") }
func (l Layout) RunsDir() string         { return filepath.Join(l.Root, "runs") }
func (l Layout) RunDir(runID string) string {
	return filepath.Join(l.RunsDir(), RunKey(runID))
}

// RunKeyLen is the length of a run key.
const RunKeyLen = 12

// RunKey names a run's directories: the run id's first 12 hex digits, so the
// socket paths stay under MaxSocketPath. A string that is not a UUID (a key
// already) is returned as it is.
func RunKey(runID string) string {
	if u, err := uuid.Parse(runID); err == nil {
		return hex.EncodeToString(u[:RunKeyLen/2])
	}
	return runID
}

// MaxSocketPath is the longest unix socket path macOS binds or connects
// (sockaddr_un's sun_path is 104 bytes with its terminating NUL).
const MaxSocketPath = 103

// SockDir is the run's socket directory, <sock-root>/u<uid>/<run-key>, under
// sockRoot ("" is admission.DefaultSockDir). It holds ctl.sock, agent.sock
// and desktop.sock.
func SockDir(sockRoot, runID string) string {
	return filepath.Join(SockConfig(sockRoot).UserDir(), RunKey(runID))
}

// SockConfig is this login's socket directory configuration under sockRoot.
func SockConfig(sockRoot string) admission.SockConfig {
	return admission.SockConfig{Dir: sockRoot, UID: os.Getuid()}
}

// checkSocketPaths refuses a socket directory whose longest socket path
// would not fit a macOS sockaddr_un.
func checkSocketPaths(dir string) error {
	if n := len(filepath.Join(dir, DesktopSock)); n > MaxSocketPath {
		return fmt.Errorf("macvm: the socket directory %s is too long for its sockets' paths (%d bytes, at most %d)", dir, n, MaxSocketPath)
	}
	return nil
}
func (l Layout) PrepareLog() string { return filepath.Join(l.Root, "prepare.log") }

// PrepRelayLogFile is the log of the in-helper relays a prepare or tools
// update boots its VM on (their refusals and drops), beside prepare.log in
// the root. It is never the helper's stdout or stderr: runnerd reads
// prepare.log's last line as progress.
const PrepRelayLogFile = "prep-relay.log"

func (l Layout) PrepRelayLog() string { return filepath.Join(l.Root, PrepRelayLogFile) }

// File names inside a base or run directory.
const (
	DiskFile      = "disk.img"
	AuxFile       = "aux.img"
	MachineIDFile = "machine-id"
	ManifestFile  = "manifest.json"
	RunConfigFile = "run.json"
	ControlSock   = "ctl.sock"
	AgentSock     = "agent.sock"
	DesktopSock   = "desktop.sock"
	HelperLog     = "helper.log"
	// installingMarker sits in a base dir from the install's start until the
	// base is ready; finding it means an install was cut short.
	installingMarker = "installing"
)

// mkdirPrivate creates dir (and parents) with mode 0700, and tightens an
// existing dir to 0700: VM disks and sockets are the user's alone.
func mkdirPrivate(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// Base image states in the manifest.
const (
	BaseReady        = "ready"
	BaseFailed       = "failed"
	BaseUpdateNeeded = "update_needed"
)

// Desktop modes, from prepare's control check.
const (
	DesktopControl = "control"
	DesktopObserve = "observe"
	DesktopNone    = "none"
)

// BaseFormat is the base layout version this helper writes and accepts.
const BaseFormat = 1

// Manifest describes a ready base image.
type Manifest struct {
	Format        int       `json:"format"`
	State         string    `json:"state"`
	GuestOS       string    `json:"guest_os"`
	BuildVersion  string    `json:"build_version"`
	IPSWSHA256    string    `json:"ipsw_sha256"`
	HelperVersion string    `json:"helper_version"`
	AgentVersion  string    `json:"agent_version,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	MinCPUs       int       `json:"min_cpus"`
	MinMemoryMiB  uint64    `json:"min_memory_mib"`
	DiskBytes     int64     `json:"disk_bytes"`
	HardwareModel []byte    `json:"hardware_model"`
	Desktop       string    `json:"desktop"`
	Harnesses     []string  `json:"harnesses"`
}

// ReadManifest reads a base's manifest.
func ReadManifest(baseDir string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(filepath.Join(baseDir, ManifestFile))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("macvm: base manifest: %w", err)
	}
	return m, nil
}

// ReadyBase reads a manifest and fails unless the base is ready and in a
// format this helper accepts.
func ReadyBase(baseDir string) (Manifest, error) {
	m, err := ReadManifest(baseDir)
	if err != nil {
		return m, err
	}
	switch {
	case m.Format != BaseFormat:
		return m, fmt.Errorf("macvm: this Mac's macOS base needs an update (format %d, helper accepts %d)", m.Format, BaseFormat)
	case m.State != BaseReady:
		return m, fmt.Errorf("macvm: this Mac's macOS base is %s, not ready", m.State)
	}
	return m, nil
}

// writeJSONAtomic writes v to path through a temp file and a rename.
func writeJSONAtomic(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// RunConfig is what runnerd writes to <run-dir>/run.json before it starts
// `wardyn-macvm run --dir <run-dir>`.
type RunConfig struct {
	RunID     string           `json:"run_id"`
	BaseDir   string           `json:"base_dir"`
	CPUs      int              `json:"cpus"`
	MemoryMiB uint64           `json:"memory_mib"`
	Identity  IdentityStrategy `json:"identity"`
	// RunnerID and LedgerEntry name this VM's admission entry, which the
	// helper claims before it builds the VM. LedgerDir overrides the default
	// ledger directory (tests).
	RunnerID    string `json:"runner_id,omitempty"`
	LedgerEntry string `json:"ledger_entry,omitempty"`
	LedgerDir   string `json:"ledger_dir,omitempty"`
	// SockRoot overrides the socket root, admission.DefaultSockDir (tests).
	SockRoot string `json:"sock_root,omitempty"`
}

// toolsModeField is the helper's stage-2 boot-configuration field. Only
// prepare and a tools update set it, in memory; a run.json that carries it
// is refused, so a run boot never listens on the tools port.
const toolsModeField = "tools_mode"

// WriteRunConfig creates the run dir (0700) and writes its run.json.
func WriteRunConfig(runDir string, c RunConfig) error {
	if err := mkdirPrivate(runDir); err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(runDir, RunConfigFile), c, 0o600)
}

// ReadRunConfig reads <run-dir>/run.json, on every open: the directory's
// name must be the key of the run id it names, and it must not carry tools
// mode.
func ReadRunConfig(runDir string) (RunConfig, error) {
	var c RunConfig
	b, err := os.ReadFile(filepath.Join(runDir, RunConfigFile))
	if err != nil {
		return c, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return c, fmt.Errorf("macvm: run config: %w", err)
	}
	if _, ok := fields[toolsModeField]; ok {
		return c, ErrToolsModeRun
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("macvm: run config: %w", err)
	}
	if c.RunID == "" || c.BaseDir == "" {
		return c, errors.New("macvm: run config needs run_id and base_dir")
	}
	if name, key := filepath.Base(filepath.Clean(runDir)), RunKey(c.RunID); name != key {
		return c, fmt.Errorf("macvm: the run directory %s holds run %s, whose directory is %s", runDir, c.RunID, key)
	}
	return c, nil
}
