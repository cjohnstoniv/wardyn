// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"
	"io"

	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

var _ runner.OutputRecoverer = (*Driver)(nil)

// RecoverOutput implements runner.OutputRecoverer for an exec-less agent: its
// workload is the container's main process, so dockerd's log of that TTY is the
// agent's whole output, kept for as long as the container exists. A finished
// container is read to its end; one still running is followed until it exits,
// which is how a process that adopts a live run resumes the capture.
//
// An exec agent (every runtime but krun) is runner.ErrOutputUnrecoverable: its
// output is a hijacked exec stream that dockerd does not log, and the
// container's own log holds only its idle holder. A container that is gone is
// runner.ErrSandboxGone. The runtime is read from the container, not from this
// process's memory, which does not survive the restart this exists for.
func (d *Driver) RecoverOutput(ctx context.Context, ref string, w io.Writer) error {
	res, err := d.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if err != nil {
		if isNotFound(err) {
			return runner.ErrSandboxGone
		}
		return fmt.Errorf("docker: recover output: inspect: %w", err)
	}
	c := res.Container
	if c.HostConfig == nil || runtimeSupportsExec(c.HostConfig.Runtime) {
		return runner.ErrOutputUnrecoverable
	}
	follow := c.State != nil && c.State.Running
	rc, err := d.cli.ContainerLogs(ctx, ref, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: follow})
	if err != nil {
		if isNotFound(err) {
			return runner.ErrSandboxGone
		}
		return fmt.Errorf("docker: recover output: logs: %w", err)
	}
	defer rc.Close()
	_, err = io.Copy(w, rc)
	return err
}
