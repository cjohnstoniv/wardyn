// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

// SelftestArgv is the BYOI image's contract check. The organisation execs it before the task
// (internal/api/runs_dispatch_byoi.go) and a runner's agent recognises it by this same argv to keep
// its output out of the run's, so both sides name it here and cannot drift apart.
func SelftestArgv() []string { return []string{"/usr/local/bin/agent-run", "--selftest"} }
