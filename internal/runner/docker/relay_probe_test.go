// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func relayProbeFixture() RelayProbe {
	return RelayProbe{Network: "wardyn-internal", Host: "wardynd.internal:192.168.5.2", Target: "wardynd.internal:443", Timeout: 200 * time.Millisecond}
}

// probeContainer returns the one relay-probe container the fake saw.
func probeContainer(t *testing.T, f *fakeDocker) *createdContainer {
	t.Helper()
	var found *createdContainer
	for name, c := range f.containers {
		if strings.HasPrefix(name, "wardyn-relay-probe-") {
			if found != nil {
				t.Fatal("more than one probe container")
			}
			found = c
		}
	}
	if found == nil {
		t.Fatal("no probe container was created")
	}
	return found
}

func TestProbeRelay_ExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        int64
		hang        bool
		wantErr     bool
		unreachable bool
	}{
		{name: "reachable", code: 0},
		{name: "unreachable", code: 1, wantErr: true, unreachable: true},
		{name: "other exit", code: 125, wantErr: true},
		{name: "timeout", hang: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker()
			f.images["wardyn-proxy:dev"] = true
			f.probeExitCode, f.waitHangs = tc.code, tc.hang
			err := newTestDriver(f).ProbeRelay(context.Background(), relayProbeFixture())
			if (err != nil) != tc.wantErr || errors.Is(err, ErrRelayUnreachable) != tc.unreachable {
				t.Fatalf("err = %v; wantErr %v unreachable %v", err, tc.wantErr, tc.unreachable)
			}
			if tc.unreachable && (!strings.Contains(err.Error(), "wardynd.internal:192.168.5.2") || !strings.Contains(err.Error(), "wardynd.internal:443")) {
				t.Errorf("error lacks host/target: %v", err)
			}
			c := probeContainer(t, f)
			if !c.removed || !c.forceRemoved {
				t.Errorf("probe container removed=%v force=%v", c.removed, c.forceRemoved)
			}
		})
	}
}

func TestProbeRelay_CreateRequest(t *testing.T) {
	f := newFakeDocker()
	f.images["wardyn-proxy:dev"] = true
	if err := newTestDriver(f).ProbeRelay(context.Background(), relayProbeFixture()); err != nil {
		t.Fatal(err)
	}
	c := probeContainer(t, f)
	if c.cfg.Image != "wardyn-proxy:dev" || !slices.Equal(c.cfg.Cmd, []string{"-egress-canary", "wardynd.internal:443"}) {
		t.Errorf("image %q cmd %v", c.cfg.Image, c.cfg.Cmd)
	}
	if c.cfg.User != "" {
		t.Errorf("user %q: the image's default is wanted", c.cfg.User)
	}
	h := c.host
	if string(h.NetworkMode) != "wardyn-internal" || !slices.Equal(h.ExtraHosts, []string{"wardynd.internal:192.168.5.2"}) {
		t.Errorf("network %q hosts %v", h.NetworkMode, h.ExtraHosts)
	}
	if !slices.Equal(h.CapDrop, []string{"ALL"}) || !slices.Contains(h.SecurityOpt, "no-new-privileges") || !h.ReadonlyRootfs || h.Memory == 0 || h.PidsLimit == nil {
		t.Errorf("hardening missing: %+v", h)
	}
	if c.cfg.Labels[labelManaged] != "true" || c.cfg.Labels[labelComponent] != componentRelayProbe {
		t.Errorf("labels %v", c.cfg.Labels)
	}
}

func TestProbeRelay_DefaultsNetwork(t *testing.T) {
	f := newFakeDocker()
	f.images["wardyn-proxy:dev"] = true
	p := relayProbeFixture()
	p.Network = ""
	if err := newTestDriver(f).ProbeRelay(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := probeContainer(t, f).host.NetworkMode; got != "wardyn-internal" {
		t.Errorf("network %q", got)
	}
}

func TestProbeRelay_PullsAnAbsentImage(t *testing.T) {
	f := newFakeDocker()
	if err := newTestDriver(f).ProbeRelay(context.Background(), relayProbeFixture()); err != nil {
		t.Fatal(err)
	}
	if !f.images["wardyn-proxy:dev"] {
		t.Error("image not pulled")
	}
}

func TestProbeRelay_ImageStillAbsentAfterPull(t *testing.T) {
	f := newFakeDocker()
	f.pullKeepsNothing = true
	err := newTestDriver(f).ProbeRelay(context.Background(), relayProbeFixture())
	if err == nil || !strings.Contains(err.Error(), "wardyn-proxy:dev") || !strings.Contains(err.Error(), "pull") || errors.Is(err, ErrRelayUnreachable) {
		t.Fatalf("err = %v", err)
	}
	if len(f.containers) != 0 {
		t.Error("a container was created without the image")
	}
}

func TestProbeRelay_CreateFailureIsNotUnreachable(t *testing.T) {
	f := newFakeDocker()
	f.images["wardyn-proxy:dev"] = true
	f.failCreateContainer = "wardyn-relay-probe-"
	err := newTestDriver(f).ProbeRelay(context.Background(), relayProbeFixture())
	if err == nil || errors.Is(err, ErrRelayUnreachable) {
		t.Fatalf("err = %v", err)
	}
}
