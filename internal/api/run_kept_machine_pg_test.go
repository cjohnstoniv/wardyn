// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A resume that meets an expired machine starts its delete, and says the machine was deleted only once
// the Mac confirmed it; the dry doors delete nothing.
func TestResumeOfAnExpiredMachineSaysDeletedOnlyOnceConfirmed(t *testing.T) {
	k := newKeptFixture(t)
	expired := func(name string) types.KeptMachine {
		run := k.newRun(t, k.owner, k.rn.ID)
		m := k.machine(t, k.owner, k.rn.ID, run.ID, name, types.KeptMachineKept)
		k.exec(t, `UPDATE kept_machines SET expires_at = now() - interval '1 hour' WHERE id = $1`, m.ID)
		m.ExpiresAt = time.Now().Add(-time.Hour)
		return m
	}
	m := expired("web")
	wantKeptRefusal(t, k.srv.keptExpiredRefusal(k.ctx, m, false), http.StatusGone, reasonKeptMachineExpired, keptMachineExpiredMsg)
	if asked := k.mac.asked(); len(asked) != 0 {
		t.Fatalf("a dry door asked the Mac to delete %v", asked)
	}
	wantKeptRefusal(t, k.srv.keptExpiredRefusal(k.ctx, m, true), http.StatusGone, reasonKeptMachineExpired,
		"Your machine web wasn't used for 30 days, so it was deleted. Start a new one.")
	if _, ok := k.row(t, m.ID); ok {
		t.Fatal("a confirmed delete left the row")
	}

	k.mac.mu.Lock()
	k.mac.fail = keptMacLosesReply
	k.mac.mu.Unlock()
	lost := expired("ios")
	wantKeptRefusal(t, k.srv.keptExpiredRefusal(k.ctx, lost, true), http.StatusGone, reasonKeptMachineExpired,
		"This machine expired and will be deleted.")
}
