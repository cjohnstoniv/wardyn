// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// RunLayout is a human's saved widget arrangement for the run-detail cockpit
// (GET/PUT /api/v1/me/run-layout), one row per (principal, preset) — see
// migration 0037_ui_layouts.sql and internal/api/ui_layout.go's doc comment
// for why this is a server row and not ui/src/app/lib/storage.ts's
// localStorage. Principal deliberately does not appear here: it is the row's
// SCOPE (how internal/store looks a layout up), never a value the API echoes
// back or a caller supplies — internal/api/ui_layout.go scopes every read and
// write by principalFromRequest(r) alone.
type RunLayout struct {
	Preset    string            `json:"preset"`
	Layout    []RunLayoutWidget `json:"layout"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// RunLayoutWidget places one evidence widget on the run cockpit's grid.
// Widget is validated server-side against a closed id set
// (internal/api/ui_layout.go's runLayoutWidgetIDs) before a layout is ever
// stored — an id no build renders would otherwise be a widget that silently
// vanishes on restore, with nothing to explain why.
type RunLayoutWidget struct {
	Widget string `json:"widget"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	W      int    `json:"w"`
	H      int    `json:"h"`
}
