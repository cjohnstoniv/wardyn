// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// namedFake is a fake substrate under its own name.
type namedFake struct {
	*runnertest.Fake
	name string
}

func (n namedFake) Name() string { return n.name }

// Before the create the substrate a class routes to answers, by CreateSandbox's first match; once the
// sandbox exists the substrate that holds it answers, even when its class would now route elsewhere.
func TestExecOutputUncapturedFor_ClassBeforeCreateRefAfter(t *testing.T) {
	docker, k8s := runnertest.NewFake(), runnertest.NewFake()
	docker.Support = substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1}}
	k8s.Support = substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1, types.CC2}, SessionRecording: true, ExecOutputUncaptured: true}
	o := New(namedFake{Fake: docker, name: "docker"}, namedFake{Fake: k8s, name: "k8s"})
	clock := time.Now()
	o.now = func() time.Time { return clock }
	ctx := t.Context()
	if u, known := o.ExecOutputUncapturedFor(ctx, "", types.CC1); !known || u {
		t.Fatalf("CC1 before create = (%v, %v), want captured: Docker is its first match", u, known)
	}
	if u, known := o.ExecOutputUncapturedFor(ctx, "", types.CC2); !known || !u {
		t.Fatalf("CC2 before create = (%v, %v), want uncaptured: only Kubernetes takes it", u, known)
	}
	sb, err := o.CreateSandbox(ctx, runner.SandboxSpec{RunID: uuid.New(), ConfinementClass: types.CC1})
	if err != nil {
		t.Fatal(err)
	}
	// Docker stops taking CC1: a new CC1 run would go to Kubernetes, but this sandbox stays Docker's.
	docker.Support.Classes = nil
	clock = clock.Add(capsCacheTTL + time.Second)
	if u, known := o.ExecOutputUncapturedFor(ctx, "", types.CC1); !known || !u {
		t.Fatalf("CC1 with Docker gone = (%v, %v), want uncaptured: Kubernetes is now its first match", u, known)
	}
	if u, known := o.ExecOutputUncapturedFor(ctx, sb.Ref, types.CC1); !known || u {
		t.Fatalf("Docker's sandbox = (%v, %v), want captured: the substrate holding the ref answers", u, known)
	}
}
