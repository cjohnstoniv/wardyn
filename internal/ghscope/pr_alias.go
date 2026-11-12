// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package ghscope

import "slices"

// prParts is what CapPR stands for.
var prParts = []Capability{CapPRWrite, CapPRReview}

// ExpandPR is a new slice holding caps with CapPR replaced by its two parts, in caps' order, without
// duplicates. It never returns or changes caps itself. nil stays nil and an empty list stays empty
// (a new empty slice).
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

// Has reports whether caps holds c, reading CapPR as its two parts and the two parts together as CapPR.
func Has(caps []Capability, c Capability) bool {
	expanded := ExpandPR(caps)
	if c == CapPR {
		return !slices.ContainsFunc(prParts, func(p Capability) bool { return !slices.Contains(expanded, p) })
	}
	return slices.Contains(expanded, c)
}
