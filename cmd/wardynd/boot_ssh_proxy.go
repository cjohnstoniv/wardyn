// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
)

// validateSSHProxyCommand refuses a WARDYN_SSH_PROXY_COMMAND that would break
// out of the quoted one-liner or the ssh_config line people paste it into.
// The daemon never runs the value; the bound protects the person's computer.
func validateSSHProxyCommand(v string) error {
	if err := cliutil.CheckSSHProxyCommand(v); err != nil {
		return fmt.Errorf("refusing to start: -ssh-proxy-command (WARDYN_SSH_PROXY_COMMAND) %w; "+
			"it is pasted into a shell one-liner inside single quotes and onto one ssh_config line", err)
	}
	return nil
}

// maxSSHSessionsPerRunLimit is the upper bound on WARDYN_SSH_MAX_SESSIONS_PER_RUN:
// the gateway's own total-connection cap, since every channel is a live exec in
// one sandbox and a run holding more than that is a leak, not a workflow.
const maxSSHSessionsPerRunLimit = 64

func validateSSHMaxSessionsPerRun(n int) error {
	if n < 1 || n > maxSSHSessionsPerRunLimit {
		return fmt.Errorf("refusing to start: WARDYN_SSH_MAX_SESSIONS_PER_RUN is %d; want an integer from 1 to %d", n, maxSSHSessionsPerRunLimit)
	}
	return nil
}
