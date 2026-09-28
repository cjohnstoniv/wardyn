// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// AttentionKind names what a live run is waiting on. Absent (RunAttention
// nil) means the run needs nobody — terminal, lease-ended, or nothing
// PENDING held.
type AttentionKind string

const (
	// AttentionApproval: a held approval (any kind but credential_reauth) is
	// parking the sandbox.
	AttentionApproval AttentionKind = "approval"
	// AttentionReauth: the run's model credential lapsed mid-run; the proxy
	// holds the next credential exchange for its owner to sign in.
	AttentionReauth AttentionKind = "reauth"
	// AttentionADOConsent: same as AttentionReauth, specifically an Azure
	// DevOps sign-in or consent request (TallyReauthADOSignIn/Consent).
	AttentionADOConsent AttentionKind = "ado_consent"
	// AttentionLost: the run lost its sandbox for a reason other than lease
	// end (reboot, outage) and needs a revive.
	AttentionLost AttentionKind = "lost"
)

// AttentionBy names who the run is waiting on, in the viewer's own console
// view (User or Admin; see RunAttention for how the two differ).
type AttentionBy string

const (
	// AttentionYou: the viewer themselves can act (decide the approval, sign
	// in, revive the run).
	AttentionYou AttentionBy = "you"
	// AttentionOwner: only the run's owner can act — reauth and lost-run
	// revive are never delegable — and the viewer isn't the owner; reachable
	// only in Admin view, since User view forces owner=me.
	AttentionOwner AttentionBy = "owner"
	// AttentionAdmin: only a security operator can act; the viewer is a
	// member who cannot decide this run's held approval.
	AttentionAdmin AttentionBy = "admin"
)

// RunAttention is GET /runs and GET /me/attention's projection onto one live
// run: what it's waiting on, who can clear it (in the caller's view), and
// how many PENDING approvals it carries. Nil when attention() has nothing to
// say (terminal, lease-ended, nothing PENDING held).
type RunAttention struct {
	Kind    AttentionKind `json:"kind"`
	By      AttentionBy   `json:"by"`
	Pending int           `json:"pending"`
}
