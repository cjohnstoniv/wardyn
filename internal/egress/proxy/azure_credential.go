// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// What a sandbox reads when the Entra token for an azure_foundry host could not be attached: a sign-in
// hold that ended without a sign-in, or any other failure of the resolve. Always 403, never 401, which
// a model client reads as "try another credential" and answers with its own key or a retry storm.
//
// DRAFT (M2 canon pending)
const (
	azureSignInTimedOutRefusal = "Wardyn held this Azure request while its owner was asked to sign in to Azure again, " +
		"and nobody signed in before the hold expired. Nothing was substituted; retry once they have signed in."
	azureSignInEndedRefusal = "Wardyn asked this Azure request's owner to sign in to Azure again, and the request " +
		"ended before a sign-in arrived. Nothing was substituted."
	azureCredentialFailedRefusal = "Wardyn could not renew this run's Azure credential."

	azureCredentialUnavailable = "azure_credential_unavailable"
)

// refuseAzureCredential answers a request to a gated Azure host whose credential resolve failed, as
// refuseAzure does for a gate refusal: one decision row under brokered:azure:denied (the hold's expiry
// has no row of its own here, so it never reaches the AWS re-auth timeout series) and a JSON body
// carrying the code. A control-plane refusal keeps its own sentence.
func (p *Proxy) refuseAzureCredential(w http.ResponseWriter, r *http.Request, host string, port int, err error) {
	msg := adoControlPlaneRefusal(err, azureCredentialFailedRefusal)
	switch {
	case errors.Is(err, errReauthTimedOut):
		msg = azureSignInTimedOutRefusal
	case errors.Is(err, errReauthNoCredential):
		msg = azureSignInEndedRefusal
	}
	slog.Warn("proxy: an Azure credential could not be resolved", "host", host, "err", reauthHoldError(err))
	if p.sink != nil {
		p.sink.emit(decisionLog(p.reqOf(r, host, port), egress.Deny, ruleSourceAzureDenied))
	}
	writeAzureJSON(w, http.StatusForbidden, azureCredentialUnavailable, msg)
}
