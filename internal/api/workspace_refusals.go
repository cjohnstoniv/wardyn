// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "fmt"

// The sentences of the workspace refusals New Run shows inline. The console
// prints the server's sentence on a refusal and uses the same bytes for its own
// pre-check (ui/src/app/lib/new-run-refusals.ts), so the two never disagree;
// TestWorkspaceRefusalSentencesMatchGolden pins both sides to one table
// (ui/src/app/lib/workspace-refusals.golden.json). Change a sentence in both
// files and the table together.

func workspaceTargetShapeMsg(path string) string {
	return fmt.Sprintf("%s must be an absolute path under /home/agent, such as /home/agent/work.", path)
}

func workspaceOverlapEqualMsg(path, other string) string {
	return fmt.Sprintf("%s is also where %s mounts. Give one of them another path.", path, other)
}

func workspaceOverlapNestedMsg(path, other, otherPath string) string {
	return fmt.Sprintf("%s is inside %s, where %s mounts. Two mounts can't nest.", path, otherPath, other)
}

func workspacePinConflictMsg(a, pinA, b, pinB string) string {
	return fmt.Sprintf("%s uses %s and %s uses %s. A run uses one model provider, so remove one of them.", a, pinA, b, pinB)
}

func workspaceImageConflictMsg(a, b string) string {
	return fmt.Sprintf("%s and %s use different base images. A run uses one image, so remove one of them.", a, b)
}

func workspaceADOOrgConflictMsg(a, orgA, b, orgB string) string {
	return fmt.Sprintf("%s uses Azure DevOps organisation %s and %s uses %s. A run signs in to one Azure DevOps organisation, so remove one of them.", a, orgA, b, orgB)
}
