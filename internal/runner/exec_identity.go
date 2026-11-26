// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "errors"

// MainProcessExecID identifies a successful exec-less launch. An empty stored
// exec ID instead means that agent launch never completed. AgentStatus resolves
// this sentinel to the container main process rather than inspecting an exec.
const MainProcessExecID = "main-process"

// ErrAgentDirUnshared refuses an exec confined to a folder of the agent's (ExecSpec.AgentDir) once the agent's own
// container has ended, where that folder is on no volume the sandbox's main container shares with it.
var ErrAgentDirUnshared = errors.New("runner: the agent's container has ended, and the folder is on no volume the sandbox's main container shares with it")
