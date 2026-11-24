// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package ghscope

import (
	"maps"
	"slices"
)

// Catalogue is every Capability value that Valid admits: the grantable set, the
// refused set, metadata, and the two unclassified answers. It is sorted and a
// new slice each call, so a caller can walk the whole set without naming a
// constant, and a test can hold a table to it.
func Catalogue() []Capability {
	out := slices.Concat(
		slices.Collect(maps.Keys(grantableCapabilities)),
		slices.Collect(maps.Keys(deniedCapabilities)),
		[]Capability{CapMetadata, CapUnclassifiedWrite, CapUnclassifiedRead},
	)
	slices.Sort(out)
	return slices.Compact(out)
}
