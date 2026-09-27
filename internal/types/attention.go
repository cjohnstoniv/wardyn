// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// AttentionKind names WHAT a live run is waiting on (#1197's attention
// rule). Absent (RunAttention nil) means the run needs nobody: it is either
// terminal, ended by lease, or holds nothing PENDING right now.
type AttentionKind string

const (
	// AttentionApproval: a held approval (any kind but credential_reauth) is
	// parking the sandbox.
	AttentionApproval AttentionKind = "approval"
	// AttentionReauth: the run's own model credential lapsed mid-run and the
	// proxy is holding the next credential exchange for its owner to sign in.
	AttentionReauth AttentionKind = "reauth"
	// AttentionADOConsent: the same as AttentionReauth, specifically an Azure
	// DevOps sign-in or consent request (TallyReauthADOSignIn/Consent).
	AttentionADOConsent AttentionKind = "ado_consent"
	// AttentionLost: the run lost its sandbox for a reason other than its
	// lease ending (reboot, outage) and needs a revive.
	AttentionLost AttentionKind = "lost"
)

// AttentionBy names WHO the run is waiting on, in the viewer's own console
// view (User or Admin — see the RunAttention doc for how the two differ).
type AttentionBy string

const (
	// AttentionYou: the viewer themselves can act (decide the approval, sign
	// in, revive the run).
	AttentionYou AttentionBy = "you"
	// AttentionOwner: only the run's OWNER can act (a credential_reauth /
	// lost-run revive is never delegable), and the viewer is not the owner —
	// only reachable in the Admin view, since the User view forces owner=me.
	AttentionOwner AttentionBy = "owner"
	// AttentionAdmin: only a security operator can act — the viewer is a
	// member who cannot decide this run's held approval.
	AttentionAdmin AttentionBy = "admin"
)

// RunAttention is GET /runs?view=/GET /me/attention's projection onto one
// live run: what it is waiting on, who (in the caller's own view) can clear
// it, and how many PENDING approvals it is carrying. Nil on a run the
// attention() rule has nothing to say about (terminal, lease-ended, or
// nothing PENDING is actually held).
type RunAttention struct {
	Kind    AttentionKind `json:"kind"`
	By      AttentionBy   `json:"by"`
	Pending int           `json:"pending"`
}
