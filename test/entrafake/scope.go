// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package entrafake

import (
	"slices"
	"strings"
)

// Resource and `.default` scope handling: one token is for one resource, and a
// `<resource>/.default` request is answered with the permissions it expands to.

// multipleResourcesDescription is the AADSTS700022 refusal text.
const multipleResourcesDescription = AADSTSMultipleResources +
	": the provided value for the input parameter 'scope' is not valid because it contains more than one resource"

// resourceOf is the resource a scope names: everything before its last "/". A
// bare scope (the OIDC scopes, or a name with no resource prefix) names none.
func resourceOf(scope string) string {
	if slices.Contains(alwaysConsented, scope) {
		return ""
	}
	if i := strings.LastIndex(scope, "/"); i > 0 {
		return scope[:i]
	}
	return ""
}

// multipleResources reports whether requested names scopes of more than one
// resource, which Entra refuses with AADSTS700022.
func multipleResources(requested []string) bool {
	seen := ""
	for _, sc := range requested {
		res := resourceOf(sc)
		switch {
		case res == "":
		case seen == "":
			seen = res
		case res != seen:
			return true
		}
	}
	return false
}

// isDefaultScope reports whether scope is a `<resource>/.default` request.
func isDefaultScope(scope string) bool { return strings.HasSuffix(scope, "/.default") }

// expandDefault expands every `<resource>/.default` in requested into the
// consented scopes under `<resource>/`, as a real authority does: the answer
// lists those permissions and never the `.default` literal. A `.default`
// request is consented iff the fixture holds a scope under its resource (a
// `.default` entry in the fixture is not one); bad names the request that is
// not. Every other requested scope passes through unchanged.
func expandDefault(requested, consented []string) (expanded []string, bad string, ok bool) {
	for _, want := range requested {
		if !isDefaultScope(want) {
			if !slices.Contains(expanded, want) {
				expanded = append(expanded, want)
			}
			continue
		}
		prefix := strings.TrimSuffix(want, "/.default") + "/"
		held := false
		for _, have := range consented {
			if strings.HasPrefix(have, prefix) && !isDefaultScope(have) {
				held = true
				if !slices.Contains(expanded, have) {
					expanded = append(expanded, have)
				}
			}
		}
		if !held {
			return nil, want, false
		}
	}
	return expanded, "", true
}
