// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import "github.com/cjohnstoniv/wardyn/internal/component"

// Deps are the platform primitives a recording.Store constructor may use. New
// seams keep their own typed Deps so heterogeneous construction stays type-safe.
type Deps struct {
	// Dir is the base directory for filesystem-backed stores. Empty => recording
	// disabled (the fs constructor returns a nil Store).
	Dir string
}

// Constructor builds a Store from Deps. It may return (nil, nil) to mean
// "recording disabled" (the fs store with an empty Dir), which callers treat as
// no-recording.
type Constructor func(Deps) (Store, error)

var reg = component.NewRegistry[Constructor]("fs")

// Register adds a recording-store implementation; call it from an init().
func Register(name string, c Constructor) { reg.Register(name, c) }

// Names lists the registered store names (sorted). Default returns the default.
func Names() []string { return reg.Names() }
func Default() string { return reg.Default() }

// New constructs the recording store selected by name (empty => default).
func New(name string, d Deps) (Store, error) {
	ctor, _, err := reg.Resolve(name)
	if err != nil {
		return nil, err
	}
	return ctor(d)
}

func init() {
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
