// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// ImageAdmissionVerdict is what image admission decided for a run's member image at create (or at a queued
// run's start), kept on the run row (AgentRun.ImageAdmission). It carries only public facts: the digest, the
// signer's name and who signed.
type ImageAdmissionVerdict struct {
	// Mode is how the image was held: enforce when it could be refused (the site mode is enforce, or the
	// member's governance profile requires a signer), audit when it was only reported.
	Mode string `json:"mode"`
	// Verdict is ImageAdmissionAdmitted, ImageAdmissionWouldRefuse (audit only) or ImageAdmissionNotApplicable
	// (a macOS VM, which never runs the image).
	Verdict string `json:"verdict"`
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
	// SiteAudit is the reason the site's own signers would refuse the image under audit, when the governance
	// profile's enforced signers admitted it.
	SiteAudit string `json:"site_audit,omitempty"`
	// Ref is the image checked when it is not the member's: the convention image (agent, harness set or
	// architecture variant) a launch checked under scope all, as configured. Empty for a member's image, whose
	// ref is the run's ImageInput. A revive re-checks Digest under Ref's coverage.
	Ref string `json:"ref,omitempty"`
}

// ImageAdmissionVerdict.Verdict values.
const (
	ImageAdmissionAdmitted      = "admitted"
	ImageAdmissionWouldRefuse   = "would_refuse"
	ImageAdmissionNotApplicable = "not_applicable"
)
