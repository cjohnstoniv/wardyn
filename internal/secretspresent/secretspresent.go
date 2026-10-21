// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package secretspresent is the "secrets present" rule a snapshot or fork of a
// sandbox must pass (accepted 2026-10-19): a sandbox holds secrets when any
// grant it holds is resident (env or file secrets, ssh_key, AWS role
// credentials, a component's env or file), when a "Needs you" takeover
// happened in it, or when its per-session sign-in store is not empty. Any one
// refuses the snapshot. 0.10.0 ships the rule; snapshots and forks are 0.10.x.
package secretspresent

// Reason names one cause of secrets being present.
type Reason string

const (
	// ResidentGrant: a credential lives inside the sandbox for the run.
	ResidentGrant Reason = "resident_grant"
	// Takeover: a person took over the sandbox ("Needs you").
	Takeover Reason = "takeover"
	// SigninStore: a desktop sign-in began, so the person's store may hold
	// logins (a begun sign-in counts: the sandbox cannot be trusted to say
	// its store is empty).
	SigninStore Reason = "signin_store"
	// Unknown: a fact could not be read; the rule fails closed.
	Unknown Reason = "unknown"
)

// Inputs are the facts the rule reads.
type Inputs struct {
	// ResidentGrants counts the run's resident grants (env_secret,
	// file_secret, ssh_key, AWS role credentials, component env or file).
	ResidentGrants int
	// TakeoverHappened is whether any "Needs you" takeover was taken.
	TakeoverHappened bool
	// SigninBegun is whether any desktop sign-in began.
	SigninBegun bool
	// Unreadable is set when any of the above could not be read.
	Unreadable bool
}

// Evaluate reports whether secrets are present and why, in a fixed order.
func Evaluate(in Inputs) (bool, []Reason) {
	var out []Reason
	if in.Unreadable {
		out = append(out, Unknown)
	}
	if in.ResidentGrants > 0 {
		out = append(out, ResidentGrant)
	}
	if in.TakeoverHappened {
		out = append(out, Takeover)
	}
	if in.SigninBegun {
		out = append(out, SigninStore)
	}
	return len(out) > 0, out
}
