// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

// Reason is why an image was not admitted. The set is closed, and each value is the API reason code the
// refusal carries.
type Reason string

const (
	// NoPolicy: no signer the deployment trusts covers the image, or a signer the member's governance profile
	// requires does not cover it or is no longer defined. Decided before any registry read.
	NoPolicy Reason = "image_admission_no_policy"
	// SignatureMissing: no candidate is a signature by a required signer.
	SignatureMissing Reason = "image_signature_missing"
	// SignatureInvalid: the image is signed, but by another identity or key than the signer requires.
	SignatureInvalid Reason = "image_signature_invalid"
	// AttestationMissing: the signer signed the image but did not attest a type it requires.
	AttestationMissing Reason = "image_attestation_missing"
	// AdmissionUnavailable: the registry or a trust root could not be read. Set by the caller that reads
	// them; the registry's own error is the Unavailable type.
	AdmissionUnavailable Reason = "image_admission_unavailable"
)

// Verdict is the outcome of one check. An empty Reason is an admission.
type Verdict struct {
	Reason Reason `json:"reason,omitempty"`
	Digest string `json:"digest,omitempty"`
	// Signer is the signer that admitted the image, or the one the refusal is about ("" for a site-wide
	// no_policy or signature_missing; Signers then lists the signers tried).
	Signer  string   `json:"signer,omitempty"`
	Signers []string `json:"signers,omitempty"`
	// Who signed, for an admission: a keyless identity and issuer, or a key id.
	Identity     string   `json:"identity,omitempty"`
	Issuer       string   `json:"issuer,omitempty"`
	KeyID        string   `json:"key_id,omitempty"`
	Attestations []string `json:"attestations,omitempty"`
	// Refusal facts. Want is what Signer requires (an identity or identity pattern, a key id, or an
	// attestation type); Found is who signed instead (signature_invalid only).
	Want        string `json:"want,omitempty"`
	WantIssuer  string `json:"want_issuer,omitempty"`
	Found       string `json:"found,omitempty"`
	FoundIssuer string `json:"found_issuer,omitempty"`
	// Undefined: the governance profile names a signer the deployment does not define (no_policy only).
	Undefined bool `json:"undefined,omitempty"`
	// Detail is the first verification error when no candidate verified (signature_missing), or why the
	// registry could not be read (image_admission_unavailable).
	Detail string `json:"detail,omitempty"`
	// Cached: the verdict was kept from an earlier check of the same digest under the same requirements.
	Cached bool `json:"cached,omitempty"`
}

// Admitted reports whether the image passed.
func (v Verdict) Admitted() bool { return v.Reason == "" }
