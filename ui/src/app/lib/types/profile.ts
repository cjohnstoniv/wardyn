/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Recording-Mode profile synthesis — POST /api/v1/runs/{id}/profile.
// ADVISORY + read-only: Wardyn replays a recording run's observed behaviour
// (egress, exec, file writes, connects) into a PROPOSED least-privilege run +
// inline_policy for a human to review. It never creates a run or mints a credential.
import type { ComposeRunProposal, RiskItem, RiskLevel } from "./compose";
import type { RunPolicySpec } from "./policy";

// One observed egress host: the HTTP methods seen and the allow/deny/pending
// decision tallies the proxy recorded for it during the recording run.
export interface ProfileDomainObservation {
  host: string;
  methods?: string[];
  allow_count: number;
  deny_count: number;
  pending_count: number;
}

// The raw, deterministic observations the synthesis is derived from. anomalies is
// the highlighted "something unexpected happened" channel (e.g. a denied host the
// agent kept retrying, an exec the profile can't explain).
export interface ProfileObservations {
  domains: ProfileDomainObservation[];
  minted_grant_ids: string[];
  exec_argv0s: string[];
  file_writes: string[];
  connects: string[];
  anomalies: string[];
}

// POST /api/v1/runs/{id}/profile response (kind:"profile_proposal"). proposed.run
// reuses the composer RunInput shape; inline_policy is the same RunPolicySpec the
// compose-review screen already renders.
export interface ProfileProposal {
  kind: "profile_proposal";
  proposed: {
    run: ComposeRunProposal;
    inline_policy: RunPolicySpec;
  };
  risk_assessment: RiskItem[];
  overall_risk: RiskLevel;
  observations: ProfileObservations;
  warnings?: string[];
}
