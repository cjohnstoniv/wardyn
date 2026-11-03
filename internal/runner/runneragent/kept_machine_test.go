// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// keptFake is the fake substrate with a kept machine delete that answers outcome or err.
type keptFake struct {
	*runnertest.Fake
	asked   []uuid.UUID
	by      []string
	outcome string
	err     error
}

func (f *keptFake) DeleteKeptMachine(_ context.Context, id uuid.UUID, by string) (string, error) {
	f.asked, f.by = append(f.asked, id), append(f.by, by)
	return f.outcome, f.err
}

func keptDispatcher(t *testing.T, sub substrate.Substrate) (func(runnerwire.KeptMachineArgs) (any, error), func()) {
	t.Helper()
	a, b := runnerwire.Loopback()
	p := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
	d, err := newTestDispatcher(Config{RunnerID: uuid.NewString(), Substrate: sub,
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	call := func(args runnerwire.KeptMachineArgs) (any, error) {
		data, _ := runnerwire.Marshal(args)
		return d.Call(context.Background(), p, 2, runnerwire.MethodKeptMachine, data)
	}
	return call, func() { p.Close(); _ = b.Close() }
}

// kept_machine reaches the substrate's delete with the machine's id and who asked, and answers its
// outcome; the Mac's refusal of a booted machine comes back as an error.
func TestKeptMachineDeleteReachesTheSubstrate(t *testing.T) {
	sub := &keptFake{Fake: runnertest.NewFake(), outcome: runnerwire.KeptMachineDeleted}
	call, done := keptDispatcher(t, sub)
	defer done()
	id := uuid.New()
	args := runnerwire.KeptMachineArgs{Op: runnerwire.KeptMachineOpDelete, ID: id.String(), By: "owner"}
	if res, err := call(args); err != nil || res != (runnerwire.KeptMachineResult{Outcome: runnerwire.KeptMachineDeleted}) {
		t.Fatalf("delete: %v %v", res, err)
	}
	sub.outcome = runnerwire.KeptMachineAlreadyAbsent
	if res, err := call(args); err != nil || res != (runnerwire.KeptMachineResult{Outcome: runnerwire.KeptMachineAlreadyAbsent}) {
		t.Fatalf("a repeat delete: %v %v", res, err)
	}
	inUse := errors.New("docker: the kept machine is running")
	sub.outcome, sub.err = "", inUse
	if _, err := call(args); !errors.Is(err, inUse) {
		t.Fatalf("a booted machine: %v", err)
	}
	sub.outcome, sub.err = "gone", nil
	_, err := call(args)
	var we *runnerwire.Error
	if err == nil || errors.As(err, &we) && we.Code == runnerwire.CodeRefused {
		t.Fatalf("an unknown outcome after a delete ran must be an internal error, not a refusal: %v", err)
	}
	if len(sub.asked) != 4 || sub.asked[0] != id || sub.by[0] != "owner" {
		t.Fatalf("the substrate was asked %v by %v", sub.asked, sub.by)
	}
}

// kept_machine refuses, before the substrate is asked, an op it does not know and an id that is not a
// machine's full id; a substrate that keeps no machines refuses every delete.
func TestKeptMachineRefusesBadArgsAndAPlainSubstrate(t *testing.T) {
	sub := &keptFake{Fake: runnertest.NewFake(), outcome: runnerwire.KeptMachineDeleted}
	call, done := keptDispatcher(t, sub)
	defer done()
	id := uuid.New()
	for name, args := range map[string]runnerwire.KeptMachineArgs{
		"op":        {Op: "reset", ID: id.String()},
		"no op":     {ID: id.String()},
		"short id":  {Op: runnerwire.KeptMachineOpDelete, ID: id.String()[:12]},
		"nil id":    {Op: runnerwire.KeptMachineOpDelete, ID: uuid.Nil.String()},
		"braced id": {Op: runnerwire.KeptMachineOpDelete, ID: "{" + id.String() + "}"},
		"a path":    {Op: runnerwire.KeptMachineOpDelete, ID: "../runs/" + id.String()},
		"empty id":  {Op: runnerwire.KeptMachineOpDelete},
	} {
		if _, err := call(args); err == nil {
			t.Errorf("%s: %+v was accepted", name, args)
		}
	}
	if len(sub.asked) != 0 {
		t.Fatalf("the substrate was asked %v", sub.asked)
	}
	plain, done2 := keptDispatcher(t, runnertest.NewFake())
	defer done2()
	if _, err := plain(runnerwire.KeptMachineArgs{Op: runnerwire.KeptMachineOpDelete, ID: id.String()}); err == nil {
		t.Fatal("a substrate that keeps no machines answered a delete")
	}
}
