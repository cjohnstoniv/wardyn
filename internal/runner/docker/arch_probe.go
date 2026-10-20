// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

const (
	componentArchProbe  = "arch-probe"
	archProbeNamePrefix = "wardyn-arch-probe-"
	// archProbeSetup bounds create, start, log read and removal around the wait.
	archProbeSetup = 15 * time.Second
	archProbeClean = 10 * time.Second
)

// archProbeUser runs the probe as the agent (runner.AgentUID), never as root.
var archProbeUser = strconv.Itoa(runner.AgentUID)

// archProbeWait is how long the probe container may run before the image is taken to start fine.
// A variable so a test need not wait it out.
var archProbeWait = 3 * time.Second

// archVerdictKey keys the runnable verdict: emulation depends on the engine and the runtime.
type archVerdictKey struct{ arch, runtime string }

// archVerdictKind is what a probe proved.
type archVerdictKind int

const (
	archInconclusive archVerdictKind = iota // the probe could not tell: allow this once, cache nothing
	archRunnable                            // the image started, or exited without an exec format error
	archUnrunnable                          // the engine cannot exec the image
)

// probeExecFormat starts ref once under its own entrypoint and under runtime, the runtime the image
// is about to run under ("" is the engine default), with no network, no mounts and the hardened
// settings, and reports what that proved. Only two outcomes prove anything: the container
// exited and its log says "exec format error" (or the start itself failed with it; crun reports it
// from the start call, and runc and crun differ in case, so the match ignores case), which is
// archUnrunnable; and a container still running at archProbeWait or exited with a clean log,
// which is archRunnable. Everything else (no command to run, a wait error, a cancelled ctx, an
// unreadable log) is archInconclusive.
//
// The probe takes the sandbox's hardened host config for that runtime, so it runs no image code
// outside the confinement the image would run under anyway.
func (d *Driver) probeExecFormat(ctx context.Context, ref, runtime string) archVerdictKind {
	ctx, cancel := context.WithTimeout(ctx, archProbeSetup+archProbeWait)
	defer cancel()

	name := archProbeNamePrefix + uuid.New().String()
	labels := map[string]string{labelManaged: "true", labelComponent: componentArchProbe}
	if d.cfg.RunnerID != "" {
		labels[runner.LabelRunnerID] = d.cfg.RunnerID // lets a sweeper claim a stray
	}
	remove := func(id string) {
		rctx, rcancel := context.WithTimeout(context.Background(), archProbeClean)
		defer rcancel()
		_, _ = d.cli.ContainerRemove(rctx, id, client.ContainerRemoveOptions{Force: true})
	}
	info, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return archInconclusive
	}
	host := hardenedHostConfig("none", runtime, runner.Resources{}, info.Info)
	host.ReadonlyRootfs = true
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       name,
		Config:     &container.Config{Image: ref, User: archProbeUser, Labels: labels},
		HostConfig: host,
	})
	if err != nil {
		remove(name) // a create that timed out client-side may still have made it
		return archInconclusive
	}
	defer remove(created.ID)

	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		if isExecFormat(err.Error()) {
			return archUnrunnable
		}
		return archInconclusive
	}
	wait := d.cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	timer := time.NewTimer(archProbeWait)
	defer timer.Stop()
	select {
	case res := <-wait.Result:
		if res.Error != nil {
			return archInconclusive
		}
		tail, err := d.archProbeLog(ctx, created.ID)
		switch {
		case err != nil:
			return archInconclusive
		case isExecFormat(tail):
			return archUnrunnable
		}
		return archRunnable
	case <-timer.C: // still running: it started, so it can run
		return archRunnable
	case <-wait.Error:
	case <-ctx.Done():
	}
	return archInconclusive
}

// archProbeLog is the probe's log tail; an error means the log could not be read, so the exit proves nothing.
func (d *Driver) archProbeLog(ctx context.Context, id string) (string, error) {
	rc, err := d.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: "20"})
	if err != nil {
		return "", err
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := stdcopy.StdCopy(&buf, &buf, rc); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func isExecFormat(s string) bool {
	return strings.Contains(strings.ToLower(s), "exec format error")
}
