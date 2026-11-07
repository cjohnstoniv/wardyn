// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package guestagent

import (
	"context"
	"fmt"
	"time"
)

// sessionEnvRetryRun and sessionEnvRetryPaths are what `agent-run --session-env-retry` uses: variables so
// a test stands in for launchctl and the files.
var (
	sessionEnvRetryRun   CmdRunner = ExecRunner{}
	sessionEnvRetryPaths           = SessionEnvPaths{}
)

// sessionEnvRetryWait bounds the retry's launchctl calls.
const sessionEnvRetryWait = 30 * time.Second

// sessionEnvRetry is `agent-run --session-env-retry`: the plain retry the window offers when the run's
// settings did not reach the desktop session. Run from a Terminal in the window, it sets them in that
// session as the person, and says the outcome in one line (the same line the status file holds).
func (a *AgentRun) sessionEnvRetry() int {
	ctx, cancel := context.WithTimeout(context.Background(), sessionEnvRetryWait)
	defer cancel()
	st, err := SessionEnvRetry(ctx, sessionEnvRetryPaths, sessionEnvRetryRun)
	if err != nil && st.State == "" {
		a.say("the run's settings could not be read: " + err.Error())
		return 1
	}
	fmt.Fprintln(a.Stdout, st.Line())
	if st.State != SessionEnvSet {
		a.say("the run's settings did not take in this session.")
		return 1
	}
	return 0
}
