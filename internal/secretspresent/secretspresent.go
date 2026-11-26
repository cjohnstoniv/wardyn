// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package secretspresent is the "secrets present" rule a snapshot or fork of a
// sandbox must pass (accepted 2026-10-19): a sandbox holds secrets when any
// grant it holds is resident (env or file secrets, ssh_key, AWS role
// credentials, a component's env or file), when its git token lives in the
// sandbox, when a "Needs you" takeover happened in it, when its per-session
// sign-in store is not empty, when a brokered credential minted for it may
// still be valid, when it is a harness sign-in run, or when a support helper
// was let into it. Any one refuses the snapshot, and a fact that could not be
// read refuses it too.
package secretspresent

// Reason names one cause of secrets being present.
type Reason string

const (
	// ResidentGrant: a credential lives inside the sandbox for the run.
	ResidentGrant Reason = "resident_grant"
	// GitToken: the run's git token was handed to the sandbox's git helper,
	// because the never-resident git broker was off when the run was
	// dispatched or revived.
	GitToken Reason = "git_token"
	// Takeover: a person took over the sandbox ("Needs you").
	Takeover Reason = "takeover"
	// SigninStore: a desktop sign-in began, so the person's store may hold
	// logins (a begun sign-in counts: the sandbox cannot be trusted to say
	// its store is empty).
	SigninStore Reason = "signin_store"
	// RecentCredential: a brokered credential minted for the run may still be
	// valid, and the agent may have written it to disk.
	RecentCredential Reason = "recent_credential"
	// HarnessLogin: the run is a harness sign-in, whose token is minted
	// inside the sandbox.
	HarnessLogin Reason = "harness_login"
	// SupportSession: a support helper was let into the run, so what they
	// typed or uploaded may be on its disk.
	SupportSession Reason = "support_session"
	// Unknown: a fact could not be read; the rule fails closed.
	Unknown Reason = "unknown"
)

// Inputs are the facts the rule reads.
type Inputs struct {
	// ResidentGrants counts the run's resident grants (env_secret,
	// file_secret, ssh_key, AWS role credentials, component env or file).
	ResidentGrants int
	// GitTokenResident is whether the run's git token lives in the sandbox.
	GitTokenResident bool
	// TakeoverHappened is whether any "Needs you" takeover was taken.
	TakeoverHappened bool
	// SigninBegun is whether any desktop sign-in began.
	SigninBegun bool
	// RecentCredential is whether a credential minted for the run may still
	// be valid in the window the caller asked about.
	RecentCredential bool
	// HarnessLogin is whether the run is a harness sign-in.
	HarnessLogin bool
	// SupportSession is whether a support helper was ever let into the run.
	SupportSession bool
	// Unreadable is set when any of the above could not be read.
	Unreadable bool
}

// Evaluate reports whether secrets are present and why, in a fixed order.
func Evaluate(in Inputs) (bool, []Reason) {
	var out []Reason
	for _, c := range []struct {
		on bool
		r  Reason
	}{
		{in.Unreadable, Unknown},
		{in.ResidentGrants > 0, ResidentGrant},
		{in.GitTokenResident, GitToken},
		{in.TakeoverHappened, Takeover},
		{in.SigninBegun, SigninStore},
		{in.RecentCredential, RecentCredential},
		{in.HarnessLogin, HarnessLogin},
		{in.SupportSession, SupportSession},
	} {
		if c.on {
			out = append(out, c.r)
		}
	}
	return len(out) > 0, out
}
