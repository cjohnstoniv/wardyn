/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Appendix A finding 2 + 2b (plan sunny-snacking-phoenix.md, lane
// ui-member-model-key) — ONE total truth table deciding what "Your model
// key" claims, replacing the two independent readiness signals
// (`llm_ready` vs `status.model_access`) that used to disagree on the same
// page. Pure and synchronous so the card (done/showEmptyForm) and the
// checklist (member-getting-started.tsx's modelKeyDone) read the SAME
// predicate — the "one source of truth" discipline this file's sibling
// (your-model-key.tsx) already applies to `mine`.
import type { SetupModelAccess } from "../../../lib/types";

export type ModelKeyResult =
  | "own"
  | "signed_in"
  | "expiring"
  | "not_signed_in"
  | "shared_expired"
  | "provided"
  | "unknown";

export interface ModelKeyStateInput {
  hasOwn: boolean;
  llmReady: boolean;
  modelAccess?: SetupModelAccess;
  // The governing harness row's credential_source (modelKeyProvider's
  // extended return value) — "per_user" is the only value this table
  // branches on; anything else (including absent) is the "shared/none" row.
  credentialSource?: string;
}

export interface ModelKeyState {
  result: ModelKeyResult;
  // done ∈ {own, signed_in, expiring, provided} — never provided/own under a
  // per_user row (the defect Appendix A finding 2 reports).
  done: boolean;
  // Whether the CARD's own "Sign in to AWS" button renders. The chip row
  // keeps its own regardless of this (both call the same
  // setAwsLoginOpen(true) — one HarnessLoginPane mount).
  button: boolean;
  // Whether "Use my own key instead" can ever appear. Hidden outright under
  // a per_user row: mechanismSatisfied (runs_dispatch_llm_mechanism.go)
  // never accepts an API key for a bedrock_sso lane, so offering the form
  // would be a write nothing reads.
  revealAllowed: boolean;
}

export function modelKeyState({
  hasOwn,
  llmReady,
  modelAccess,
  credentialSource,
}: ModelKeyStateInput): ModelKeyState {
  const perUser = credentialSource === "per_user";
  const state = modelAccess?.state;

  let result: ModelKeyResult;
  if (perUser) {
    // hasOwn is IGNORED under per_user — a member's own ANTHROPIC_API_KEY can
    // never satisfy this lane, so "Your key" + a done checklist would sit
    // over runs that are all refused (Appendix A finding 2's own suggested
    // ordering had this backwards; both blind reviewers verified it in the
    // tree).
    if (state === "live") result = "signed_in";
    else if (state === "expiring") result = "expiring";
    else if (state === "not_configured" || state === "expired_signin") result = "not_signed_in";
    else result = "unknown";
  } else if (hasOwn) {
    result = "own";
  } else if (state === "shared_expired") {
    result = "shared_expired";
  } else {
    result = llmReady ? "provided" : "unknown";
  }

  return {
    result,
    done: result === "own" || result === "signed_in" || result === "expiring" || result === "provided",
    button: result === "expiring" || result === "not_signed_in",
    revealAllowed: !perUser,
  };
}
