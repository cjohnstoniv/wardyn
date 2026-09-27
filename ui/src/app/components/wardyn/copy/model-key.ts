/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Member Getting Started's "Your model key" section (6c BYOK) — the
// provider-conventional secret name the member's own key is stored under, and
// the per-state copy. The name is a VALUE (not just a type) because the field
// shows it verbatim as a mono label; since X3-F3 it is per-provider, so
// BY_AGENT below is the only place it lives.
export const YOUR_MODEL_KEY = {
  // DRAFT (M2 canon pending) — X3-F3. The member's own key is stored under the
  // PROVIDER's conventional name, and which provider that is follows the org's
  // agent roster: a codex-only roster cannot use an anthropic key at all, so
  // offering one was a write nothing would ever read. USERS.md already names
  // both. Keyed by the harness catalog id the roster row carries.
  BY_AGENT: {
    "claude-code": { secretName: "anthropic-api-key", placeholder: "sk-ant-…" },
    "codex-cli": { secretName: "openai-api-key", placeholder: "sk-…" },
  },
  EMPTY_BODY:
    "Bring your own key. It is stored write-only under the provider's conventional name; nothing ever reads it back to you.",
  SET_HINT: "Your runs can use this key — pick it under Model access when you launch.",
  PROVIDED_CHIP: "Provided by your admin",
  PROVIDED_BODY: "Model access is already configured for you.",
  USE_OWN_KEY: "Use my own key instead",
  REFUSED_SHORT: "Keys shorter than 8 characters are refused.",
  SAVE_ERROR: "Couldn't save this key.",
  REMOVE_ERROR: "Couldn't remove this key.",
  // DRAFT (M2 canon pending) — Appendix A finding 2. Under a per_user roster
  // row the card reads status.model_access instead of the deployment-wide
  // llm_ready (see model-key-state.ts's total truth table). EXPIRING reuses
  // SIGNED_IN_BODY (still true — the sign-in just needs renewing soon) and
  // SHARED_EXPIRED reuses AGENTS.MODEL_ACCESS_SHARED_EXPIRED for its chip
  // (workspace-providers-copy.ts).
  SIGNED_IN_CHIP: "Your AWS sign-in",
  SIGNED_IN_BODY: "You signed in to AWS. Your runs use your own session.",
  EXPIRING_CHIP: "Your AWS sign-in · Expiring",
  NOT_SIGNED_IN_CHIP: "Not signed in",
  NOT_SIGNED_IN_BODY: "Sign in to AWS to give your runs model access. Nothing is configured for you until you do.",
  SHARED_EXPIRED_BODY: "Ask your admin to sign in again.",
  // FIX PASS 1 (REVIEW-1.md rulings R1/R2) — the two "unknown" bodies a
  // member with no actionable state can land on: PER_PERSON_NA_BODY for a
  // per_user row whose model_access is a state the card takes no action on
  // (not_applicable — the admin-token principal on an enabled per_user row —
  // is the common real case, not a skew artifact); ADMIN_NOT_READY_BODY for
  // a shared row whose mechanism is Bedrock but llmReady is false.
  PER_PERSON_NA_BODY: "Model access on this deployment is per person. There is nothing to set up for this sign-in.",
  ADMIN_NOT_READY_BODY: "Model access is not set up on this deployment yet. Ask your admin.",
  // DRAFT (M2 canon pending) — U-10: `expired_signin` and `not_configured` both
  // grade `not_signed_in`, and NOT_SIGNED_IN_BODY's "Nothing is configured for
  // you until you do" is false for the first: a session IS stored for this
  // member and it stopped working — which is also what the server's own action
  // line on the same page says.
  EXPIRED_SIGNIN_BODY: "Your AWS sign-in is no longer valid. Sign in to AWS again to give your runs model access.",
  // DRAFT (M2 canon pending) — U-13's other half; see
  // MEMBER_GETTING_STARTED.SIGN_IN_AWS_ARIA_SUMMARY for why.
  SIGN_IN_AWS_ARIA_CARD: "Sign in to AWS — from Your model key",
} as const;

