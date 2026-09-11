// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/component"
)

// Deps are the platform primitives a recording.Store constructor may use. New
// seams keep their own typed Deps so heterogeneous construction stays type-safe.
type Deps struct {
	// Dir is the base directory for filesystem-backed stores. Empty => recording
	// disabled (the fs constructor returns a nil Store). fs-specific; pg ignores it.
	Dir string
	// Pool is the shared pgxpool the pg-backed store persists through — the SAME
	// pool the rest of the control plane uses, so a cast is visible to every
	// replica instead of living on one pod's local disk.
	Pool *pgxpool.Pool
}

// Constructor builds a Store from Deps. It may return (nil, nil) to mean
// "recording disabled" (the fs store with an empty Dir), which callers treat as
// no-recording.
type Constructor func(Deps) (Store, error)

var reg = component.NewRegistry[Constructor]("pg")

// Register adds a recording-store implementation; call it from an init().
func Register(name string, c Constructor) { reg.Register(name, c) }

// Names returns the registered store names (for /healthz and error messages).
func Names() []string { return reg.Names() }

// New constructs the recording store selected by name (empty => default).
func New(name string, d Deps) (Store, error) {
	ctor, _, err := reg.Resolve(name)
	if err != nil {
		return nil, err
	}
	return ctor(d)
}

func init() {
	// "off" — recording disabled, spelled out. It exists because 0.7's
	// FlagEnv keeps a compiled default when an env value is EMPTY (F011/F067:
	// WARDYN_LISTEN="" must not become 0.0.0.0:80), which made the Helm
	// chart's old off recipe — WARDYN_RECORDING_STORE=fs plus an empty
	// WARDYN_RECORDING_DIR — unreachable: the empty dir kept ./data/recordings,
	// NewFSStore's MkdirAll hit the read-only root FS, and a stock install
	// crash-looped (ci.yml helm-install-test, red since the 0.7.0 release).
	Register("off", func(Deps) (Store, error) { return nil, nil })
	Register("fs", func(d Deps) (Store, error) {
		if d.Dir == "" {
			return nil, nil // recording disabled (no replay/upload)
		}
		s, err := NewFSStore(d.Dir)
		if err != nil {
			return nil, err
		}
		return s, nil // avoid the typed-nil interface trap
	})
}
