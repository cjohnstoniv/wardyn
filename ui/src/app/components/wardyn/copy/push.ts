/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Held-push canon (#181, built on #180/#494's server side) — byte-exact
// strings for the push_content approval kind: the console card, the run rail's
// "Push rules" section, and the kind label everywhere push_content appears in
// a list. Mock approved by the owner (packet 7, 2026-09-22) — see
// docs/design/held-push-canon.md for the strings table and the three
// mock questions (Q181-1..3) this file answers.
//
// A SEPARATE module from copy.ts (not a fourth one merged in): copy.ts is
// already a thin barrel near its own size discipline, and this feature's
// strings have no reader outside the push card / rail — see copy/approvals.ts
// for the ONE cross-cutting entry (WIRE_TO_COPY) this feature needs there.
export const PUSH = {
  KIND_LABEL: "Push",
  CARD_TITLE: "A push is held for review",
  FIELD_REPOSITORY: "Repository",
  FIELD_BRANCH: "Branch",
  // Render acts_as_label ONLY — never the raw acts_as ("<grant kind>:<uuid>"),
  // which is a credential reference, not a principal (see
  // PushContentScope's own doc in lib/types/approvals.ts).
  FIELD_ACTS_AS: "Acts as",
  PATHS_TITLE: "Files under review",
  PATHS_MORE: (n: number) => `+${n} more`,
  // Q181-2: the card never expands past the first ten paths — the full list
  // is in the run's audit trail, not a click away on this card.
  PATHS_NOTE: "These are the paths your push rules hold for review. The push carries other files too.",
  // Follows the reused APPROVAL_BANNER_LABEL.what ("What you're approving:").
  WHAT: (repo: string, person: string) => `the run pushes this branch to ${repo}, as ${person}.`,
  // Follows the reused APPROVAL_BANNER_LABEL.blast ("Blast radius:").
  BLAST:
    "the whole push reaches the branch, including files not listed here. Approving does not narrow it to the paths under review.",
  HELD_NOTE:
    "The push is waiting at the proxy for a short time. Approving lets it through now, or the next time the run pushes.",
  APPROVE_NOTE: "Approving lets this push through. The next push asks again.",
  TIMEOUT_BODY: "Nobody answered in time, so the push was refused and the run was told. It can push again.",
  // Never a held request — a push_rules.deny_paths match is refused
  // synchronously, before any approval row exists (push_rules.go). Rendered
  // wherever that refusal appears in a list (the run's audit trail), never on
  // the push card itself.
  DENIED_PATH_BODY: "This push touched a path your rules deny, so it was refused and the run was told. Nobody was asked.",
  RAIL_TITLE: "Push rules",
  RAIL_BODY: (denied: number, review: number) =>
    `${denied} ${pathWord(denied)} denied · ${review} ${pathWord(review)} held for review`,
  RAIL_UNATTENDED: "Nobody is here to answer, so a push that would be held is refused instead.",
} as const;

function pathWord(n: number): string {
  return n === 1 ? "path" : "paths";
}
