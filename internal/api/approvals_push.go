// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The control-plane half of a held git push (push_rules.require_review_paths).
//
// The proxy sidecar does the holding (internal/egress/proxy/push_hold.go) and
// raises a push_content approval through handleInternalRequestApproval. What
// lives here is what the control plane checks before that row exists:
//
//   - THE SHAPE. The scope is what the console renders on the push card, so a
//     row is only written when it is one (types.PushContentScope.Validate).
//   - WHO IT ACTS AS. The sidecar names the credential (acts_as, a grant
//     ref); the control plane resolves it against the run's own grants and
//     stamps acts_as_kind and acts_as_label, the principal the console shows.
//     A raise that names a grant this run does not hold, or that carries
//     either stamp itself, is refused.
//   - THE RUN IS ATTENDED. An unattended run's sidecar refuses a review-path
//     push outright rather than raising (brokered:git:push-held-unattended);
//     refusing here too means a sidecar that did not, for whatever reason,
//     still leaves no question in front of a human that nobody's run is
//     waiting on.
//
// Who may DECIDE one is authorizeMemberDecision's, unchanged: members are kept
// to egress_domain (and their own Azure DevOps escalations), so push_content
// is admin-decidable only. A member approving their own run's workflow-file
// edit is the exfiltration the rule exists to stop. A decision carries no
// decision_scope (decide's rule 4 refuses one on this kind): the sidecar
// treats an approval as covering exactly the commits the scope names.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pushContentUnattendedBody is the refusal a raise for an unattended run gets.
const pushContentUnattendedBody = "this run is unattended, so a push that needs review is refused rather than held"

// admitPushContentRaise is handleInternalRequestApproval's push_content arm.
// It returns the scope to store — the sidecar's, with acts_as_kind and
// acts_as_label stamped — or writes its own 4xx and reports false.
func (s *Server) admitPushContentRaise(w http.ResponseWriter, r *http.Request, claims *identity.Claims,
	raw json.RawMessage) (json.RawMessage, bool) {
	var scope types.PushContentScope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&scope); err != nil {
		writeError(w, http.StatusBadRequest, "invalid push_content requested_scope: "+err.Error())
		return nil, false
	}
	if err := scope.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid push_content requested_scope: "+err.Error())
		return nil, false
	}
	if scope.ActsAsKind != "" || scope.ActsAsLabel != "" {
		writeError(w, http.StatusBadRequest, "invalid push_content requested_scope: acts_as_kind and acts_as_label are set by the control plane")
		return nil, false
	}
	if s.cfg.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "run store unavailable")
		return nil, false
	}
	run, err := s.cfg.Store.GetRun(r.Context(), claims.RunID)
	if err != nil {
		writeServerError(w, r, "load run for push_content", err)
		return nil, false
	}
	if !run.Interactive {
		writeError(w, http.StatusForbidden, pushContentUnattendedBody)
		return nil, false
	}
	grants, err := s.cfg.Store.ListGrantsByRun(r.Context(), run.ID)
	if err != nil {
		writeServerError(w, r, "list run grants for push_content", err)
		return nil, false
	}
	if scope.ActsAsKind, scope.ActsAsLabel, err = s.pushActsAs(r.Context(), run, claims.Sub, grants, scope.ActsAs); err != nil {
		writeError(w, http.StatusBadRequest, "invalid push_content requested_scope: "+err.Error())
		return nil, false
	}
	out, err := json.Marshal(scope)
	if err != nil {
		writeServerError(w, r, "encode push_content scope", err)
		return nil, false
	}
	return out, true
}

// pushActsAs resolves acts_as ("<grant kind>:<grant id>") against the run's
// own grants: which lane's credential the push uses, and the principal it
// belongs to.
//
//   - github_token: the GitHub App installation. It belongs to nobody, so the
//     label is the run's owner, on whose behalf the push is made.
//   - git_pat: the run owner when the secret it names is in their own
//     namespace (the one the broker reads first, internal/broker's ownerOf),
//     otherwise the operator's shared secret, labelled PushActsAsOperator.
//   - the Azure DevOps Entra lane's api_key grant (the one whose secret is the
//     types.ADOEntraAccessTokenSecret sentinel): the person's own bearer,
//     resolved for the run's owner, so the label is the run owner.
//
// ponytail: labels are principals as runs record them (AgentRun.CreatedBy);
// resolve to a display name when a people directory exists to ask.
func (s *Server) pushActsAs(ctx context.Context, run types.AgentRun, subject string,
	grants []types.CredentialGrant, actsAs string) (kind, label string, err error) {
	gk, rawID, _ := strings.Cut(actsAs, ":")
	id, perr := uuid.Parse(rawID)
	i := slices.IndexFunc(grants, func(g types.CredentialGrant) bool { return g.ID == id })
	if perr != nil || i < 0 || string(grants[i].Spec.Kind) != gk {
		return "", "", fmt.Errorf("acts_as %q names no %s grant of this run", actsAs, gk)
	}
	switch grants[i].Spec.Kind {
	case types.GrantGitHubToken:
		return types.PushActsAsGitHubApp, run.CreatedBy, nil
	case types.GrantGitPAT:
		_, secret, _, serr := gitPATScopeFields(grants[i].Spec.Scope)
		if serr != nil {
			return "", "", fmt.Errorf("acts_as %q: %w", actsAs, serr)
		}
		if subject != "" && s.cfg.Secrets != nil {
			if names, lerr := s.cfg.Secrets.For(subject).List(ctx); lerr == nil && slices.Contains(names, secret) {
				return types.PushActsAsGitPAT, run.CreatedBy, nil
			}
		}
		return types.PushActsAsGitPAT, types.PushActsAsOperator, nil
	case types.GrantAPIKey:
		if rule, rerr := injectionRuleFromScope(grants[i].Spec.Scope); rerr == nil && rule.SecretName == types.ADOEntraAccessTokenSecret {
			return types.PushActsAsADOEntra, run.CreatedBy, nil
		}
	}
	return "", "", fmt.Errorf("acts_as %q is not a credential a push authenticates with", actsAs)
}
