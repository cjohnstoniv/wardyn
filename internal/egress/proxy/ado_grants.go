// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
)

// ADOGrantConfig is the run's per-person Azure DevOps grant as dispatch writes
// it into this sidecar's configuration: the organisation the provider row pins,
// the capabilities the run was granted, and the EXACT hosts (bare, lower-case)
// the grant covers. The sidecar is a separate process, so this — not a live
// call — is how the REST gate (ado_gate.go) learns what to hold each request to.
type ADOGrantConfig struct {
	Organization string                `json:"organization"`
	Capabilities []adoscope.Capability `json:"capabilities"`
	Hosts        []string              `json:"hosts"`
}

// adoGrantsByHost is what the gate reads: the run's one grant, under each host
// it covers. Nil == no host gated.
type adoGrantsByHost map[string]ADOGrant

// ADOGrantFor answers the run's grant for host; ok=false means the host is not
// covered and the gate stands aside.
func (m adoGrantsByHost) ADOGrantFor(host string) (ADOGrant, bool) {
	g, ok := m[strings.ToLower(strings.TrimSuffix(host, "."))]
	return g, ok
}

// newADOGrantsByHost indexes the configured grant by host. It returns nil when
// nothing is configured, which the gate reads as "off".
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
