// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// gh_own_pat_run.go is the run-time half of a person's own GitHub token
// (gh_own_pat.go). A row's lanes only veto, so the token is used where the
// run's policy already grants git_pat for github.com: when the row deciding
// that grant (laneRowsForGrantHost) is an own-token row, the grant reads the
// run owner's own sealed token, owner_only, instead of the shared secret it
// names. per_user is "whose credential the lane uses".
//
// IT NEVER ENTERS THE SANDBOX. The broker hands the token only to the run's
// proxy: with the PAT broker on, every git_pat grant id is brokered
// (brokeredPATGrantIDs) and the proxy refuses a raw mint of one from the
// sandbox, and git reaches GitHub through the proxy's /wardyn/git/ route. So
// a launch is refused when the PAT broker is off, or the run is on a person's
// own runner, whose route that is not; and the mint door refuses the grant
// while the broker is off (a run revived after the switch).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The launch refusals. Each names what the run's owner or the admin can do.
const (
	gitHubOwnTokenRequiredRefusal = "This run uses your own GitHub token for github.com, and you have not added one. Add it under Settings, then launch again."
	gitHubOwnTokenExpiredRefusal  = "Your GitHub token has reached the expiry you entered. Add a new token under Settings, then launch again."
	gitHubOwnTokenBrokerOff       = "This run would use your own GitHub token, and this deployment's git broker is off, so the token would be handed into the sandbox. Ask your admin."
	gitHubOwnTokenLocalRefusal    = "Your own GitHub token can't be used on a run on your own runner yet. Run it on the organisation's runners."
	gitHubOwnTokenTwoRows         = "This run's github.com repositories are on two GitHub rows that each take your own token, and one run can use one. Launch them separately."
	gitHubOwnTokenMixedRefusal    = "github.com repositories in this run are admitted by both %s and %s (your own token); one run uses one GitHub credential. Pick repositories from one row."
	gitHubOwnTokenResidentRefusal = "wardyn: this git_pat grant is a person's own GitHub token, and this deployment's git broker is off, so it would be handed into the sandbox. Nothing was minted."
)

// gitHubOwnTokenRefusal is a launch refused over the token, with its reason.
type gitHubOwnTokenRefusal struct{ reason, body string }

func (e *gitHubOwnTokenRefusal) Error() string { return e.body }

// gitHubOwnTokenDecider is the own-token row that decides a git_pat grant for
// host, or ok=false when none does. More than one is a refusal: a run holds
// one git_pat grant per host, so it can carry one person's token there. So is
// an own-token row beside a shared one: the grant is rewritten whole, which
// would swap the shared row's credential for the person's token.
func gitHubOwnTokenDecider(sc types.SiteConfig, host string, repos []string) (types.GitProvider, bool, error) {
	if !hostEqual(host, "github.com") {
		return types.GitProvider{}, false, nil
	}
	var own []types.GitProvider
	var shared []string
	for _, row := range laneRowsForGrantHost(sc, host, repos) {
		switch {
		case row.Disabled:
		case isGitHubOwnTokenRow(row):
			own = append(own, row)
		default:
			shared = append(shared, row.ID)
		}
	}
	switch len(own) {
	case 0:
		return types.GitProvider{}, false, nil
	case 1:
		if len(shared) > 0 {
			return types.GitProvider{}, false, &gitHubOwnTokenRefusal{reasonGitHubOwnTokenUndeliverable,
				fmt.Sprintf(gitHubOwnTokenMixedRefusal, strings.Join(shared, ", "), own[0].ID)}
		}
		return own[0], true, nil
	}
	return types.GitProvider{}, false, &gitHubOwnTokenRefusal{reasonGitHubOwnTokenUndeliverable, gitHubOwnTokenTwoRows}
}

// usableGitHubOwnToken is subject's token on row, or a refusal saying why a
// run cannot have it. A run with no person behind it has none.
func (s *Server) usableGitHubOwnToken(ctx context.Context, subject string, row types.GitProvider) (types.GitHubOwnToken, error) {
	if s.cfg.DisableGitPATBroker {
		return types.GitHubOwnToken{}, &gitHubOwnTokenRefusal{reasonGitHubOwnTokenUndeliverable, gitHubOwnTokenBrokerOff}
	}
	var tok types.GitHubOwnToken
	found := false
	if !operatorOwnedRequest(ctx) {
		var err error
		if tok, found, err = s.readGitHubOwnToken(ctx, subject, row.ID); err != nil {
			return types.GitHubOwnToken{}, err
		}
	}
	switch {
	case !found:
		return types.GitHubOwnToken{}, &gitHubOwnTokenRefusal{reasonGitHubOwnTokenRequired, gitHubOwnTokenRequiredRefusal}
	case tok.Expired(s.cfg.Now()):
		return types.GitHubOwnToken{}, &gitHubOwnTokenRefusal{reasonGitHubOwnTokenExpired, gitHubOwnTokenExpiredRefusal}
	}
	return tok, nil
}

