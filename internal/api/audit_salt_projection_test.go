// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// TestProjectedRawLine_ClearsSaltOnlyWhenProjected: an export line or receipt event whose data was changed by the
// projection gives up its salt (its hash can then not be recomputed from what is shown); one whose data is as
// stored keeps it, so an export holder verifies it offline. A format 1 line carries neither field.
func TestProjectedRawLine_ClearsSaltOnlyWhenProjected(t *testing.T) {
	salt := strings.Repeat("ab", 32)
	data := `{"kind": "git", "ref": "main", "path": "/home/dev/secret"}`
	hash := strings.Repeat("cd", 32)
	row := store.AuditPartitionRow{Seq: 5, ID: uuid.New(), ActorType: "system", Actor: "a", Action: "source.write", Outcome: "success",
		Data: &data, RowHash: &hash, HashFormat: 2, HashSalt: &salt}
	var redacted int64
	projects := partitionProjector(false, &redacted) // source.write's path is withheld below a super admin
	keeps := partitionProjector(true, &redacted)
	if l := projectedRawLine(row, projects); l.HashSalt != nil || l.HashFormat != 2 || l.Data == nil || *l.Data == data {
		t.Errorf("a projected line = format %d, salt kept %v, data %v; want format 2, no salt, the projected data", l.HashFormat, l.HashSalt != nil, l.Data)
	}
	if l := projectedRawLine(row, keeps); l.HashSalt == nil || *l.HashSalt != salt || l.HashFormat != 2 || *l.Data != data {
		t.Errorf("an unprojected line = format %d, salt %v; want format 2 with its salt and its data as stored", l.HashFormat, l.HashSalt)
	}
	if l := rawPartitionLine(row); l.HashSalt == nil || l.HashFormat != 2 {
		t.Errorf("a raw line must carry format 2 and its salt, got %d, %v", l.HashFormat, l.HashSalt)
	}

	redacted = 0
	if ev := receiptEvent(row, projects, &redacted); ev.Row.HashSalt != nil || ev.Row.HashFormat != 2 || redacted != 1 {
		t.Errorf("a redacted receipt event = format %d, salt kept %v, redacted %d; want format 2, no salt, 1", ev.Row.HashFormat, ev.Row.HashSalt != nil, redacted)
	}
	redacted = 0
	if ev := receiptEvent(row, keeps, &redacted); ev.Row.HashSalt == nil || *ev.Row.HashSalt != salt || redacted != 0 {
		t.Errorf("an unredacted receipt event lost its salt or counted a redaction (%d)", redacted)
	}

	v1 := row
	v1.HashFormat, v1.HashSalt = 1, nil
	if l := rawPartitionLine(v1); l.HashFormat != 0 || l.HashSalt != nil {
		t.Errorf("a format 1 line = format %d, salt %v; want neither, the shape it always had", l.HashFormat, l.HashSalt)
	}
}
