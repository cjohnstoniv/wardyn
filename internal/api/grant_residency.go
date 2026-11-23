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

// grantResidency is whether g's credential value is inside the sandbox; known is false when this
// cannot be said (an unknown kind), and the wire then omits inside_sandbox.
func grantResidency(g types.GrantSpec, brokerOn bool, brokeredRepos []string, sigv4AtProxy bool) (inside, known bool) {
	switch g.Kind {
	case types.GrantAPIKey, types.GrantSSHKey:
		// Injected on the wire, or signed by the proxy's ssh agent: the value stays out.
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
		if sigv4AtProxy {
			return false, true
		}
		return true, true
	default:
		// Never claim "not inside" for a kind this table does not know.
		return false, false
	}
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
