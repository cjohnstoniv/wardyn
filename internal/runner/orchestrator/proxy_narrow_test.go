// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// narrowingSubstrate is a fakeSubstrate that narrows a proxy in place and records each call.
type narrowingSubstrate struct {
	*fakeSubstrate
	probed, narrowed []string
	denies           [][]string
}

func (n *narrowingSubstrate) ProxyNarrowState(_ context.Context, ref string) (runner.ProxyNarrowState, error) {
	n.rec(&n.probed, ref)
	return runner.ProxyNarrowState{ConfigSHA256: "cfg", BaseSHA256: "base"}, nil
}

func (n *narrowingSubstrate) NarrowProxy(_ context.Context, ref string, denies []string) (runner.ProxyNarrowState, error) {
	n.rec(&n.narrowed, ref)
	n.denies = append(n.denies, denies)
	return runner.ProxyNarrowState{ConfigSHA256: "next", LiveDenies: denies, Changed: true}, nil
}

// A substrate that narrows is forwarded to, with the same ref and list. One that does not answers
// ErrNarrowNotOffered with no call, so a Docker edit stays on the revive path.
func TestProxyNarrower_ForwardsAndNotOffered(t *testing.T) {
	ctx := context.Background()
	sub := &narrowingSubstrate{fakeSubstrate: &fakeSubstrate{name: "k8s", classes: []types.ConfinementClass{types.CC1}}}
	var r runner.Runner = New(sub)
	n, ok := r.(runner.ProxyNarrower)
	if !ok {
		t.Fatal("the orchestrator is not a runner.ProxyNarrower")
	}
	st, err := n.ProxyNarrowState(ctx, "wardyn-agent-x")
	if err != nil || st.ConfigSHA256 != "cfg" || st.BaseSHA256 != "base" || len(sub.probed) != 1 {
		t.Fatalf("ProxyNarrowState = %+v, %v; probed %v", st, err, sub.probed)
	}
	st, err = n.NarrowProxy(ctx, "wardyn-agent-x", []string{"a.example"})
	if err != nil || st.ConfigSHA256 != "next" || !st.Changed || len(sub.narrowed) != 1 ||
		len(sub.denies) != 1 || len(sub.denies[0]) != 1 || sub.denies[0][0] != "a.example" {
		t.Fatalf("NarrowProxy = %+v, %v; narrowed %v denies %v", st, err, sub.narrowed, sub.denies)
	}

	plain := &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}
	o := New(plain)
	if st, err := o.ProxyNarrowState(ctx, "wardyn-agent-x"); !errors.Is(err, runner.ErrNarrowNotOffered) || st.ConfigSHA256 != "" {
		t.Fatalf("ProxyNarrowState on a substrate that does not narrow: %+v, %v; want ErrNarrowNotOffered", st, err)
	}
	if st, err := o.NarrowProxy(ctx, "wardyn-agent-x", []string{"a.example"}); !errors.Is(err, runner.ErrNarrowNotOffered) || st.ConfigSHA256 != "" {
		t.Fatalf("NarrowProxy on a substrate that does not narrow: %+v, %v; want ErrNarrowNotOffered", st, err)
	}
}
