// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/url"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// cloneTarget is a clone URL reduced to the three facts the match rule compares.
// SSH is its own shape because an SSH clone URL carries no comparable path
// (git@ssh.dev.azure.com:v3/org/... is not /org/...), so org-path scoping is
// host-level only for SSH in v1 — a documented ceiling, not an oversight.
type cloneTarget struct {
	scheme string
	host   string
	path   string
	ssh    bool
}

// parseCloneTarget reduces a DERIVED clone URL (repoCloneURL's output, never a
// raw slug) to a cloneTarget. ok=false for anything it cannot read — including
// the empty string repoCloneURL returns for a transport that already failed
// closed — so an unreadable target is REFUSED rather than admitted by accident.
//
// The port is deliberately not part of the comparison: hostrules.HostOf discards
// it and so does the egress allowlist, so treating it as a scoping boundary here
// would claim a narrowing the rest of the system does not implement.
func parseCloneTarget(cloneURL string, adoServerHosts []string) (cloneTarget, bool) {
	raw := strings.TrimSpace(cloneURL)
	if raw == "" {
		return cloneTarget{}, false
	}
	// The traversal guard, before either form is read: a path the server and the
	// sandbox's git read differently is not a target this function can reduce
	// honestly, so it is UNREADABLE and therefore refused — with the same
	// operator/member sentences every other unreadable target earns. See
	// repoLocatorPathSafe (runs_scm.go) for why no spelling of pathAdmits
	// survives one.
	if !repoLocatorPathSafe(raw, adoServerHosts) {
		return cloneTarget{}, false
	}
	// sshCloneHost answers only for ssh:// and scp-form strings (it refuses
	// anything else carrying a scheme), so an https clone URL falls through.
	if host, ok := sshCloneHost(raw); ok {
		return cloneTarget{scheme: "ssh", host: host, ssh: true}, true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return cloneTarget{}, false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return cloneTarget{}, false
	}
	return cloneTarget{
		scheme: scheme,
		host:   strings.ToLower(u.Hostname()),
		path:   strings.TrimSuffix(u.Path, "/"),
	}, true
}

// canonicalProviderHost folds the ssh.<host> alias onto its primary so one
// stored base URL admits (and CLAIMS) both clone forms — the same fold
// canonicalSSHKeySecret applies to the secret name.
func canonicalProviderHost(host string) string {
	return strings.TrimPrefix(host, "ssh.")
}

// hostsMatch compares a clone target's host with a provider base URL's host.
// For an SSH target the ssh.<host> alias matches in either direction; for an
// https target the host must be equal (case already folded by both sides).
func hostsMatch(t cloneTarget, baseHost string) bool {
	if t.host == baseHost {
		return true
	}
	return t.ssh && canonicalProviderHost(t.host) == canonicalProviderHost(baseHost)
}

// pathAdmits reports whether a clone path is inside a base URL's path: equal, or
// extending it AT A "/" BOUNDARY. The boundary is the whole point — without it a
// base URL of /acme would admit /acme-evil/repo.
//
// Case handling is the CALLER's (rowAdmits, via foldsPathCase): github.com
// treats /Acme and /acme as one org, so both sides are folded there; every other
// host is compared as written, which refuses a case the admin did not name
// rather than admitting one they did not.
func pathAdmits(clonePath, basePath string) bool {
	if basePath == "" || basePath == "/" {
		return true
	}
	return clonePath == basePath || strings.HasPrefix(clonePath, basePath+"/")
}

// rowAdmits reports whether one provider row's base URLs admit a clone target.
func rowAdmits(row types.GitProvider, t cloneTarget) bool {
	for _, raw := range row.BaseURLs {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		baseHost := strings.ToLower(u.Hostname())
		if !hostsMatch(t, baseHost) {
			continue
		}
		if t.ssh {
			return true // host-level only for SSH, see cloneTarget
		}
		if strings.ToLower(u.Scheme) != t.scheme {
			continue
		}
		clonePath, basePath := t.path, strings.TrimSuffix(u.Path, "/")
		if foldsPathCase(baseHost) {
			clonePath, basePath = strings.ToLower(clonePath), strings.ToLower(basePath)
		}
		if pathAdmits(clonePath, basePath) {
			return true
		}
	}
	return false
}

// sshAdmittedAbovePath reports whether a row admitted an SSH target ONLY because
// SSH scoping is host-level: the target is SSH and every base URL of this row
// that matches its host carries an org path. That is the documented ceiling
// (see cloneTarget) and it is REAL — `https://github.com/acme` admits
// `git@github.com:other-org/x.git` — so the callers say it out loud rather than
// letting a path-scoped row look narrower than it is.
func sshAdmittedAbovePath(row types.GitProvider, t cloneTarget) bool {
	if !t.ssh {
		return false
	}
	for _, raw := range row.BaseURLs {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		if hostsMatch(t, strings.ToLower(u.Hostname())) && strings.Trim(u.Path, "/") == "" {
			return false // this row bounds the whole host anyway; nothing was widened
		}
	}
	return rowAdmits(row, t)
}

// rowClaimsHost reports whether one row CLAIMS a host — the precedence question,
// which is separate from whether it admits a given path.
//
// By HOST, and kind-wide on the two well-known hosts, because a kind cannot be
// derived for a self-hosted forge. Without this, the documented MDM content
// scm_hosts: ["github.com"] would admit github.com/other/* past a provider
// limited to https://github.com/acme, making org-path scoping vacuous on exactly
// the fleets this feature is for.
func rowClaimsHost(row types.GitProvider, t cloneTarget) bool {
	host := canonicalProviderHost(t.host)
	switch row.Kind {
	case types.GitProviderGitHub:
		if host == "github.com" {
			return true
		}
	case types.GitProviderAzureDevOps:
		if adoEgressDomains(host) != nil {
			return true
		}
	}
	for _, raw := range row.BaseURLs {
		if h := hostrules.HostOf(raw); h != "" && hostsMatch(t, h) {
			return true
		}
	}
	return false
}
