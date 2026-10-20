// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Each create method starts exactly one kind, and create_vm needs a runner that offers VMs; every
// refusal comes before the durable intent, so nothing is recorded or created for it.
func TestDispatcherCreateMethodsStartOnlyTheirOwnKind(t *testing.T) {
	id := uuid.NewString()
	cases := []struct {
		name, method string
		kind         types.MachineKind
		offersVM     bool
		ok           bool
		refusal      string
	}{
		{"container by create_sandbox", runnerwire.MethodCreateSandbox, "", false, true, ""},
		{"explicit container by create_sandbox", runnerwire.MethodCreateSandbox, types.MachineContainer, false, true, ""},
		{"VM by create_sandbox", runnerwire.MethodCreateSandbox, types.MachineLinuxVM, true, false, "needs create_vm"},
		{"container by create_vm", runnerwire.MethodCreateVM, "", true, false, "Linux VM sandboxes only"},
		{"VM on a runner that does not offer VMs", runnerwire.MethodCreateVM, types.MachineLinuxVM, false, false, "does not offer Linux VM sandboxes"},
		{"unknown kind", runnerwire.MethodCreateVM, "windows_vm", true, false, "machine_kind"},
		{"VM by create_vm", runnerwire.MethodCreateVM, types.MachineLinuxVM, true, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := runnertest.NewFake()
			intents := 0
			d, err := newTestDispatcher(Config{RunnerID: id, Substrate: fake, LinuxVM: tc.offersVM,
				BeforeCreate: func(context.Context, runner.SandboxSpec) error { intents++; return nil },
				Created:      func(context.Context, runner.SandboxSpec, string) error { return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			a, b := runnerwire.Loopback()
			defer b.Close()
			p := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
			defer p.Close()
			data, _ := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: runner.SandboxSpec{RunID: uuid.New(), RunnerID: id, MachineKind: tc.kind}})
			_, err = d.Call(context.Background(), p, 2, tc.method, data)
			if tc.ok {
				if err != nil || len(fake.Created) != 1 || fake.Created[0].MachineKind != tc.kind {
					t.Fatalf("create = %v, created %+v", err, fake.Created)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("create = %v, want a refusal naming %q", err, tc.refusal)
			}
			if len(fake.Created) != 0 || intents != 0 {
				t.Fatalf("a refused create reached the substrate (%d) or recorded an intent (%d)", len(fake.Created), intents)
			}
		})
	}
}

// A VM create that arrives over a link that has dropped is refused, and nothing is recorded or
// created: a Linux VM sandbox never starts while the org is unreachable (OFFLINE-VM rule a).
func TestDispatcherRefusesACreateVMOnADroppedLink(t *testing.T) {
	id := uuid.NewString()
	fake := runnertest.NewFake()
	intents := 0
	d, err := newTestDispatcher(Config{RunnerID: id, Substrate: fake, LinuxVM: true,
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { intents++; return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	a, b := runnerwire.Loopback()
	defer b.Close()
	p := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
	p.Close()
	data, _ := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: runner.SandboxSpec{RunID: uuid.New(), RunnerID: id, MachineKind: types.MachineLinuxVM}})
	if _, err := d.Call(context.Background(), p, 2, runnerwire.MethodCreateVM, data); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("create_vm on a dropped link = %v, want %v", err, runner.ErrRunnerOffline)
	}
	if len(fake.Created) != 0 || intents != 0 {
		t.Fatalf("a refused create reached the substrate (%d) or recorded an intent (%d)", len(fake.Created), intents)
	}
}
