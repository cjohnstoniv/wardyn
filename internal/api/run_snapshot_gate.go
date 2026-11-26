// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretspresent"
)

// A snapshot of a Linux VM run passes the secrets check twice: at admission, on the facts as of now, and again
// after the copy is cut, on durable state only, with the cut's own window for brokered mints. The second check is
// the guarantee: a secret that arrived between the two, or during the cut, refuses the snapshot and its copy is
// deleted.

// mintWindow is the window a brokered mint is judged in: a mint recorded at or before to (zero: with no bound)
// whose credential is still valid after from (zero: the database's clock when the facts are read) counts.
type mintWindow struct {
	from, to time.Time
}

// admissionWindow is the window of a snapshot's admission: every mint so far, judged as of now.
func admissionWindow() mintWindow { return mintWindow{} }

// cutWindow is the window of the check after a cut: a mint at or before cutEnd whose credential was still valid
// after cutStart may be in the copy.
func cutWindow(cutStart, cutEnd time.Time) mintWindow { return mintWindow{from: cutStart, to: cutEnd} }

// snapshotSecretsGate runs the secrets rule for runID in window w. present true refuses the snapshot, and the
// reasons name causes, never values.
func (s *Server) snapshotSecretsGate(ctx context.Context, runID uuid.UUID, w mintWindow) (present bool, reasons []secretspresent.Reason) {
	return s.secretsPresent(ctx, runID, w)
}
