// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import "slices"

// prParts is what CapPR stands for.
var prParts = []Capability{CapPRCreate, CapPRVote, CapPRComplete}

// ExpandPR is a new slice holding caps with CapPR replaced by its three parts, in caps' order, without
// duplicates. It never returns or changes caps itself. nil stays nil and an empty list stays empty
// (a new empty slice: an empty list means the provider's default profile).
func ExpandPR(caps []Capability) []Capability {
	if caps == nil {
		return nil
	}
	out := make([]Capability, 0, len(caps)+len(prParts))
	for _, c := range caps {
		add := []Capability{c}
		if c == CapPR {
			add = prParts
		}
		for _, a := range add {
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
}

// Has reports whether caps holds c, reading CapPR as its three parts and the three parts together as CapPR.
func Has(caps []Capability, c Capability) bool {
	expanded := ExpandPR(caps)
	if c == CapPR {
		return !slices.ContainsFunc(prParts, func(p Capability) bool { return !slices.Contains(expanded, p) })
	}
	return slices.Contains(expanded, c)
}

// HasName is Has over wire names (a resolve answer's capabilities).
func HasName(names []string, c Capability) bool {
	caps := make([]Capability, len(names))
	for i, n := range names {
		caps[i] = Capability(n)
	}
	return Has(caps, c)
}
