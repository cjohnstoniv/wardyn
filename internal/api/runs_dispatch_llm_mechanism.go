// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The model-credential refusal class and the two questions every door asks
// before choosing a model provider: is this a model run, and what does the site
// config say. The declared-mechanism gate that used to live here compared a
// fixed lane precedence against the roster; the model provider a run chooses
// replaced both (multi-provider design §2.5).
package api

import (
	"context"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// llmRefusalAuditReason is the MACHINE-READABLE class on a model-provider
// refusal's run.create/failure audit row and 422: "the run was refused over a
// model credential". Not copy: a wire value the console grades an ending by
// (lib/api/audit.ts's CREDENTIAL_REASON), so it never changes with the wording.
//
// Deliberately NOT narrowed to "a sign-in repairs it": the server states the
// CLASS, and the console decides whether to offer a door from the same
// model-access grading every other surface reads — a refusal whose renewal
// merely did not complete ("launch again in a moment") grades live and gets no
// button, correctly, without this key knowing anything about it.
const llmRefusalAuditReason = "model_credential"

// createDoorIsModelRun answers isModelRun's own question for a create-door
// REQUEST rather than a resolved run: workspace id AND source id are nil by
// construction on this door (seedRequestWorkspace, runs_create.go, never sets
// run.WorkspaceID from req.WorkspaceID — that column is the TRUSTED
// scan/verify/record linkage a user-facing create must never claim — and a
// source-bound run, record/verify/build, is launched by newStepRun, never
// decoded from this door's body), so this door can never produce the
// (workspace_id/source_id + non-interactive) shape isModelRun reads as a scan.
// Passing req.WorkspaceID through used to tell a caller of this an ordinary
// `--workspace` launch (docs/OPERATIONS.md, the console's workspace_id) was a
// scan, while dispatch — reading the run's own, never-set WorkspaceID —
// decided the opposite and dispatched it as a model run anyway. A login run
// is also never a model run here, whatever isModelRun would answer.
//
// enforceRunModelProvider (run_model_provider.go) asks exactly this (#767 step
// 2), so the create door and dispatch can never ask a different question.
func createDoorIsModelRun(req createRunRequest) bool {
	return req.Task != harnessLoginTask && isModelRun(req.TaskMode, nil, nil, req.Interactive)
}

// siteConfigForDispatch reads the operator-wide site config for ONE dispatch,
// retrying a failed read exactly once.
//
// Why a retry belongs here and nowhere else. This one read decides three
// things at once: which artifact redirects apply, which upstream proxy the run
// gets, and — the one that matters — which model provider serves it. A failed
// read refuses a model run outright (resolveProviderLane's mpRunUnreadable)
// rather than guess. That is the right answer for a genuinely unreadable site
// config and the wrong one for a single dropped connection, which is what a pgx
// pool blip usually is.
//
// ONE retry, immediately, with no delay: a dead pool answers instantly so the
// cost of the second attempt is nil, and a transient failure is very often gone
// by the next call. A loop would turn one Postgres hiccup into a create request
// that hangs; the sweep and the caller's own retry cover everything past that.
//
// It deliberately does NOT widen what is served on failure. The refusal
// downstream is unchanged, so a deployment whose site config really cannot be
// read still fails its model runs closed.
func (s *Server) siteConfigForDispatch(ctx context.Context) (types.SiteConfig, error) {
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err == nil {
		return sc, nil
	}
	slog.WarnContext(ctx, "wardynd: the site config read failed at dispatch; retrying once before the model provider is decided",
		slog.Any("err", err))
	return s.cfg.Store.GetSiteConfig(ctx)
}
