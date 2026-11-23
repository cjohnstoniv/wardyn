// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistant

import (
	"fmt"
	"strings"
)

// Edit is one field the person changed in the draft themselves, with its new value.
type Edit struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

// NextTurn is what the model reads for one person turn: a line for each field the person changed since the
// last turn, then their own text. This is prompt text, not a console string.
func NextTurn(message string, edits []Edit) string {
	if len(edits) == 0 {
		return message
	}
	var b strings.Builder
	for _, e := range edits {
		fmt.Fprintf(&b, "The person changed %s to %s.\n", e.Field, e.Value)
	}
	b.WriteString(message)
	return b.String()
}
