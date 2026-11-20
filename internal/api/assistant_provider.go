// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "github.com/cjohnstoniv/wardyn/internal/types"

// inspectionDeferredFor reports whether p leaves inspection of its prompts to its gateway. It is the one
// definition: a Bedrock call is scanned by the generic forward check, which a route never defers, and a
// provider with no gateway identity is always inspected by Wardyn.
func inspectionDeferredFor(p types.ModelProvider) bool {
	return p.GatewayIdentity != nil && p.GatewayIdentity.Inspection == types.GatewayInspectionGateway && !p.Kind.IsBedrock()
}
