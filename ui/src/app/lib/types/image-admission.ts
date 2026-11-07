/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One run's image admission verdict (Go: types.ImageAdmissionVerdict, image_admission_verdict.go).
export interface ImageAdmissionVerdict {
  // enforce when the image could be refused, audit when it was only reported.
  mode?: "enforce" | "audit" | (string & {});
  // would_refuse is audit only; not_applicable is a macOS VM, which never runs the image.
  verdict?: "admitted" | "would_refuse" | "not_applicable" | (string & {});
  // The refusal reason audit reported (would_refuse only).
  reason?: string;
  digest?: string;
  // The member's ref pinned to digest, set only when the image was enforced and admitted.
  pinned_ref?: string;
  signer?: string;
  identity?: string;
  issuer?: string;
  key_id?: string;
  attestations?: string[];
  // True when the verdict came from the admitter's cache rather than a registry read for this create.
  cached?: boolean;
  // Why the site's own signers would refuse the image under audit, when the profile's enforced signers admitted it.
  site_audit?: string;
  // The convention image a launch checked under scope all, as configured; empty for a member's image. A revive re-checks digest under its coverage.
  ref?: string;
  // Where the image came from when the member named none; alone (no mode or verdict) when admission decided nothing.
  origin?: "convention" | "devcontainer" | "workspace" | "wardyn" | (string & {});
}
