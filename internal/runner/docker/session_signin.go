// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"fmt"

	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

var _ runner.SigninExecer = (*Driver)(nil)

// ExecStreamAs is ExecStream as user (uid:gid). Desktop sign-ins run the
// person's stack this way (runner.SigninExecer); an empty user is refused so a
// caller can never fall back to the agent's uid by accident.
func (d *Driver) ExecStreamAs(ctx context.Context, ref, user string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if user == "" {
		return nil, errors.New("docker: exec stream as: no user")
	}
	return d.execStream(ctx, ref, user, spec)
}

// SignalMain sends signal to the container's main process from outside its
// pid namespace, which is the only place a stop or continue of pid 1 is
// delivered from.
func (d *Driver) SignalMain(ctx context.Context, ref, signal string) error {
	if _, err := d.cli.ContainerKill(ctx, ref, client.ContainerKillOptions{Signal: signal}); err != nil {
		return fmt.Errorf("docker: signal %s: %w", signal, err)
	}
	return nil
}
