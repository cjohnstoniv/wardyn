// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"io"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auditreceipt"
	"github.com/cjohnstoniv/wardyn/internal/auditsign"
)

// AuditReceiptConfig is Config's signed audit checkpoint wiring (cmd/wardynd's startAuditCheckpoints),
// embedded so it lives beside the feature rather than in server.go.
type AuditReceiptConfig struct {
	// AuditReceiptKeys lists the checkpoint signing keys' public halves, the current key first and every
	// previous one after it (retired ones flagged). Nil while signing is off: the interval is 0 or the key
	// could not be loaded, and the receipt routes answer that signing is off.
	AuditReceiptKeys func() []auditreceipt.PublicKey
	// AuditCheckpointStatus is the signer's status for the wardyn_audit_checkpoint_* metrics. Nil (the
	// interval is 0) omits the series.
	AuditCheckpointStatus *auditsign.Status
	// AuditCheckpointInterval is how often the signer writes a checkpoint (WARDYN_AUDIT_CHECKPOINT_INTERVAL, 0 is
	// off); a pending receipt's Retry-After is this interval (audit_receipt.go).
	AuditCheckpointInterval time.Duration
}

// writeAuditCheckpoint emits the checkpoint signer's series. They are this process's: with WARDYN_HA the
// replica holding the signer lock on a tick updates its own, so read the last timestamp with max() across
// replicas.
func (s *Server) writeAuditCheckpoint(w io.Writer) {
	if s.cfg.AuditCheckpointStatus == nil {
		return
	}
	snap := s.cfg.AuditCheckpointStatus.Snapshot()
	var last int64
	if !snap.LastSigned.IsZero() {
		last = snap.LastSigned.Unix()
	}
	fmt.Fprintf(w, "# HELP wardyn_audit_checkpoint_lag_rows Audit rows appended since the last signed checkpoint, as of this replica's last signer tick.\n"+
		"# TYPE wardyn_audit_checkpoint_lag_rows gauge\nwardyn_audit_checkpoint_lag_rows %d\n", snap.LagRows)
	fmt.Fprintf(w, "# HELP wardyn_audit_checkpoint_last_timestamp_seconds Unix time this replica last wrote a signed audit checkpoint, 0 if never.\n"+
		"# TYPE wardyn_audit_checkpoint_last_timestamp_seconds gauge\nwardyn_audit_checkpoint_last_timestamp_seconds %d\n", last)
	fmt.Fprint(w, "# HELP wardyn_audit_checkpoint_failures_total Audit checkpoint signing failures by reason; chain_break and foreign_row stop signing until wardynd restarts.\n"+
		"# TYPE wardyn_audit_checkpoint_failures_total counter\n")
	for _, r := range auditsign.Reasons {
		fmt.Fprintf(w, "wardyn_audit_checkpoint_failures_total{reason=%q} %d\n", r, snap.Failures[r])
	}
}
