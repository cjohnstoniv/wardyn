// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"strings"
)

// ruleSourceGitPushCapability marks a brokered push refused because the run's
// effective capabilities for that forge leave out code_write
// (Config.GitPushWithheld).
const ruleSourceGitPushCapability = "brokered:git:push-capability"

// canonicalHostKeys lowercases a host-keyed map once, as the brokers match a
// host case-insensitively.
func canonicalHostKeys(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			out[k] = v
		}
	}
	return out
}

// refuseWithheldPush answers a push to a host whose pushes this run's
// capabilities withhold, before anything mints or reaches the forge; true means
// it answered. The ref advertisement a push starts with is refused as well as
// the receive-pack POST: git prints a text/plain refusal of the advertisement
// as remote: lines, and the forge would otherwise answer it with its own 403.
func (p *Proxy) refuseWithheldPush(w http.ResponseWriter, r *http.Request, host, verb string, deny func(ruleSource string)) bool {
	forge, withheld := p.gitPushWithheld[host]
	push := verb == "git-receive-pack" || (verb == patInfoRefs && r.URL.Query().Get("service") == "git-receive-pack")
	if !withheld || !push {
		return false
	}
	deny(ruleSourceGitPushCapability)
	p.denyAttributed(w, "Your walls (or this run's policy) don't include code_write for "+forge+
		", so this run can't push. Change them in Settings, or start a run that allows pushing.", http.StatusForbidden)
	return true
}
