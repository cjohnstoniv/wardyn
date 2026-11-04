// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskmanifest

import "github.com/google/uuid"

// SetLoadPause has a load call f with "before-register" and again with "after-register" (the latter holding
// the load's registration step, so f must not start another load).
func (m *Manifests) SetLoadPause(f func(stage string, runID uuid.UUID)) { m.loadPause = f }

// BeginDrop is the manifests' drop guard, for a test that wraps it to stop a removal part way.
func (m *Manifests) BeginDrop(runID uuid.UUID) (end func()) { return m.beginDrop(runID) }
