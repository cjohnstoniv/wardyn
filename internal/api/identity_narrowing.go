// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"os"
)

// envIdentityNarrowing switches two narrowings by the identities behind a request (#4073): a unit or group
// decider's own egress_host grant on an egress_domain approve, and restrictions keyed on every user type a
// person's sign-in matches. audit records what enforce would refuse or narrow and changes nothing; enforce
// applies it. Env-only, like WARDYN_APPROVAL_RULES: a setting held in the database could be switched off by
// the administrator it constrains.
const envIdentityNarrowing = "WARDYN_IDENTITY_NARROWING"

const (
	identityNarrowingAudit   = "audit"
	identityNarrowingEnforce = "enforce"
)

// identityNarrowingDefault is the mode when the variable is unset.
const identityNarrowingDefault = identityNarrowingAudit

// validateIdentityNarrowingSwitch is the boot check (ValidateEnvToggles): exactly audit or enforce, or unset.
func validateIdentityNarrowingSwitch() error {
	switch v := os.Getenv(envIdentityNarrowing); v {
	case "", identityNarrowingAudit, identityNarrowingEnforce:
		return nil
	default:
		return fmt.Errorf("invalid %s=%q: want audit or enforce", envIdentityNarrowing, v)
	}
}

// identityNarrowingEnforced is the switch as a request reads it. Boot refused any other value, so one read here
// is the default.
func identityNarrowingEnforced() bool {
	v := os.Getenv(envIdentityNarrowing)
	if validateIdentityNarrowingSwitch() != nil || v == "" {
		v = identityNarrowingDefault
	}
	return v == identityNarrowingEnforce
}

// identityNarrowingMode is the mode an alert row names.
func identityNarrowingMode() string {
	if identityNarrowingEnforced() {
		return identityNarrowingEnforce
	}
	return identityNarrowingAudit
}
