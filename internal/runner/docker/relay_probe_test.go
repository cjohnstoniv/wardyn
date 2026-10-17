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

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

const relayProbeTarget = "192.168.5.2"

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

func relayProbeDriver(f *fakeDocker) *Driver {
	f.images["wardyn-proxy:dev"] = true
	return newTestDriver(f)
}

func TestProbeRelayRunsOneHardenedThrowawayContainer(t *testing.T) {
	f := newFakeDocker()
	if err := relayProbeDriver(f).ProbeRelay(context.Background(), relayProbeTarget, 8443); err != nil {
		t.Fatal(err)
	}
	c := probeContainer(t, f)
	if c.cfg.Image != "wardyn-proxy:dev" ||
		!slices.Equal(c.cfg.Entrypoint, []string{"/usr/local/bin/wardyn-proxy"}) ||
		!slices.Equal(c.cfg.Cmd, []string{"-egress-canary", "wardyn-relay-probe:8443"}) {
		t.Errorf("image %q entrypoint %v cmd %v", c.cfg.Image, c.cfg.Entrypoint, c.cfg.Cmd)
	}
	if c.cfg.User != "" || len(c.cfg.Env) != 0 {
		t.Errorf("user %q env %v: the image's defaults are wanted", c.cfg.User, c.cfg.Env)
	}
	h := c.host
	if string(h.NetworkMode) != "wardyn-internal" || !slices.Equal(h.ExtraHosts, []string{"wardyn-relay-probe:" + relayProbeTarget}) {
		t.Errorf("network %q hosts %v", h.NetworkMode, h.ExtraHosts)
	}
	if !slices.Equal(h.CapDrop, []string{"ALL"}) || !slices.Contains(h.SecurityOpt, "no-new-privileges") || !h.ReadonlyRootfs ||
		h.AutoRemove || h.Memory == 0 || h.PidsLimit == nil || len(h.Mounts)+len(h.Binds) != 0 {
		t.Errorf("hardening wrong: %+v", h)
	}
	if c.cfg.Labels[labelManaged] != "true" || c.cfg.Labels[labelComponent] != componentRelayProbe {
		t.Errorf("labels %v", c.cfg.Labels)
	}
	if !c.removed || !c.forceRemoved {
		t.Errorf("probe container removed=%v force=%v", c.removed, c.forceRemoved)
	}
}

func TestProbeRelayLabelsTheRunner(t *testing.T) {
	f := newFakeDocker()
	f.images["wardyn-proxy:dev"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", RunnerID: "r-1"})
	if err := d.ProbeRelay(context.Background(), relayProbeTarget, 8443); err != nil {
		t.Fatal(err)
	}
	if got := probeContainer(t, f).cfg.Labels[runner.LabelRunnerID]; got != "r-1" {
		t.Errorf("runner label %q", got)
	}
}

func TestProbeRelayExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        int64
		wantErr     string
		unreachable bool
	}{
		{name: "reachable", code: 0},
		{name: "unreachable", code: 1, wantErr: "a container on this engine did not connect to 192.168.5.2:8443", unreachable: true},
		{name: "image without the canary", code: 2, wantErr: "the proxy image wardyn-proxy:dev did not run its reachability check (exit 2); it must be wardyn-proxy 0.9.0 or later"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker()
			f.probeExitCode = tc.code
			err := relayProbeDriver(f).ProbeRelay(context.Background(), relayProbeTarget, 8443)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if errors.Is(err, ErrRelayUnreachable) != tc.unreachable {
				t.Fatalf("errors.Is unreachable = %v, want %v (%v)", !tc.unreachable, tc.unreachable, err)
			}
			if c := probeContainer(t, f); !c.removed || !c.forceRemoved {
				t.Errorf("probe container removed=%v force=%v", c.removed, c.forceRemoved)
			}
		})
	}
}

func TestProbeRelayNeverReportsUnreachableForAProbeThatDidNotRun(t *testing.T) {
	t.Run("wait error", func(t *testing.T) {
		f := newFakeDocker()
		f.waitErrs = 1
		d := relayProbeDriver(f)
		if err := d.ProbeRelay(context.Background(), relayProbeTarget, 8443); err == nil || errors.Is(err, ErrRelayUnreachable) {
			t.Fatalf("err = %v", err)
		}
		if c := probeContainer(t, f); !c.removed || !c.forceRemoved {
			t.Error("probe container not force-removed")
		}
	})
	t.Run("ctx cancelled while waiting", func(t *testing.T) {
		f := newFakeDocker()
		f.waitHangs = true
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if err := relayProbeDriver(f).ProbeRelay(ctx, relayProbeTarget, 8443); err == nil || errors.Is(err, ErrRelayUnreachable) {
			t.Fatalf("err = %v", err)
		}
		if c := probeContainer(t, f); !c.removed || !c.forceRemoved {
			t.Error("probe container not force-removed")
		}
	})
	t.Run("start fails after a good create", func(t *testing.T) {
		f := newFakeDocker()
		f.failStartContainer = "wardyn-relay-probe-"
		if err := relayProbeDriver(f).ProbeRelay(context.Background(), relayProbeTarget, 8443); err == nil || errors.Is(err, ErrRelayUnreachable) {
			t.Fatalf("err = %v", err)
		}
		if c := probeContainer(t, f); !c.removed || !c.forceRemoved {
			t.Errorf("probe container removed=%v force=%v", c.removed, c.forceRemoved)
		}
	})
	t.Run("create fails", func(t *testing.T) {
		f := newFakeDocker()
		f.failCreateContainer = "wardyn-relay-probe-"
		if err := relayProbeDriver(f).ProbeRelay(context.Background(), relayProbeTarget, 8443); err == nil || errors.Is(err, ErrRelayUnreachable) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestProbeRelayNeverPulls(t *testing.T) {
	f := newFakeDocker()
	err := newTestDriver(f).ProbeRelay(context.Background(), relayProbeTarget, 8443)
	if err == nil || !strings.Contains(err.Error(), "wardyn-proxy:dev") || !strings.Contains(err.Error(), "docker pull") || errors.Is(err, ErrRelayUnreachable) {
		t.Fatalf("err = %v", err)
	}
	if f.images["wardyn-proxy:dev"] || len(f.containers) != 0 {
		t.Error("the probe pulled or created a container")
	}
}
