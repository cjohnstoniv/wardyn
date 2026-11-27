// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Names of the configured sources in a namespace census, beside the stored columns store.NamespaceCensusQ
// names ("table.column").
const (
	censusConfiguredDefaultPolicy  = "configured.default_policy"
	censusConfiguredOperatorEmails = "configured.operator_emails"
)

// configuredNamespaceCensus counts, for the portal key issuerKey, every place the configuration this process
// holds names a person under it: a string starting delegate:person:<issuerKey>: , in any letter case. The
// stored census cannot see these, since the default policy and the operator allowlist live in the process and
// not in a table, and a UI-app caller written there is read by the gateway for every run. The result is keyed
// like store.NamespaceCensusQ, counts only and never a value, and holds no key for a source with no reference,
// so an empty map means the configuration is clean.
//
// It reads s.cfg, the same fields the running policy and access paths read, on every call, so a configuration
// change after a proposal is seen when the proposal is approved.
func (s *Server) configuredNamespaceCensus(issuerKey string) map[string]int {
	out := map[string]int{}
	needle := strings.ToLower(types.PortalPersonPrefix + issuerKey + ":")
	if needle == "" || issuerKey == "" {
		return out
	}
	// The whole default policy is read as the JSON the policy path serialises, so a caller named in
	// ui_apps, and any person list a later field adds, is counted without naming each field here.
	if b, err := json.Marshal(s.cfg.DefaultPolicy); err == nil {
		if n := strings.Count(strings.ToLower(string(b)), needle); n > 0 {
			out[censusConfiguredDefaultPolicy] = n
		}
	}
	n := 0
	for _, e := range s.cfg.OperatorEmails {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(e)), needle) {
			n++
		}
	}
	if n > 0 {
		out[censusConfiguredOperatorEmails] = n
	}
	return out
}
