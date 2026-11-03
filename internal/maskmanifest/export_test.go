// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskmanifest

import "github.com/google/uuid"

// SetBeforePublish has a load run f as its last step before it installs its entry.
func (m *Manifests) SetBeforePublish(f func(runID uuid.UUID)) { m.beforePublish = f }
