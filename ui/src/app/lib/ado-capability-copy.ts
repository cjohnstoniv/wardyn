/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Azure DevOps capability-request-card copy (0.7.10, plan slice S10) — a
// SUBSET of the frozen canon tables in docs/design/ado-entra-prompt.md
// §7.4 (capability labels), §7.6 (the capability request card) and §7.8
// (TOOL_CALL_NOTE), transcribed verbatim, PLUS §10 — new rows the round-2
// independent review found the card needs and no frozen table names (a
// per-capability noun, the ref-class field, the consent heading, the hold's
// honest-expiry sentence). §10 is an ADDITION, not an amendment: §7.2-7.8
// stay exactly as frozen 2026-09-22; see §10's own header note.
//
// WHY A SUBSET, AND WHY A SEPARATE FILE: PR #415 (feat/386-ado-access) adds
// ui/src/app/lib/ado-entra-copy.ts, the full ADO namespace for §7.2-§7.8 —
// but that file is not on this branch's base (feat/390-ado-hold). Re-typing
// the same 217-row transcription here would drift from #415's copy the
// moment either one is edited, so this file carries ONLY the keys the
// capability request card actually renders (this round's S10 slice), under
// a namespace of its own (ADO_CAPABILITY, not ADO) so the two never collide.
// KEY NAMES ARE KEPT IDENTICAL TO #415's so the swap at merge is mechanical
// (delete this file + its test, re-point ado-capability-card.tsx's import at
// ADO.*) — deferred until #415 merges, per the lead.
//
// Keys deliberately NOT carried here, and why (§7.6/§7.4 rows this card does
// not render, because the real wire scope — adoCapabilityScope,
// internal/api/injection_ado_capability.go — does not carry the data they
// need):
//   - REQ_FIELD_BRANCH/PR/CHANGE, REQ_WHAT_*, REQ_BLAST_*, REQ_UNPROTECTED_REF,
//     REQ_PROTECTED_REF_TITLE/BODY: all need a ref name or a PR number; the
//     canonical scope carries neither (lane, provider_id, org, grant_id,
//     capability, repo, ref_class, tool, cmd only). REQ_FIELD_REF_CLASS /
//     REQ_REF_CLASS_PROTECTED (§10) carry the fact the scope DOES have
//     (ref_class) without inventing the ref name it doesn't.
//   - REQ_COUNTDOWN: no expiry timestamp reaches the client for a tool_call
//     hold (unlike egress's HOLD_TIMEOUT_MS), so a live countdown here would
//     be a fabricated number, not a read one. REQ_HELD / REQ_HELD_EXPIRED
//     (§10) still say whether the request is STILL held, from requested_at.
//   - REQ_CEILING_TITLE/BODY, REQ_ALWAYS_DENIED_TITLE/BODY,
//     REQ_GOVERNANCE_TITLE/BODY, REQ_UNCLASSIFIED_HEADING/TITLE/BODY: every
//     one of these refusals is answered SYNCHRONOUSLY to the sandbox
//     (answerADOCapability's c.Grantable()/ceiling checks, raiseADOCapability's
//     always_deny check) — none of them ever creates an ApprovalRequest row,
//     so no card can ever reach these states. Building them would be UI for
//     a state the server cannot produce.
//   - REQ_CONSENT_SOURCE: needs "you allowed this at {ts}" — the ORIGINAL
//     escalation's own approval timestamp, which the consent row's wire shape
//     (adoConsentScopeBody: lane, mechanism, owner, provider_id, scopes) does
//     not carry. REQ_SOURCE(ts) is used instead, with the consent row's own
//     requested_at — honest about what actually happened (asked), not what
//     it cannot prove (allowed, and when). REQ_CONSENT_HEADING (§10) titles
//     the card in place of the paired escalation's own name — see
//     ado-capability-card.tsx's doc comment.
//   - REQ_REFUSED_CHIP, OUTCOME_*, COL_*: the decided-row/table surfaces this
//     card does not render (approvals.tsx's DecidedRow stays generic; see its
//     own comment) — approvalScopeBadge (copy.ts) carries the one outcome
//     phrase (OUTCOME_ALLOWED_ONCE/RUN) this round DOES need, for the decided
//     badge, and is extended there rather than duplicated here.
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
  REQ_FIELD_ACTS_AS: `Acts as`,
  REQ_ACTS_AS_HINT: (person: string) => `Whatever you allow happens as ${person} on Azure DevOps, and Azure DevOps records it that way.`,
  // Post-freeze correction (§7.6's own note, S10 round 2 — owner-delegated to
  // the lead, 2026-09-22): no expiry timestamp reaches the client for an ADO
  // hold, so "for up to four minutes" claimed a precision the card doesn't
  // have. REQ_HELD_EXPIRED (§10.3) is its honest "already lapsed" twin.
  REQ_HELD: (thing: string) => `The ${thing} may be waiting at the proxy for a short time; approving lets it through now or the next time the run asks.`,
  REQ_SCOPE_READOUT: (scope: string) => `Scope: ${scope}`,
  REQ_APPROVING_ONCE: (thing: string) => `Approving lets this one ${thing} through. The next one asks again.`,
  REQ_APPROVING_RUN: (thing: string) => `Approving lets every ${thing} from this run through until it ends. Nothing carries to the next run.`,
  // Post-freeze correction (§7.6's own note, S10 round 2 — owner-delegated to
  // the lead, 2026-09-22): a deny STICKS for the rest of the run since #414,
  // UNLESS a later `run`-scoped Approve of the same capability lifts it
  // (adoStanding) — see that note for the full reasoning.
  REQ_DENYING: (thing: string) => `Denying refuses this ${thing} for the rest of the run, unless this kind of change is later allowed for the whole run.`,
  REQ_SCOPE_ONCE_HINT: (thing: string) => `This one ${thing} goes through. The next one asks again.`,
  REQ_SCOPE_RUN_HINT: (thing: string) => `Every ${thing} from this run goes through until it ends. (default)`,
  REQ_SCOPE_UNTIL_REFUSED: `Refused for Azure DevOps capabilities: a capability can't outlive the run that was granted it.`,
  REQ_SCOPE_ALWAYS_REFUSED: `Refused for Azure DevOps capabilities: nothing here is saved to the workspace.`,
  REQ_CONSENT_CHIP: `Needs your Microsoft consent`,
  // Post-freeze correction (§7.6's own note, S10 round 2 — owner-delegated to
  // the lead, 2026-09-22): "You allowed it, but…" is false on the dispatch-
  // granted path (nobody approved anything; the run simply asked and Entra
  // refused the redemption). Dropped the {capability} parameter too — no
  // capability name reaches the wire scope on this path either, so this is a
  // plain string, not a function, as of round 2.
  REQ_CONSENT_BODY: `Microsoft needs your consent before Azure DevOps lets this run use this access. Reconnecting asks Microsoft for it — you'll see a consent screen, and nothing else changes. The run's request stays held meanwhile.`,
  REQ_CONSENT_CTA: `Allow and continue`,
  REQ_CONSENT_OTHER_BODY: (person: string) => `You allowed it, but only ${person} can give Microsoft the extra permission it needs — the run acts as ${person}, and consent is theirs to give. They've been shown this on their Getting started page.`,
  REQ_NOT_YOURS_CHIP: `Not yours to decide`,
  REQ_NOT_YOURS_BODY: (person: string) => `Only ${person}, who started this run, or an admin can answer this.`,
  LIST_ENDED_CHIP: `This run has ended`,
  LIST_ENDED_BODY: `There's nothing to allow — the request went away with the run. It's here so you can see what it asked for.`,
  // Reused for the decided badge (§10.4) — approvalScopeBadge (wardyn/copy.ts).
  OUTCOME_ALLOWED_ONCE: `Allowed once`,
  OUTCOME_ALLOWED_RUN: `Allowed for this run`,

  // ---- §7.8 Sentences this design falsifies — rewritten in the same change ----
  TOOL_CALL_NOTE: `An Azure DevOps capability request is held at the proxy until someone decides it — nothing reaches Azure DevOps first, and a denial is a refusal, not a note. Every other kind of tool-call approval is a record of what a run said it was about to do; nothing stops it.`,

  // ---- §10.1 `ADO` — the consequence sentences' {thing}, per capability ----
  CAP_THING_READ: `read`,
  CAP_THING_CODE_WRITE: `push`,
  CAP_THING_PR: `action`,
  CAP_THING_POLICY_ADMIN: `change`,
  CAP_THING_POLICY_BYPASS: `push`,
  CAP_THING_REPO_ADMIN: `change`,
  CAP_THING_BUILD_EXECUTE: `run`,
  CAP_THING_WORK_WRITE: `edit`,
  CAP_THING_WIKI_WRITE: `edit`,

  // ---- §10.2 `ADO` — the ref-class field and the consent card's heading ----
  REQ_FIELD_REF_CLASS: `Ref class`,
  REQ_REF_CLASS_PROTECTED: `Protected by a branch policy`,
  REQ_CONSENT_HEADING: `Azure DevOps needs more access`,

  // ---- §10.3 `ADO` — the hold's honest expiry ----
  REQ_HELD_EXPIRED: (thing: string) => `No longer waiting — approving lets the ${thing} through the next time the run asks.`,

  // ---- §10.5 `ADO` — the remaining mount-site strings ----
  REQ_RUN_UNAVAILABLE: `Couldn't load this run — try again.`,
  SCOPE_UNTIL_LABEL: `Until…`,
  SCOPE_ALWAYS_LABEL: `Always`,
  STRIP_HEADING_CONSENT: `Azure DevOps sign-in needed — sign in to let this run's Azure DevOps access through`,
  WAITING_ADO_MINE: `Waiting for your Azure DevOps sign-in`,
  WAITING_ADO_OWNER: `Waiting for the owner's Azure DevOps sign-in`,
} as const;
