// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// ProxyFence is advertised for a substrate that can cut an agent off its networks, never for the process: a
// substrate with the Kubernetes driver's method set, which has no FenceProxy, does not send it.
func TestHostCaps_ProxyFenceFollowsTheSubstrate(t *testing.T) {
	ctx := context.Background()
	fencing := runnertest.NewFake()
	caps, err := hostCaps(ctx, fencing, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if !caps.ProxyFence {
		t.Error("a substrate that implements runner.ProxyFencer: ProxyFence = false; want true")
	}
	// Embedding the interface leaves only its methods, as the Kubernetes driver's method set does.
	caps, err = hostCaps(ctx, struct{ substrate.Substrate }{fencing}, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if caps.ProxyFence {
		t.Error("a substrate without FenceProxy advertised ProxyFence; want it off")
	}
}

// ProxyNarrow is advertised for a substrate that narrows a proxy in place, and not for one that does not.
func TestHostCaps_ProxyNarrowFollowsTheSubstrate(t *testing.T) {
	ctx := context.Background()
	plain := runnertest.NewFake()
	caps, err := hostCaps(ctx, plain, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if caps.ProxyNarrow {
		t.Error("a substrate that is not a runner.ProxyNarrower advertised ProxyNarrow; want it off")
	}
	caps, err = hostCaps(ctx, narrowingHost{plain}, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if !caps.ProxyNarrow {
		t.Error("a substrate that implements runner.ProxyNarrower: ProxyNarrow = false; want true")
	}
}

// narrowingHost is a substrate with the Kubernetes driver's narrowing methods.
type narrowingHost struct{ substrate.Substrate }

func (narrowingHost) ProxyNarrowState(context.Context, string) (runner.ProxyNarrowState, error) {
	return runner.ProxyNarrowState{}, nil
}

func (narrowingHost) NarrowProxy(context.Context, string, []string) (runner.ProxyNarrowState, error) {
	return runner.ProxyNarrowState{}, nil
}

// MoveImportPresent is advertised only for a substrate with the Docker driver's marker
// (runner.MoveImportPresenter): runnertest's plain fake and a substrate with the Kubernetes driver's method set
// (the embedded interface) do not send it, and the Docker driver has it. MoveState is sent by every substrate.
func TestHostCaps_MoveImportPresentFollowsTheSubstrate(t *testing.T) {
	ctx := context.Background()
	plain := runnertest.NewFake()
	for name, sub := range map[string]substrate.Substrate{
		"runnertest's plain fake":            plain,
		"the Kubernetes driver's method set": struct{ substrate.Substrate }{plain},
	} {
		caps, err := hostCaps(ctx, sub, "amd64")
		if err != nil {
			t.Fatal(err)
		}
		if caps.MoveImportPresent {
			t.Errorf("%s advertised MoveImportPresent; want it off", name)
		}
		if !caps.MoveState {
			t.Errorf("%s: MoveState = false; want true", name)
		}
	}
	caps, err := hostCaps(ctx, presentHost{plain}, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if !caps.MoveImportPresent {
		t.Error("a substrate that implements runner.MoveImportPresenter: MoveImportPresent = false; want true")
	}
	// The Docker driver has the marker, so a Docker runner sends the cap; the Kubernetes driver's lack of it is
	// that package's TestDriverIsNoMoveImportPresenter.
	if _, ok := any(&docker.Driver{}).(runner.MoveImportPresenter); !ok {
		t.Error("the Docker driver is not a runner.MoveImportPresenter; a Docker runner would not send MoveImportPresent")
	}
}

// presentHost is a substrate with the Docker driver's move marker.
type presentHost struct{ substrate.Substrate }

func (presentHost) MoveImportReadsPresent() {}
