// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// ImageAdmissionVerdict is what image admission decided for a run's member image at create (or at a queued
// run's start), or what the run's launch recorded for an image the member did not name (Ref, Origin), kept on
// the run row (AgentRun.ImageAdmission). It carries only public facts: the digest, the
// signer's name and who signed.
type ImageAdmissionVerdict struct {
	// Mode is how the image was held: enforce when it could be refused (the site mode is enforce, or the
	// member's governance profile requires a signer), audit when it was only reported.
	Mode string `json:"mode,omitempty"`
	// Verdict is ImageAdmissionAdmitted, ImageAdmissionWouldRefuse (audit only) or ImageAdmissionNotApplicable
	// (a macOS VM, which never runs the image). Mode and Verdict are empty only on a record of Origin alone.
	Verdict string `json:"verdict,omitempty"`
	// Reason is the refusal reason audit reported (would_refuse only).
	Reason string `json:"reason,omitempty"`
	Digest string `json:"digest,omitempty"`
	// PinnedRef is the member's ref pinned to Digest (name, any tag, @sha256:...), set only when the image was
	// enforced and admitted: the ref the run's image consumers use, so what runs is what was checked.
	PinnedRef    string   `json:"pinned_ref,omitempty"`
	Signer       string   `json:"signer,omitempty"`
	Identity     string   `json:"identity,omitempty"`
	Issuer       string   `json:"issuer,omitempty"`
	KeyID        string   `json:"key_id,omitempty"`
	Attestations []string `json:"attestations,omitempty"`
	// Cached is true when the verdict came from the admitter's cache (up to an hour old) rather than a read of
	// the registry for this create.
	Cached bool `json:"cached,omitempty"`
	// SiteAudit is the reason the site's own signers would refuse the image under audit, when the governance
	// profile's enforced signers admitted it.
	SiteAudit string `json:"site_audit,omitempty"`
	// Ref is the image checked when it is not the member's: the convention image (agent, harness set or
	// architecture variant) a launch checked under scope all, as configured. Empty for a member's image, whose
	// ref is the run's ImageInput. A revive re-checks Digest under Ref's coverage.
	Ref string `json:"ref,omitempty"`
	// Origin is where the image came from when the member named none, written by every launch whatever the
	// admission mode: ImageOriginConvention, ImageOriginDevcontainer or ImageOriginWorkspace; ImageOriginWardyn
	// is written at create by newStepRun alone. Alone (no Mode or Verdict) when admission decided nothing about
	// the image. Empty for a member's image and for a run launched
	// before origins were recorded.
	Origin string `json:"origin,omitempty"`
}

// ImageAdmissionVerdict.Origin values.
const (
	ImageOriginConvention   = "convention"   // the agent's, harness set's or an architecture variant's image
	ImageOriginDevcontainer = "devcontainer" // built from the request's devcontainer repository
	ImageOriginWorkspace    = "workspace"    // built from a workspace with no base image: its repo's devcontainer or a generated image
	ImageOriginWardyn       = "wardyn"       // a run Wardyn launches itself (newStepRun): scan, record, verify, harness login, capacity probe; never checked by image admission
)

// ImageAdmissionVerdict.Verdict values.
const (
	ImageAdmissionAdmitted      = "admitted"
	ImageAdmissionWouldRefuse   = "would_refuse"
	ImageAdmissionNotApplicable = "not_applicable"
)
