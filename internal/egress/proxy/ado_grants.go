// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
)

// ADOGrantConfig is the run's per-person Azure DevOps grant as dispatch writes
// it into this sidecar's configuration. The sidecar is a separate process, so
// this — not a live call — is how the REST gate learns what to hold requests to.
type ADOGrantConfig struct {
	Organization string                `json:"organization"`
	Capabilities []adoscope.Capability `json:"capabilities"`
	Hosts        []string              `json:"hosts"`
}

// adoGrantsByHost is the run's grant indexed by host; nil == no host gated.
type adoGrantsByHost map[string]ADOGrant

// ADOGrantFor reports ok=false when host is not covered.
func (m adoGrantsByHost) ADOGrantFor(host string) (ADOGrant, bool) {
	g, ok := m[strings.ToLower(strings.TrimSuffix(host, "."))]
	return g, ok
}

// newADOGrantsByHost returns nil when nothing is configured.
func newADOGrantsByHost(g *ADOGrantConfig) adoGrantsByHost {
	if g == nil || len(g.Hosts) == 0 {
		return nil
	}
	m := adoGrantsByHost{}
	for _, h := range g.Hosts {
		m[strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))] = ADOGrant{
			Organization: g.Organization,
			Capabilities: append([]adoscope.Capability(nil), g.Capabilities...),
		}
	}
	return m
}
