// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The run launch, split at the run row.
//
// POST /api/v1/runs used to hold the request open through the image build and
// dispatchRun. imageBuildTimeout is 30 minutes and CreateSandbox blocks for as
// long as the substrate needs, while the console's launch deadline is 5
// minutes: a bring-your-own-image launch that took longer was answered with
// the unreachable-daemon sentence while the run went on without anyone
// holding its id.
//
// The split point is the run row. Every refusal — 4xx, 422, and the 500s that
// the abort compensator covers — is answered by handleCreateRun before it
// writes the 201, so no refusal can become a failed run. What is left can only
// build, dispatch, or fail the run itself: finishCreateRunLaunch, on a
// context.WithoutCancel so the client's own disconnect cannot strand a
// half-provisioned sandbox. It mirrors the sign-in lane's split
// (harnesscred_launch.go).
//
// Why a kill landing during the build or pull is safe: dispatchRun claims
// PENDING->STARTING with a CAS (runs_dispatch.go), so a POST /runs/{id}/kill
// that wins the race makes dispatch ABORT rather than resurrect the run, and a
// failed build CASes from PENDING too, so it cannot clobber a KILLED run.

// createRunLaunch is what handleCreateRun resolved and the detached tail still
// needs. The ceiling travels already translated — the handler passes
// ceilingForDispatch(ceiling), never a re-resolution here: resolveDispatchCeiling
// memoizes per request and effectiveCeiling keys on OIDC claims, so the
// handler's resolution is the only one this run gets.
type createRunLaunch struct {
	req           createRunRequest
	spec          types.RunPolicySpec
	ceiling       dispatchCeiling
	gw            grantWiring
	wsRefs        []types.Workspace
	driveMount    *types.DriveMount
	ephemeralDirs []string
	bedrockRef    *types.WorkspaceBedrockRef
	runToken      string
	created       types.AgentRun
}

// DRAFT (M2 canon pending) — the run's failure_hint when the detached launch
// PANICS. The caller already holds a 201, so the run itself has to carry the
// reason. Names the class and where the detail is, since a panic's own text is
// a stack trace nobody should read off a console banner.
const createRunInternalError = "This run hit an internal error while starting — try again; if it repeats, check the daemon log."

// finishCreateRunLaunch is the part of the launch that can block: the sandbox
// image build, then dispatchRun. It runs detached, after the caller already
// holds a 201 and a run id. With no runner wired the run stays PENDING, as it
// always has.
func (s *Server) finishCreateRunLaunch(ctx context.Context, l createRunLaunch) {
	runID := l.created.ID
	// Contain a panic so a launch bug cannot take the daemon down. Fail from the
	// run's CURRENT state: by the time anything can realistically panic,
	// dispatchRun has CASed PENDING->STARTING, and failAndRevoke's non-applied
	// path is silent — see finishHarnessLoginLaunch for the full argument.
	defer func() {
		if rec := recover(); rec != nil {
			slog.ErrorContext(ctx, "wardynd: run launch panicked",
				slog.String("run_id", runID.String()), slog.Any("panic", rec))
			from := types.RunPending
			if got, gerr := s.cfg.Store.GetRun(ctx, runID); gerr == nil && !isTerminalRunState(got.State) {
				from = got.State
			}
			s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.dispatch",
				runID.String(), "failure", mustJSON(map[string]any{
					"note": "the detached run launch panicked; the run is failed rather than left non-terminal",
				})))
			s.failAndRevoke(ctx, runID, from, createRunInternalError)
		}
	}()

	// Resolve the sandbox image (BYOI wrap > devcontainer build > workspace
	// profile > convention image) and persist it for provenance. A failed
	// BYOI/devcontainer build has already marked the run FAILED.
	image, failed := s.resolveCreateRunImage(ctx, l.req, runID, l.wsRefs)
	if failed {
		return
	}

	// Dispatch the sandbox if a runner is wired; otherwise stay PENDING.
	if s.cfg.Runner != nil {
		// The dispatch-time deny re-assertion's inputs (runs_dispatch_ceiling.go).
		// A create-time deny alone is not enough — the artifact-redirect phase INSIDE
		// dispatch adds corporate hosts and authors token injections for them, AFTER
		// the handler's clamp ran — so the profile's walls are re-asserted there.
		s.dispatchRun(ctx, l.created, l.ceiling, dispatchParams{
			RunToken:           l.runToken,
			Image:              image,
			Policy:             l.spec,
			FirstGitHubGrantID: l.gw.firstGitHubGrantID,
			GitGrants:          l.gw.gitGrants,
			GitPATGrants:       l.gw.gitPATGrants,
			SSHGrants:          l.gw.sshGrants,
			Injections:         l.gw.injections,
			Interactive:        l.req.Interactive,
			TaskMode:           l.req.TaskMode,
			InteractiveStart:   l.req.InteractiveStart,
			SeedAutoTools:      l.req.SeedAutoTools,
			ToolApprovals:      l.req.ToolApprovals,
			BedrockRef:         l.bedrockRef,
			EphemeralDirs:      l.ephemeralDirs,
			Toolchains:         runToolchainNeeds(l.wsRefs),
			// The member's own persistent storage, already resolved and narrowed
			// at create (seedRequestDrive) — nil unless this run asked for it.
			// Carried rather than re-resolved for the reason the ceiling is:
			// resolution keys on the caller's OIDC claims, which the run row does
			// not hold. See user_drives_run.go's own note.
			Drive: l.driveMount,
			// The zero posture unless this run attaches a MEMBER-OWNED workspace, in
			// which case the driver re-checks that member's own binds against these
			// roots immediately before ContainerCreate (userMountPosture,
			// workspace_refs.go).
			UserMounts: s.userMountPosture(l.wsRefs),
		})
	}
}
