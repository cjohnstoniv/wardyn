// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"regexp"
	"strings"
)

// SecretNameRE is the shape of a stored secret's name: a safe, predictable
// identifier, so a name that could never be stored is refused where it is
// typed rather than found missing at dispatch.
//
// ponytail: internal/api still carries its own secretNameRE and
// validInjectionFormat with these same rules; point both at this file and
// delete the copies.
var SecretNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,126}[a-z0-9])?$`)

// ValidInjectionFormat is the rule for the value template of a
// proxy-injected credential header. The secret is substituted for its one
// %s, so a template with none sends a bare prefix and drops the credential,
// one with two renders a formatting error onto the wire, and a line break is
// a header-splitting shape. Empty is valid: the caller chooses the default.
func ValidInjectionFormat(format string) error {
	if format == "" {
		return nil
	}
	if strings.Count(format, "%s") != 1 || strings.Count(format, "%") != 1 {
		return fmt.Errorf("format: %q must contain exactly one %%s (where the secret goes) and no other verb", format)
	}
	if strings.ContainsAny(format, "\r\n") {
		return fmt.Errorf("format: must not contain a line break")
	}
	return nil
}
