// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

// MainProcessExecID identifies a successful exec-less launch. An empty stored
// exec ID instead means that agent launch never completed. AgentStatus resolves
// this sentinel to the container main process rather than inspecting an exec.
const MainProcessExecID = "main-process"
