// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// servesMoves republishes rig's caps with MoveState, as a runner that serves moves sends them.
func servesMoves(t *testing.T, rig *runnertest.Rig) {
	t.Helper()
	c, ok := rig.Sub.Caps()
	if !ok {
		t.Fatal("no caps")
	}
	c.MoveState = true
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	rig.Sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
}

// TestImportSandbox_GatedAsACreate: an import creates a sandbox, so it reads the Dispatch gate first and, with
// runners turned off, sends nothing; and a spec carrying a value delivered for the run as runner_resident is
// refused before the call, as CreateSandbox refuses it.
func TestImportSandbox_GatedAsACreate(t *testing.T) {
	set := runner.CarriedSet{Roots: []string{"/home/agent/work"}}
	closed := errors.New("dispatch closed")
	reads := 0
	gated := remote.New(rid, &remote.Link{}, remote.Options{Dispatch: func(context.Context) error { reads++; return closed }})
	spec := runner.SandboxSpec{RunID: uuid.New(), RunnerID: rid, Image: "img", ConfinementClass: types.CC1}
	if _, _, err := gated.ImportSandbox(ctxT(t), spec, set, 1<<20, strings.NewReader("")); !errors.Is(err, closed) || !errors.Is(err, runner.ErrMoveCreate) || reads != 1 {
		t.Fatalf("import with the gate closed = %v after %d gate reads; want the gate's refusal at the create stage after one", err, reads)
	}

	rig := runnertest.NewRig(t, rid)
	servesMoves(t, rig)
	run, grant := uuid.New(), uuid.New()
	const secret = "s3cr3t-resident-value"
	if err := rig.Sub.DeliverResident(ctxT(t), runnerwire.DeliverResidentArgs{RunID: run, GrantID: grant, Kind: "env_secret", Value: secret, NotAfter: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	leak := runner.SandboxSpec{RunID: run, RunnerID: rid, Image: "img", ConfinementClass: types.CC1, SecretEnv: map[string]string{"TOKEN": secret}}
	if _, _, err := rig.Sub.ImportSandbox(ctxT(t), leak, set, 1<<20, strings.NewReader("")); !errors.Is(err, remote.ErrResidentLeak) || !errors.Is(err, runner.ErrMoveCreate) {
		t.Fatalf("an import spec carrying the resident value = %v; want ErrResidentLeak at the create stage", err)
	}
}
