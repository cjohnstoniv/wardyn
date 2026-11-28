// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// spooledOnceRecorder takes the first person.create as a replica's local spool does when the store write
// fails: the production sentinel (audit.ErrSpooled), and no row in the shared table.
type spooledOnceRecorder struct {
	inner   *recRecorder
	spooled bool
}

func (s *spooledOnceRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if ev.Action == "person.create" && !s.spooled {
		s.spooled = true
		return fmt.Errorf("%w: audit chain lock timed out", audit.ErrSpooled)
	}
	return s.inner.Record(ctx, ev)
}

// TestPortalIntroduction_SpooledReceiptStaysOwed: a receipt that only reached the local spool leaves the
// introduction owed; the next exchange, recorded by another replica, stores the one event with its fixed id
// and time, and only then is it marked delivered.
func TestPortalIntroduction_SpooledReceiptStaysOwed(t *testing.T) {
	h := newExchHarness(t)
	sp := &spooledOnceRecorder{inner: h.rec}
	h.srv.cfg.Audit = sp
	a, credA := h.seedPortal(nil)
	portalToken(t, h.exchange(t, a.ID, credA, "subject-token", nil))
	ident := h.ps.idents[h.ver.subj.Principal]
	if !sp.spooled || ident == nil || ident.audited || len(h.introductions()) != 0 {
		t.Fatalf("after a spooled receipt: spooled %v, identity %+v, rows %d; want it owed and no stored row", sp.spooled, ident, len(h.introductions()))
	}
	// Another replica, whose recorder reaches the shared table.
	h.srv.cfg.Audit = h.rec
	portalToken(t, h.exchange(t, a.ID, credA, "subject-token", nil))
	rows := h.introductions()
	if len(rows) != 1 || rows[0].ID != ident.eventID || !rows[0].Time.Equal(ident.eventAt) || !ident.audited {
		t.Fatalf("after the retry: rows %+v, audited %v; want the one event %s %v, settled", rows, ident.audited, ident.eventID, ident.eventAt)
	}
}
