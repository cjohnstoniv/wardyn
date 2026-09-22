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

// adoGrantsByHost is the ADOGrantSource the gate reads: one entry per covered host.
type adoGrantsByHost map[string]ADOGrant

func (m adoGrantsByHost) ADOGrantFor(host string) (ADOGrant, bool) {
	g, ok := m[strings.ToLower(strings.TrimSuffix(host, "."))]
	return g, ok
}

// newADOGrantSource builds the gate's source from configuration. It returns a
// NIL INTERFACE, not an empty map, when nothing is configured: the gate reads
// nil as "off", and a typed-nil map inside the interface would not be nil.
func newADOGrantSource(grants []ADOGrantConfig) ADOGrantSource {
	m := adoGrantsByHost{}
	for _, g := range grants {
		for _, h := range g.Hosts {
			m[strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))] = ADOGrant{
				Organization: g.Organization,
				Capabilities: append([]adoscope.Capability(nil), g.Capabilities...),
			}
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
