// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"regexp"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sandboxWhereValue is a value a runner may report about where a sandbox runs: a Kubernetes object name (a node,
// a RuntimeClass or its handler). Anything else is left out of the row, never written as the runner sent it.
var sandboxWhereValue = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,251}[a-z0-9])?$`)

// auditSandboxStart writes run.sandbox.start: the node a sandbox was scheduled on, and the RuntimeClass and
// handler it runs under, as its runner reported them. It is written once the create answered, before the
// readback, so a start that then fails still says where it ran. No row when the runner reported nothing
// (Docker, an older runner) or nothing valid.
func (s *Server) auditSandboxStart(ctx context.Context, run types.AgentRun, sb runner.Sandbox) {
	if sb.Where == nil {
		return
	}
	fields := map[string]any{}
	for k, v := range map[string]string{
		"node": sb.Where.Node, "runtime_class": sb.Where.RuntimeClass, "runtime_handler": sb.Where.RuntimeHandler,
	} {
		if sandboxWhereValue.MatchString(v) {
			fields[k] = v
		}
	}
	if len(fields) == 0 {
		return
	}
	fields["sandbox_ref"] = sb.Ref
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.sandbox.start",
		run.ID.String(), "success", mustJSON(fields)))
}
