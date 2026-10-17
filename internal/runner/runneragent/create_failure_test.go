// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

type interruptedCreate struct {
	*runnertest.Fake
	cancel context.CancelFunc
	err    error
}

func (s interruptedCreate) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	_, _ = s.Fake.CreateSandbox(ctx, spec)
	s.cancel()
	return runner.Sandbox{}, s.err
}

func TestCreateFailureContainsAfterCancellationAndPreservesErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provisionErr, containErr := errors.New("provision response lost"), errors.New("proxy containment unavailable")
	id, run := uuid.NewString(), uuid.New()
	intent, contained, forgotten := false, 0, 0
	d, err := newTestDispatcher(Config{RunnerID: id, Substrate: interruptedCreate{runnertest.NewFake(), cancel, provisionErr},
		BeforeCreate: func(context.Context, runner.SandboxSpec) error {
			if intent {
				return errors.New("existing creation intent")
			}
			intent = true
			return nil
		},
		Created: func(context.Context, runner.SandboxSpec, string) error {
			t.Fatal("persisted ambiguous create as successful")
			return nil
		},
		CreateFailed: func(safety context.Context, spec runner.SandboxSpec) error {
			if ctx.Err() == nil || safety.Err() != nil || spec.RunID != run {
				t.Fatal("failed create containment inherited cancellation or lost identity")
			}
			if _, ok := safety.Deadline(); !ok {
				t.Fatal("unbounded failed create containment")
			}
			contained++
			return containErr
		},
		Output: func(uuid.UUID) (io.Writer, error) { return io.Discard, nil },
		BindOutput: func(uuid.UUID, string) error {
			t.Fatal("bound output after ambiguous create")
			return nil
		},
		ForgetOutput: func(uuid.UUID) error { forgotten++; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	a, b := runnerwire.Loopback()
	defer b.Close()
	peer := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
	defer peer.Close()
	data, _ := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: runner.SandboxSpec{RunID: run, RunnerID: id}, ExecOutput: true})
	_, err = d.Call(ctx, peer, 2, runnerwire.MethodCreateSandbox, data)
	if !errors.Is(err, provisionErr) || !errors.Is(err, containErr) || contained != 1 || forgotten != 0 {
		t.Fatalf("error=%v contained=%d forgotten=%d", err, contained, forgotten)
	}
	if _, err := d.Call(context.Background(), peer, 4, runnerwire.MethodCreateSandbox, data); err == nil || contained != 1 || forgotten != 0 {
		t.Fatalf("duplicate contained existing intent: error=%v contained=%d forgotten=%d", err, contained, forgotten)
	}
}

// A create that fails after its resident values were delivered erases them
// (the value is deleted, the erasure reported) and takes no further delivery
// for the run.
func TestDispatcher_CreateFailureErasesResident(t *testing.T) {
	f := newResidentFixture(t, nil)
	grant := uuid.New()
	if err := f.deliver(grant, "s3cret", true); err != nil {
		t.Fatal(err)
	}
	provisionErr := errors.New("provision failed")
	d, err := newTestDispatcher(Config{RunnerID: f.id, Substrate: interruptedCreate{runnertest.NewFake(), func() {}, provisionErr}, Resident: f.res,
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: runner.SandboxSpec{RunID: f.run, RunnerID: f.id,
		SecretEnv: map[string]string{"TOKEN": placement.ResidentPlaceholder(grant)}}})
	if _, err := d.Call(context.Background(), f.peer, 3, runnerwire.MethodCreateSandbox, data); !errors.Is(err, provisionErr) {
		t.Fatalf("create = %v, want the provision error", err)
	}
	if f.ks.size() != 0 {
		t.Fatalf("keystore after a failed create holds %d items, want none", f.ks.size())
	}
	if ev := f.takeEvents(); len(ev) != 1 || ev[0].GrantID != grant || ev[0].Outcome != runnerwire.ResidentErased {
		t.Fatalf("events after a failed create = %+v, want the grant reported erased", ev)
	}
	if err := f.deliverTo(f.run, uuid.New(), "v", true, f.clock.now().Add(time.Hour)); err == nil {
		t.Fatal("a delivery for a run whose create failed was taken")
	}
}
