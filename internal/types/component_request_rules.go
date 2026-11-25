// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"fmt"
	"strings"
)

// validateRequestRules is request_rules' shape: one host rule set in the host-rule grammar (at most
// MaxHostRulesPerSet rules, each in normal form), and every rule on a host the definition itself names
// exactly, never only through a "*." entry, on port 443 or on any port. The proxy opens and judges every
// request to a rule host, and HTTPS to it goes through port 443 only, so a host named only on another
// port could never be reached under its rules. No rules is valid.
func validateRequestRules(d ComponentDefinition) error {
	if len(d.RequestRules) == 0 {
		return nil
	}
	if len(d.RequestRules) > MaxHostRulesPerSet {
		return fmt.Errorf("request_rules: at most %d rules, got %d", MaxHostRulesPerSet, len(d.RequestRules))
	}
	for i, r := range d.RequestRules {
		if err := validHostRule(r); err != nil {
			return fmt.Errorf("request_rules[%d].%w", i, err)
		}
	}
	// The one set also meets every per-set check of host_rules, so a check added there reaches component saves too.
	if err := ValidateHostRuleSets([]HostRuleSet{{Rules: d.RequestRules}}); err != nil {
		return errors.New(strings.Replace(err.Error(), "host_rules[0].rules", "request_rules", 1))
	}
	exact := map[string]bool{}
	for _, h := range d.Hosts {
		dest, err := ParseDestination(h)
		if err != nil || dest.Wildcard || dest.Port != 0 && dest.Port != 443 {
			continue
		}
		exact[dest.Host] = true
	}
	for i, r := range d.RequestRules {
		if !exact[r.Host] {
			return fmt.Errorf("request_rules[%d].host: %q is not one of the component's hosts named exactly, with no port or port 443", i, r.Host)
		}
	}
	return nil
}
