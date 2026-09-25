/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Workspace + run-deltas copy canon — verbatim transcription of the approved
// mock export (mockup/wardyn-workspaces.js's `C`/`V2C` and
// mockup/wardyn-rundeltas.js's `RD`, plus rundeltas' inline power-source-line
// strings). Screens read these instead of retyping the copy, so the redesigned
// Workspaces/Getting-Started/New-Run surfaces can't drift from the approved
// wording. Pure TS — no React, no fetch, no DOM.

// Honesty canon (verbatim)
// mockup/wardyn-workspaces.js's `C` — the "Add workspace" wizard + workspace
// detail hub's honesty canon.
export const C = {
  DECLARED: "Declared by workspace files (untrusted) — names only, values are never read.",
  LOCATION_ONLY:
    "Only the location and detector are flagged — the secret value itself is never shown or stored.",
  BLIND_SPOT:
    "Detected from committed files only — runtime hosts hidden behind env-var defaults, secrets mentioned only in docs, files deeper than 6 levels, and anything past the scan's per-run file budget (a few hundred files) or a file's first 1 MB/4,000 lines are not visible to the scan.",
  REQ_DEF: "Required — attached to every run that uses this workspace, no prompting.",
  OPT_DEF: "Optional — off by default; a run can switch it on when it needs it.",
  SEEDED: "Defaults below came from the scan. You decide.",
  SECRET_INJECT: "A required secret is injected proxy-side at launch; it is never written into the sandbox.",
  UNMET_OK: "Runs will start without it; whatever needs it will fail at that point.",
  MODEL_INJECT:
    "The key is injected proxy-side at use time — it is never written into the sandbox, and the API never returns it.",
  SERVICES:
    "Wardyn doesn't start these. They're recorded here and written into AGENTS.md so a run knows what it expects to find.",
  HOLDING: "These came from file CONTENT, which is untrusted. Approve one to add it as a requirement.",
  CLOSE_KEEPS: "Closing keeps this workspace — you can pick up from its page any time.",
  NO_CONTRACT:
    "No contract yet — the scan hasn't produced a profile. Runs can attach this workspace; nothing will be attached automatically.",
  SESSION_SURVIVES:
    "This session keeps running until you click Done — even if you navigate away. You can also stop it from Runs.",
  RESCAN_DESTROYS:
    "Rescanning re-reads the source from scratch. Your requirements and recorded sessions are cleared — they were reviewed against the old content.",
  ENV_CAVEAT:
    "Generated from the scanned profile as it is right now — regenerate after a rescan or a requirements change.",
  DIR_HELPER: "Mounted into the sandbox at run time. Wardyn reads it from this host — it is never uploaded anywhere.",
  IMG_NO_SCAN:
    "Wardyn doesn't inspect the image. There are no committed files to scan; what's inside is whatever you built into it.",
  SCAN_REPO:
    "A confined, throwaway run clones the repo and reads its committed files. Nothing is written back, and no file contents leave that sandbox.",
  SSH_GATE:
    "An SSH source needs its key stored first. Wardyn writes the private key into the sandbox at clone time, so it has to exist before this workspace can be created.",
  S3_BLURB:
    "Set what this workspace always carries, and what a run has to ask for. The scan read the directory the way a run would mount it — gitignored files included; none of it has run.",
  EMPTY_SCAN: "The scan found nothing this workspace needs. No secrets, no hosts beyond the auto-allowed set.",
  REPO_RO:
    "Repos are cloned fresh into the sandbox — nothing on your machine is touched, so there's nothing to protect with read-only.",
};

// WORKSPACE_DETAIL_DRAFT
// DRAFT (M2 canon pending): new strings staged in
// local/v074/canon/ui-workspaces-approvals.md — not part of the frozen `C`
// export above (workspace-copy.test.ts's byte-checks parse only `C`).
export const WORKSPACE_DETAIL_DRAFT = {
  // F5-F6: the Add-workspace dialog's one honest image choice — replaces the
  // two dishonest "devcontainer.json" / "standard sandbox image" picks that
  // stored byte-identical state. TITLE is used at both the OptionCard and the
  // collapsed Disclosure summary, so the two can never drift from each other.
  ADD_WORKSPACE_IMAGE_AUTO_TITLE: "Auto",
  ADD_WORKSPACE_IMAGE_AUTO_HINT:
    "devcontainer.json if this repo has one, else the standard image.",
  // F5-F10: the loop writes egress: requirement rows, not a least-privilege
  // policy directly — the policy hand-off is the separate optional "Save
  // session profile" action. This subtitle matches RecordPane's own wording.
  SESSIONS_SUBTITLE:
    "Run a task once with everything open. Wardyn watches what it reaches and you approve the hosts. Replay it confined to prove that approval is enough.",
} as const;

// M-6 (QM-10/§4.6, admin-member-modes-design.md, modes-b.html) — Record
// stays an Admin view control, but it runs on the admin's own model
// connection, made in the User view (record-pane.tsx's dependency line).
export const RECORD = {
  NEEDS_OWN_CONNECTION: "Record uses your own model connection — connect it in the user view.",
} as const;
