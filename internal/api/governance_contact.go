// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// parseProfileContact reads a profile write's raw `contact` member, which has
// three states an older client makes distinct: absent (set=false: keep the stored
// value, so an SDK or console that predates the field cannot wipe it), null
// (set=true, nil: clear) and an object (set=true; one with every field empty is
// stored as none). A bad value is a message for the 400.
func parseProfileContact(raw json.RawMessage) (c *policyref.Contact, set bool, msg string) {
	if len(raw) == 0 {
		return nil, false, ""
	}
	var v *policyref.Contact
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return nil, false, "invalid contact: " + err.Error()
	}
	if v == nil || v.IsZero() {
		return nil, true, ""
	}
	if err := policyref.Validate(*v); err != nil {
		return nil, false, strings.TrimPrefix(err.Error(), "contact.")
	}
	return v, true, ""
}

// contactAudit is what an audit row records about a contact: which fields are set
// and the link members are sent to, the one value a reviewer must see change. The
// owner and email are a person's details and stay out of the append-only log.
func contactAudit(c *policyref.Contact) (fields []string, requestURL string) {
	if c == nil {
		return []string{}, ""
	}
	return c.Fields(), c.RequestURL
}

// validatePolicyHelp is the site config's write check for policy_help: the same
// rules as a profile's contact, named for the key the 400 is about.
func validatePolicyHelp(c *policyref.Contact) error {
	if c == nil {
		return nil
	}
	if err := policyref.Validate(*c); err != nil {
		return fmt.Errorf("policy_help.%s", strings.TrimPrefix(err.Error(), "contact."))
	}
	return nil
}

// carryForwardPolicyHelp keeps the stored policy_help when the body did not
// mention it, for the MDM re-apply reason the sign-in help pair states, and stores
// an all-empty block as none.
func carryForwardPolicyHelp(cfg *types.SiteConfig, existing types.SiteConfig, present map[string]bool) {
	if !present["policy_help"] {
		cfg.PolicyHelp = existing.PolicyHelp
	} else if cfg.PolicyHelp != nil && cfg.PolicyHelp.IsZero() {
		cfg.PolicyHelp = nil
	}
}

// auditPolicyHelp adds the policy_help fields to a site_config.write datum: which
// fields are set and the request link, never the owner or email, which are a
// person's. Only once a block is stored or the body named one, so a deployment
// with none writes the row it always wrote.
func auditPolicyHelp(datum map[string]any, c *policyref.Contact, named bool) {
	if c != nil || named {
		datum["policy_help_fields"], datum["policy_help_request_url"] = contactAudit(c)
	}
}
