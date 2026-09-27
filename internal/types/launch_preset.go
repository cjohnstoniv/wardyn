// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"time"
)

// LaunchPreset is one row of launch_presets (migration 0086): an
// admin-managed, named bundle of existing POST /runs fields. Request is the
// stored create-run body, kept raw here because its Go type lives in the
// public SDK (pkg/client.CreateRunRequest), which imports this package.
type LaunchPreset struct {
	Name        string          `json:"name"`
	Version     int             `json:"version"`
	Description string          `json:"description,omitempty"`
	UserTypes   []string        `json:"user_types"`
	Request     json.RawMessage `json:"request"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	CreatedBy   string          `json:"created_by,omitempty"`
	UpdatedBy   string          `json:"updated_by,omitempty"`
}
