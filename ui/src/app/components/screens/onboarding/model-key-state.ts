/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Appendix A finding 2 + 2b (0.7.4 field report) — ONE total truth table
// deciding what "Your model key" claims, replacing the two independent
// readiness signals (`llm_ready` vs `status.model_access`) that could
// disagree on the same page. Pure and synchronous so the card
// (done/showEmptyForm) and the checklist (member-getting-started.tsx's
// modelKeyDone) read the SAME predicate — the "one source of truth"
// discipline this file's sibling (your-model-key.tsx) already applies to
// `mine`.
import type { SetupHarnessTool, SetupModelAccess } from "../../../lib/types";

export type ModelKeyResult =
  | "own"
  | "signed_in"
  | "expiring"
  | "not_signed_in"
  | "shared_expired"
  | "provided"
  | "unknown";

// FIX PASS 1 (REVIEW-1.md ruling R1) — a member's own API key can satisfy a
// run ONLY when the governing harness row is neither `per_user` (the row's
// own credential_source, "Model access · Your AWS sign-in" territory) nor a
// Bedrock mechanism (`mechanismSatisfied`, runs_dispatch_llm_mechanism.go,
// refuses an Anthropic/OpenAI key on ANY Bedrock roster row — a provider
// mismatch, not just a per_user one). Takes the harness row's own two wire
// fields directly (the shape `modelKeyProvider` reads from `harnesses`) so
// the formula reads exactly as ruled.
export function ownKeyApplies(row?: Pick<SetupHarnessTool, "credential_source" | "mechanism"> | null): boolean {
  return !(row?.credential_source === "per_user" || (row?.mechanism ?? "").startsWith("bedrock_"));
}

export interface ModelKeyStateInput {
  hasOwn: boolean;
  llmReady: boolean;
  modelAccess?: SetupModelAccess;
  // The governing harness row's credential_source (modelKeyProvider's
  // extended return value).
  credentialSource?: string;
  // The governing harness row's declared mechanism (modelKeyProvider's
  // extended return value) — only the "bedrock_" prefix is graded.
  mechanism?: string;
}

export interface ModelKeyState {
  result: ModelKeyResult;
  // done ∈ {own, signed_in, expiring, provided} — never provided/own when
  // ownKeyApplies(row) is false (the defect Appendix A finding 2 reports).
  done: boolean;
  // Whether the CARD's own "Sign in to AWS" button renders. The chip row
  // keeps its own regardless of this (both call the same
  // setAwsLoginOpen(true) — one HarnessLoginPane mount).
  button: boolean;
  // Whether "Use my own key instead" / the bring-your-own-key form can ever
  // appear — mirrors ownKeyApplies(row): hidden under a per_user row AND
  // under a shared row whose mechanism is Bedrock.
  revealAllowed: boolean;
  // Which of the ruling's three row groups governs — component-only
  // concern, for choosing between the two distinct "unknown" bodies
  // (PER_PERSON_NA_BODY vs ADMIN_NOT_READY_BODY).
  band: "per_user" | "shared_bedrock" | "other";
}

export function modelKeyState({
  hasOwn,
  llmReady,
  modelAccess,
  credentialSource,
  mechanism,
}: ModelKeyStateInput): ModelKeyState {
  const perUser = credentialSource === "per_user";
  const ownApplies = ownKeyApplies({ credential_source: credentialSource, mechanism });
  const state = modelAccess?.state;

  const band: ModelKeyState["band"] = perUser
    ? "per_user"
    : (mechanism ?? "").startsWith("bedrock_")
      ? "shared_bedrock"
      : "other";

  let result: ModelKeyResult;
  if (band === "per_user") {
    // hasOwn is IGNORED under per_user — a member's own ANTHROPIC_API_KEY can
    // never satisfy this lane, so "Your key" + a done checklist would sit
    // over runs that are all refused (Appendix A finding 2's own suggested
    // ordering had this backwards; both blind reviewers verified it in the
    // tree).
    if (state === "live") result = "signed_in";
    else if (state === "expiring") result = "expiring";
    else if (state === "not_configured" || state === "expired_signin") result = "not_signed_in";
    // shared_expired / not_applicable / absent / any unrecognised string —
    // ALL land here. not_applicable in particular is not a rare skew case:
    // it is emitted for exactly the admin-token principal ON an enabled
    // per_user row (awsSSOScopeIsMechanism, internal/api/modelaccess.go), so
    // this branch is reached in real traffic, not only synthetic tests.
    else result = "unknown";
  } else if (band === "shared_bedrock") {
    // ownApplies is false here too (mechanism starts with bedrock_): hasOwn
    // is IGNORED for the same provider-mismatch reason as per_user.
    if (state === "shared_expired") result = "shared_expired";
    else result = llmReady ? "provided" : "unknown";
  } else {
    // band === "other" — rows 5-7 of the original table, unchanged: an
    // api-key/subscription mechanism, or no roster row at all.
    if (hasOwn) result = "own";
    else if (state === "shared_expired") result = "shared_expired";
    else result = llmReady ? "provided" : "unknown";
  }

  return {
    result,
    done: result === "own" || result === "signed_in" || result === "expiring" || result === "provided",
    button: result === "expiring" || result === "not_signed_in",
    revealAllowed: ownApplies,
    band,
  };
}
