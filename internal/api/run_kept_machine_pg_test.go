// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
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

// A suspension's kill (the system's, as the SCIM deprovision's teardown makes it) keeps the person's machine
// as a hard stop: nothing is deleted, and nothing but its owner's own session, which the suspension ended,
// can resume it.
func TestSuspensionKillKeepsTheMachineUnbootable(t *testing.T) {
	k := newKeptFixture(t)
	m := k.machine(t, k.owner, k.rn.ID, k.run.ID, "dev", types.KeptMachineInUse)
	if applied, _, err := k.srv.killRunCascade(k.ctx, k.run, types.ActorSystem, scimActor, map[string]any{"reason": "scim_suspend"}); err != nil || !applied {
		t.Fatalf("the suspension's kill: applied %v, %v", applied, err)
	}
	if run, err := k.st.GetRun(k.ctx, k.run.ID); err != nil || run.State != types.RunKilled {
		t.Fatalf("run after the suspension: %+v, %v", run.State, err)
	}
	k.sweep(t)
	k.requireKept(t, m)
	in := keptMachineAsk{req: createRunRequest{Agent: "claude-code", MachineKind: string(types.MachineMacOSVM), Interactive: true,
		Machine: &client.RunMachine{Resume: "dev"}}}
	for name, ctx := range map[string]context.Context{
		"the system, or an admin token": context.Background(),
		"another person":                personCtx(bobSub),
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(ctx)
		if plan, rf := k.srv.keptMachineDecision(req, in); rf == nil || plan != nil {
			t.Errorf("%s resumed the suspended person's machine: %+v", name, plan)
		}
	}
}
