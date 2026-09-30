// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The retired Bedrock BEARER resolve. A run's Bedrock API key is now its model
// provider's — the run owner's own wardyn-provider-<uid>-key, resolved by
// resolveProviderKeyInjection — and dispatch authors no grant naming
// bedrock-api-key (dropLegacyModelInjections strips any other author's). The
// generic sink's read, Store.For(sub).Get, falls back to the operator's row, so
// a grant naming the name must still never reach it: this arm refuses every
// one.

// bedrockBearerNotRecorded refuses a grant naming bedrock-api-key.
//
// DRAFT (M2 canon pending)
const bedrockBearerNotRecorded = "bedrock-api-key is no longer a model credential: a run's Bedrock key comes from its " +
	"model provider, so this grant is not served"

// resolveBedrockBearerInjection is the bedrock-api-key arm of
// handleInternalInjection. handled=false means the grant names another secret
// and the generic path must run; handled=true means the refusal was written.
func (s *Server) resolveBedrockBearerInjection(w http.ResponseWriter, r *http.Request,
	claims *identity.Claims, minted broker.Minted, grantID uuid.UUID,
) bool {
	if minted.Injection.SecretName != bedrockAPIKeySecret {
		return false
	}
	s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", bedrockAPIKeySecret, "failure",
		mustJSON(map[string]any{"reason": reasonMissingScopeSnapshot, "grant_id": grantID})))
	writeErrorReason(w, http.StatusForbidden, reasonMissingScopeSnapshot, bedrockBearerNotRecorded)
	return true
}