// gitHubOwnTokenNeeds is validateRunSecretRefs' pass for these grants, before
// the run exists: a git_pat grant an own-token row decides does not need the
// secret it names, and, where credentials are graded, needs the launcher's own
// usable token instead.
func (s *Server) gitHubOwnTokenNeeds(ctx context.Context, subject string, needed []neededSecret, spec types.RunPolicySpec, credentials bool) ([]neededSecret, int, error) {
	if s.cfg.Store == nil || !slices.ContainsFunc(needed, func(n neededSecret) bool {
		return n.kind == types.GrantGitPAT && hostEqual(n.host, "github.com")
	}) {
		return needed, 0, nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	kept := needed[:0]
	for _, n := range needed {
		if n.kind != types.GrantGitPAT {
			kept = append(kept, n)
			continue
		}
		row, decided, err := gitHubOwnTokenDecider(sc, n.host, repoLocatorsOf(spec.WorkspaceRepos))
		if err == nil && decided && credentials {
			_, err = s.usableGitHubOwnToken(ctx, subject, row)
		}
		if err != nil {
			return nil, http.StatusUnprocessableEntity, err
		}
		if !decided {
			kept = append(kept, n)
		}
	}
	return kept, 0, nil
}

// ownGitHubTokenGrants returns grants with each git_pat grant an own-token row
// decides rewritten to the run owner's own token, and audits each use. It
// answers the request itself and returns ok=false when the run cannot have
// the token. The slice is a copy, the same length and order as grants.
func (s *Server) ownGitHubTokenGrants(ctx context.Context, w http.ResponseWriter, r *http.Request, runID uuid.UUID,
	grants []types.GrantSpec, sc types.SiteConfig, specRepos []string, placement types.Placement,
) ([]types.GrantSpec, bool) {
	out := slices.Clone(grants)
	subject := runIdentitySubject(ctx, principalFromRequest(r))
	for i, g := range out {
		if g.Kind != types.GrantGitPAT {
			continue
		}
		pat, err := types.DecodeGitPATScope(g.Scope)
		if err != nil {
			continue
		}
		row, decided, err := gitHubOwnTokenDecider(sc, pat.Host, specRepos)
		var tok types.GitHubOwnToken
		switch {
		case err == nil && !decided:
			continue
		case err == nil && placement == types.PlacementLocal:
			err = &gitHubOwnTokenRefusal{reasonGitHubOwnTokenUndeliverable, gitHubOwnTokenLocalRefusal}
		case err == nil:
			tok, err = s.usableGitHubOwnToken(ctx, subject, row)
		}
		var refusal *gitHubOwnTokenRefusal
		if errors.As(err, &refusal) {
			writeErrorReason(w, http.StatusUnprocessableEntity, refusal.reason, refusal.body)
			return nil, false
		}
		if err != nil {
			writeServerError(w, r, "read the GitHub token", err)
			return nil, false
		}
		pat.SecretName = gitHubOwnTokenSecretName(row.ID)
		scope, err := json.Marshal(pat)
		if err != nil {
			writeServerError(w, r, "write the git_pat grant", err)
			return nil, false
		}
		out[i].Scope, out[i].OwnerOnly = scope, true
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorHuman, subject, gitHubPATAuditOwnUse, pat.SecretName, "success",
			mustJSON(map[string]any{"provider_row": row.ID, "host": pat.Host, "expires_on": tok.ExpiresOn.Format(time.DateOnly)})))
	}
	return out, true
}

// refuseResidentGitHubOwnToken refuses a mint of a person's own GitHub token
// while the PAT broker is off: that mode answers a git_pat mint with the
// value, for the sandbox's git. With the broker on the proxy holds it, and
// nothing is read here.
func (s *Server) refuseResidentGitHubOwnToken(w http.ResponseWriter, r *http.Request, runID, grantID uuid.UUID, actor string) bool {
	if !s.cfg.DisableGitPATBroker || s.cfg.Store == nil {
		return false
	}
	grants, err := s.cfg.Store.ListGrantsByRun(r.Context(), runID)
	if err != nil {
		slog.WarnContext(r.Context(), "wardynd: could not list run grants for the own GitHub token check; refusing the mint", slog.Any("err", err))
		writeErrorReason(w, http.StatusServiceUnavailable, reasonGitHubOwnTokenResident, gitHubOwnTokenResidentRefusal)
		return true
	}
	i := slices.IndexFunc(grants, func(g types.CredentialGrant) bool { return g.ID == grantID })
	if i < 0 || grants[i].Spec.Kind != types.GrantGitPAT {
		return false
	}
	if pat, err := types.DecodeGitPATScope(grants[i].Spec.Scope); err != nil || !types.IsGitHubOwnTokenSecret(pat.SecretName) {
		return false
	}
	s.recordAudit(r.Context(), s.auditEvent(&runID, types.ActorAgent, actor, "credential.mint", grantID.String(), "denied",
		mustJSON(map[string]any{"grant_id": grantID.String(), "kind": string(types.GrantGitPAT), "reason": reasonGitHubOwnTokenResident})))
	writeErrorReason(w, http.StatusForbidden, reasonGitHubOwnTokenResident, gitHubOwnTokenResidentRefusal)
	return true
}
