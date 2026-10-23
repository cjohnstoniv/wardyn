// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Revoking a local run's Azure DevOps tokens when a write withdraws their
// runner_resident delivery. The relay judges that delivery again on every
// resolve (relayedResidentADO), but a run's proxy resolves only every
// residentRejudgeEvery, so until then a withdrawn delivery would leave a live
// token in the runner's proxy. Each write that can withdraw it calls
// revokeWithdrawnLocalPATs once it has committed (the two site-config doors on
// their way out, after the unlock: a run's token lock is never taken under the
// site-config lock):
//
//   - the connection's opt-in, its row disabled or its token mode:
//     handlePutSiteConfig and handlePutWorkspaceProviders;
//   - the delivery policy: handlePutCredentialDelivery and its held change
//     (applyCredentialDeliveryChange);
//   - the runner's trust class: handlePutRunnerTrust and its held change
//     (applyRunnerTrustChange);
//   - the runner's posture: PostureFromRunner.

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// revokeWithdrawnLocalPATs judges the runner_resident delivery of every live
// local run still holding an Azure DevOps token against the configuration as it
// now stands, and revokes the tokens of each run it no longer admits (reason
// drift). Best-effort: the write stands whatever happens here. A failed revoke
// is logged and audited by revokeRunPATs, and the run's next resolve judges it
// again.
func (s *Server) revokeWithdrawnLocalPATs(ctx context.Context) {
	st, ok := s.cfg.Store.(store.RunPATStore)
	if !ok {
		return
	}
	ctx = context.WithoutCancel(ctx)
	rows, err := st.ListUnrevokedRunPATs(ctx, store.RunPATFilter{})
	if err != nil {
		slog.WarnContext(ctx, "wardynd: listing the Azure DevOps tokens a withdrawn local delivery may revoke failed; each run's next resolve judges it again",
			slog.Any("err", err))
		return
	}
	judged := map[uuid.UUID]bool{}
	for _, p := range rows {
		if judged[p.RunID] {
			continue
		}
		judged[p.RunID] = true
		if s.localResidentWithdrawn(ctx, p.RunID) {
			s.revokeRunPATs(ctx, p.RunID, adoPATRevokeDrift)
		}
	}
}

// localResidentWithdrawn reports whether runID is a live local run with a
// runner_resident Azure DevOps grant its delivery no longer admits. A read that
// fails withdraws nothing.
func (s *Server) localResidentWithdrawn(ctx context.Context, runID uuid.UUID) bool {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || run.State.IsTerminal() || run.Placement != types.PlacementLocal || run.RunnerID == nil {
		return false
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, runID)
	if err != nil {
		return false
	}
	for _, g := range grants {
		if g.Delivery != types.GrantDeliveryResident || g.Spec.Kind != types.GrantAPIKey {
			continue
		}
		if _, withdrawn := s.judgeResidentADO(ctx, run, run.CreatedBy, g); withdrawn {
			return true
		}
	}
	return false
}

// judgeResidentADO judges one runner_resident grant of run, owned by owner, as
// an Azure DevOps minted token delivered to run's runner now: admitted, or
// withdrawn (refused, so the run's tokens are to be revoked). It is neither for
// a grant of any other class and for a read that failed.
func (s *Server) judgeResidentADO(ctx context.Context, run types.AgentRun, owner string, grant types.CredentialGrant) (admitted, withdrawn bool) {
	origin, ref := s.localGrantOrigin(ctx, owner, grant.Spec)
	if ref != nil || !residentADOOrigin(origin) {
		return false, false
	}
	r, err := s.residentADOAdmits(ctx, run, origin)
	if err != nil {
		return false, false
	}
	return r == nil, r != nil
}
