// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/containerd/errdefs"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

func TestProbeSubstrate_ClassifiesThePing(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want runner.SubstrateState
	}{
		{"answers", nil, runner.SubstrateOK},
		{"transport failure", errors.New("dial unix /var/run/docker.sock: connect: no such file or directory"), runner.SubstrateUnreachable},
		{"credential refused", fmt.Errorf("ping: %w", errdefs.ErrUnauthenticated), runner.SubstrateUnauthorized},
		{"permission denied on the socket", fmt.Errorf("ping: %w", os.ErrPermission), runner.SubstrateForbidden},
		{"daemon refuses the verb", fmt.Errorf("ping: %w", errdefs.ErrPermissionDenied), runner.SubstrateForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker()
			f.pingErr = tc.err
			if got := newTestDriver(f).ProbeSubstrate(context.Background()); got != tc.want {
				t.Fatalf("ProbeSubstrate = %q, want %q", got, tc.want)
			}
		})
	}
}

// A daemon that never answers is unreachable once the caller's deadline ends,
// and the call returns then rather than waiting on it.
func TestProbeSubstrate_DeadlineIsUnreachable(t *testing.T) {
	f := newFakeDocker()
	f.pingBlock = true
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan runner.SubstrateState, 1)
	go func() { done <- newTestDriver(f).ProbeSubstrate(ctx) }()
	select {
	case got := <-done:
		if got != runner.SubstrateUnreachable {
			t.Fatalf("ProbeSubstrate = %q, want unreachable", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ProbeSubstrate did not return after its deadline")
	}
}
