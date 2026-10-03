package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// brokeredPATGrantIDs is every git_pat grant id of the run, for
// proxy.Config.BrokeredPATGrantIDs: with the PAT broker on, the proxy refuses a
// raw mint of any of them at /wardyn/v1/credentials/mint.
//
// It reads the run's stored grants rather than the dispatch maps on purpose.
// PATGrants and GitPATGrants are narrowed per host (a same-host shadow, a vetoed
// lane, a brokered forge's withheld PAT, an owner-only Azure DevOps grant), and a
// grant those drop is still a stored credential this run's token can name at the
// mint relay. The set must be every row, so no filter belongs here.
//
// Broker off returns nil: that mode mints the PAT into the sandbox on purpose.
func (s *Server) brokeredPATGrantIDs(ctx context.Context, runID uuid.UUID, brokerOn bool) ([]uuid.UUID, error) {
	if !brokerOn {
		return nil, nil
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for _, g := range grants {
		if g.Spec.Kind == types.GrantGitPAT {
			ids = append(ids, g.ID)
		}
	}
	return ids, nil
}
