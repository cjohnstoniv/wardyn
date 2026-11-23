// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// grantResidencyGitHubScope is the part of a github_token grant's scope that
// names its repositories (the same "repos" key the broker reads).
type grantResidencyGitHubScope struct {
	Repos []string `json:"repos"`
}

// grantResidency is whether g's credential value, or a credential the sandbox can turn it into, is
// inside the sandbox; known is false when this cannot be said, and the wire then omits
// inside_sandbox. sigv4AtProxy is the server-wide switch that lets the proxy re-sign an AWS
// sign-in path (Server.gatewayBedrockSSOAvailable, the switches awsSigV4Lane reads first), not a
// per-run fact.
func grantResidency(g types.GrantSpec, brokerOn bool, brokeredRepos []string, sigv4AtProxy bool) (inside, known bool) {
	switch g.Kind {
	case types.GrantAPIKey:
		return grantResidencyAPIKey(g.Scope, sigv4AtProxy)
	case types.GrantSSHKey:
		// Signed by the proxy's ssh agent: the value stays out.
		return false, true
	case types.GrantGitPAT, types.GrantEnvSecret, types.GrantFileSecret:
		// The git credential helper, the environment or a file inside the sandbox holds it.
		return true, true
	case types.GrantGitHubToken:
		if brokerOn && grantResidencyAllBrokered(g.Scope, brokeredRepos) {
			return false, true
		}
		return true, true
	case types.GrantCloudSTS:
		// Resident unless the proxy signs with it, and sigv4AtProxy says only that this server
		// can re-sign, not that this run was re-signed.
		if sigv4AtProxy {
			return false, false
		}
		return true, true
	default:
		// Never claim "not inside" for a kind this table does not know.
		return false, false
	}
}

// grantResidencyAzureCLIScope is the part of an api_key grant's scope that names the Azure CLI
// path (azureARMGrantSnapshot's mechanism) and the access its row grants.
type grantResidencyAzureCLIScope struct {
	Snapshot struct {
		Mechanism string `json:"mechanism"`
		Access    string `json:"access"`
	} `json:"snapshot"`
}

// grantResidencyAPIKey is grantResidency for an api_key grant, which the proxy injects on the wire.
// The paths whose sandbox can trade that value for a credential it then holds never answer "not
// inside":
//   - The AWS sign-in session (its secret is types.AWSSSOAccessTokenSecret). Re-signing off, the
//     sandbox's SDK redeems it for role credentials that stay resident. On, a path whose hosts the
//     proxy cannot all re-sign keeps that flow, and the grant row does not say which one this run
//     took, so the answer is unknown.
//   - The Azure CLI with write access: armscope.Scope.Write admits the POSTs that return keys
//     (listKeys and the like), and the grant row does not say whether this run made one, so the
//     answer is unknown. With read access every POST is refused.
//
// The other api_key paths keep the value at the proxy. Where a door fronts a service that could
// mint a credential from it, the door refuses those calls: kubectl (kubeAPIDoor: Secrets, service
// account token requests, exec) and the Azure DevOps sign-in (adoscope's token areas).
func grantResidencyAPIKey(scope json.RawMessage, sigv4AtProxy bool) (inside, known bool) {
	if apiKeyGrantScopeSecret(scope) == types.AWSSSOAccessTokenSecret {
		if sigv4AtProxy {
			return false, false
		}
		return true, true
	}
	var az grantResidencyAzureCLIScope
	if json.Unmarshal(scope, &az) == nil && az.Snapshot.Mechanism == azureCLIMechanism &&
		az.Snapshot.Access == types.ComponentAccessWrite {
		return false, false
	}
	return false, true
}

// grantResidencyAllBrokered is true when the scope names at least one repository
// and every one of them is in brokered. A scope that cannot be read, or names
// none, is not brokered.
func grantResidencyAllBrokered(scope json.RawMessage, brokered []string) bool {
	var sc grantResidencyGitHubScope
	if err := json.Unmarshal(scope, &sc); err != nil || len(sc.Repos) == 0 {
		return false
	}
	have := make(map[string]struct{}, len(brokered))
	for _, r := range brokered {
		have[r] = struct{}{}
	}
	for _, r := range sc.Repos {
		if _, ok := have[r]; !ok {
			return false
		}
	}
	return true
}
