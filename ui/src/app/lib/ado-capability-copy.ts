/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Azure DevOps capability-request-card copy (0.7.10, plan slice S10) — a
// SUBSET of the frozen canon tables in docs/design/ado-entra-prompt.md
// §7.4 (capability labels) and §7.6 (the capability request card), plus
// §7.8's TOOL_CALL_NOTE, transcribed verbatim.
//
// WHY A SUBSET, AND WHY A SEPARATE FILE: PR #415 (feat/386-ado-access) adds
// ui/src/app/lib/ado-entra-copy.ts, the full ADO namespace for §7.2-§7.8 —
// but that file is not on this branch's base (feat/390-ado-hold). Re-typing
// the same 217-row transcription here would drift from #415's copy the
// moment either one is edited, so this file carries ONLY the keys the
// capability request card actually renders (this round's S10 slice), under
// a namespace of its own (ADO_CAPABILITY, not ADO) so the two never collide.
// AT MERGE: delete this file, re-point ado-capability-card.tsx's import at
// #415's ado-entra-copy.ts ADO.*, and delete this file's test.
//
// Keys deliberately NOT carried here, and why (§7.6/§7.4 rows this card does
// not render, because the real wire scope — adoCapabilityScope,
// internal/api/injection_ado_capability.go — does not carry the data they
// need):
//   - REQ_FIELD_BRANCH/PR/CHANGE/ACTS_AS, REQ_ACTS_AS_HINT, REQ_WHAT_*,
//     REQ_BLAST_*, REQ_UNPROTECTED_REF, REQ_PROTECTED_REF_TITLE/BODY: all
//     need a ref name, a PR number or the acting person's display name; the
//     canonical scope carries none of those (lane, provider_id, org,
//     grant_id, capability, repo, ref_class, tool, cmd only). The
//     capability heading (CAP_* below) already distinguishes a protected
//     push (policy_bypass) from an ordinary one (code_write), so the ref
//     class still reaches the card — see AdoRefNote in the card component.
//   - REQ_COUNTDOWN: no expiry timestamp reaches the client for a tool_call
//     hold (unlike egress's HOLD_TIMEOUT_MS), so a live countdown here would
//     be a fabricated number, not a read one.
//   - REQ_CEILING_TITLE/BODY, REQ_ALWAYS_DENIED_TITLE/BODY,
//     REQ_GOVERNANCE_TITLE/BODY, REQ_UNCLASSIFIED_HEADING/TITLE/BODY: every
//     one of these refusals is answered SYNCHRONOUSLY to the sandbox
//     (answerADOCapability's c.Grantable()/ceiling checks, raiseADOCapability's
//     always_deny check) — none of them ever creates an ApprovalRequest row,
//     so no card can ever reach these states. Building them would be UI for
//     a state the server cannot produce.
//   - REQ_CONSENT_SOURCE/BODY/OTHER_BODY, REQ_REFUSED_CHIP, LIST_ENDED_CHIP/
//     BODY, OUTCOME_*, COL_*: see ado-capability-card.tsx's own doc comment.
//   - REQ_SOURCE_LIST: the standalone /approvals list already shows the run's
//     name, starter and a link via RunContextRow (reused, not re-drawn) —
//     see ado-capability-card.tsx's mount in screens/approvals.tsx.
export const ADO_CAPABILITY = {
  // ---- §7.4 `ADO` — capability labels (the card's heading) ----
  CAP_READ: `Read`,
  CAP_CODE_WRITE: `Push`,
  CAP_PR: `Pull requests`,
  CAP_POLICY_ADMIN: `Branch policies`,
  CAP_POLICY_BYPASS: `Push past a branch policy`,
  CAP_BUILD_EXECUTE: `Run pipelines`,
  CAP_REPO_ADMIN: `Repository settings`,
  CAP_WORK_WRITE: `Work items`,
  CAP_WIKI_WRITE: `Wiki`,
  // CAP_TOKENS is deliberately absent: it labels CapDeniedTokens, which is
  // never Grantable() — no PENDING row can ever carry it (see this file's
  // "always refused" note above), so no heading map needs it.

  // ---- §7.6 `ADO` — the capability request card ----
  REQ_WAITING: `Waiting for you`,
  REQ_WAITING_OTHER: (person: string) => `Waiting for ${person}`,
  REQ_SOURCE: (ts: string) => `Azure DevOps · asked by this run at ${ts}`,
  REQ_FIELD_REPOSITORY: `Repository`,
  REQ_FIELD_COMMAND: `Command`,
  REQ_FIELD_REQUEST: `Request`,
  REQ_HELD: (thing: string) => `The ${thing} is held at the proxy for up to four minutes while you answer. Nothing has reached Azure DevOps.`,
  REQ_SCOPE_READOUT: (scope: string) => `Scope: ${scope}`,
  REQ_APPROVING_ONCE: (thing: string) => `Approving lets this one ${thing} through. The next one asks again.`,
  REQ_APPROVING_RUN: (thing: string) => `Approving lets every ${thing} from this run through until it ends. Nothing carries to the next run.`,
  REQ_DENYING: (thing: string) => `Denying refuses this ${thing} and tells the tool why. The run keeps going and may ask again.`,
  REQ_SCOPE_ONCE_HINT: (thing: string) => `This one ${thing} goes through. The next one asks again.`,
  REQ_SCOPE_RUN_HINT: (thing: string) => `Every ${thing} from this run goes through until it ends. (default)`,
  REQ_SCOPE_UNTIL_REFUSED: `Refused for Azure DevOps capabilities: a capability can't outlive the run that was granted it.`,
  REQ_SCOPE_ALWAYS_REFUSED: `Refused for Azure DevOps capabilities: nothing here is saved to the workspace.`,
  REQ_CONSENT_CHIP: `Needs your Microsoft consent`,
  REQ_CONSENT_CTA: `Allow and continue`,
  REQ_NOT_YOURS_CHIP: `Not yours to decide`,
  REQ_NOT_YOURS_BODY: (person: string) => `Only ${person}, who started this run, or an admin can answer this.`,

  // ---- §7.8 Sentences this design falsifies — rewritten in the same change ----
  TOOL_CALL_NOTE: `An Azure DevOps capability request is held at the proxy until someone decides it — nothing reaches Azure DevOps first, and a denial is a refusal, not a note. Every other kind of tool-call approval is a record of what a run said it was about to do; nothing stops it.`,
} as const;
