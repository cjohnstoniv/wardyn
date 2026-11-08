// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import "slices"

// withoutExpiring drops elevations (memberships with an end) from a governance document: they are granted
// through requests and never written or pruned by a document apply.
func withoutExpiring(doc GovernanceDocument) GovernanceDocument {
	doc.Memberships = slices.DeleteFunc(slices.Clone(doc.Memberships), func(m GovernanceMembership) bool { return m.ExpiresAt != nil })
	return doc
}
