// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

// armsDocker wraps fakeDocker with the two daemon answers these arms need: a
// scripted ContainerKill error, and an ImageList whose "reference" filter —
// like the real daemon's, which is tag-shaped — never matches a digest ref.
type armsDocker struct {
	*fakeDocker
	killErr    error
	inspectErr error
	stopErrFor string // ContainerStop of this one container fails
}

func (a *armsDocker) ContainerStop(ctx context.Context, id string, o client.ContainerStopOptions) (client.ContainerStopResult, error) {
	if id == a.stopErrFor {
		return client.ContainerStopResult{}, errors.New("Error response from daemon: cannot stop container: context deadline exceeded")
	}
	return a.fakeDocker.ContainerStop(ctx, id, o)
}

func (a *armsDocker) ContainerKill(ctx context.Context, id string, o client.ContainerKillOptions) (client.ContainerKillResult, error) {
	if a.killErr != nil {
		return client.ContainerKillResult{}, a.killErr
	}
	return a.fakeDocker.ContainerKill(ctx, id, o)
}

func (a *armsDocker) ImageList(context.Context, client.ImageListOptions) (client.ImageListResult, error) {
	return client.ImageListResult{}, nil
}

func (a *armsDocker) ImageInspect(ctx context.Context, ref string, o ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	if a.inspectErr != nil {
		return client.ImageInspectResult{}, a.inspectErr
	}
	return a.fakeDocker.ImageInspect(ctx, ref, o...)
}

// TestKillSandbox_NotRunningIsBenignRealErrorIsNot: a kill refused because the
// container already stopped still tears the sandbox down; any other kill error
// is returned and nothing is removed — never reported as a clean kill.
func TestKillSandbox_NotRunningIsBenignRealErrorIsNot(t *testing.T) {
	for _, tc := range []struct {
		name        string
		killErr     error
		wantErr     bool
		wantRemoved bool
	}{
		{"not running", errors.New("Error response from daemon: container abc is not running"), false, true},
		{"daemon error", errors.New("Error response from daemon: cannot kill container: permission denied"), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker()
			f.images["busybox:latest"] = true
			a := &armsDocker{fakeDocker: f}
			d := newWithClient(a, Config{ProxyImage: "wardyn-proxy:dev"})
			sb, err := d.CreateSandbox(context.Background(), testSpec())
			if err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}

			a.killErr = tc.killErr
			err = d.KillSandbox(context.Background(), sb.Ref)
			if (err != nil) != tc.wantErr {
				t.Fatalf("KillSandbox = %v, want error = %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "docker: kill") {
				t.Errorf("err = %v, want it to name the kill", err)
			}
			f.mu.Lock()
			removed := f.containers[sb.Ref] != nil && f.containers[sb.Ref].removed
			f.mu.Unlock()
			if removed != tc.wantRemoved {
				t.Errorf("agent removed = %v, want %v", removed, tc.wantRemoved)
			}
		})
	}
}

// TestStopProxy_EscalatesToKillAndNeverRemoves is #1060 (0.8 review F06): a
// proxy whose graceful stop fails is killed, which keeps the container and its
// env (a revive reads its config back), and the stop reports success with the
// agent left running. A proxy that survives the kill too is an error, and the
// agent is stopped so no work runs while its egress is unconfirmed. Nothing is
// ever removed: the writable layer is the files the run is kept for.
func TestStopProxy_EscalatesToKillAndNeverRemoves(t *testing.T) {
	for _, tc := range []struct {
		name         string
		killErr      error
		wantErr      bool
		agentRunning bool
	}{
		{"stop fails, kill lands", nil, false, true},
		{"stop and kill both fail", errors.New("Error response from daemon: cannot kill container: permission denied"), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker()
			f.images["busybox:latest"] = true
			a := &armsDocker{fakeDocker: f}
			d := newWithClient(a, Config{ProxyImage: "wardyn-proxy:dev"})
			sb, err := d.CreateSandbox(context.Background(), testSpec())
			if err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			proxy := proxyContainerName(testSpec().RunID)
			a.stopErrFor, a.killErr = proxy, tc.killErr

			err = d.StopProxy(context.Background(), sb.Ref)
			if (err != nil) != tc.wantErr {
				t.Fatalf("StopProxy = %v, want error = %v", err, tc.wantErr)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for name, c := range f.containers {
				if c.removed {
					t.Errorf("container %s was removed; a failed stop must never remove anything", name)
				}
			}
			if p := f.containers[proxy]; !tc.wantErr && (p.state == nil || p.state.Running) {
				t.Errorf("proxy after a landed kill = %+v; want it stopped", p.state)
			}
			if agent := f.containers[sb.Ref]; agent.state == nil || agent.state.Running != tc.agentRunning {
				t.Errorf("agent running = %+v, want %v", agent.state, tc.agentRunning)
			}
		})
	}
}

// TestImagePresent_DigestPinnedRef: a repo@sha256 ref is checked by inspect,
// because the tag-shaped list filter never matches it — a pre-pulled pinned
// image must read present (no pull against a registry the host may have no
// auth for), an absent one absent, and an inspect failure must surface rather
// than read as "absent" and trigger a pull.
func TestImagePresent_DigestPinnedRef(t *testing.T) {
	const pinned = "registry.example/agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	ctx := context.Background()

	f := newFakeDocker()
	f.images[pinned] = true
	a := &armsDocker{fakeDocker: f}
	d := newWithClient(a, Config{ProxyImage: "wardyn-proxy:dev"})
	if ok, err := d.imagePresent(ctx, pinned); !ok || err != nil {
		t.Fatalf("imagePresent(pre-pulled pinned) = (%v, %v), want (true, nil)", ok, err)
	}

	f.images[pinned] = false
	if ok, err := d.imagePresent(ctx, pinned); ok || err != nil {
		t.Fatalf("imagePresent(absent pinned) = (%v, %v), want (false, nil)", ok, err)
	}

	a.inspectErr = errors.New("Error response from daemon: i/o timeout")
	if ok, err := d.imagePresent(ctx, pinned); ok || err == nil || !strings.Contains(err.Error(), "image inspect") {
		t.Fatalf("imagePresent(inspect failed) = (%v, %v), want the inspect error", ok, err)
	}
}
