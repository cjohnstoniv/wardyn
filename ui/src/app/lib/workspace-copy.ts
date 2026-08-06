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

// ============================ HONESTY CANON (verbatim) ============================
// mockup/wardyn-workspaces.js's `C` — the "Add workspace" wizard + workspace
// detail hub's honesty canon.
export const C = {
  DECLARED: "Declared by workspace files (untrusted) — names only, values are never read.",
  LOCATION_ONLY:
    "Only the location and detector are flagged — the secret value itself is never shown or stored.",
  BLIND_SPOT:
    "Detected from committed files only — runtime hosts hidden behind env-var defaults, secrets mentioned only in docs, and files deeper than 4 levels are not visible to the scan.",
  REQ_DEF: "Required — attached to every run that uses this workspace, no prompting.",
  OPT_DEF: "Optional — off by default; a run can switch it on when it needs it.",
  SEEDED: "Defaults below came from the scan. You decide.",
  SECRET_INJECT: "A required secret is injected proxy-side at launch; it is never written into the sandbox.",
  UNMET_OK: "Runs will start without it; whatever needs it will fail at that point.",
  MODEL_INJECT:
    "The key is injected proxy-side at use time — it is never written into the sandbox, and the API never returns it.",
  IMAGE_ENV: "Container images aren't scanned — the image is the environment.",
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

// ============================ Base-image step 2/3 canon (verbatim) ============================
// mockup/wardyn-workspaces.js's `V2C` — the source/base-image wizard steps.
export const V2C = {
  S1_BLURB:
    "Name it and add what the sandbox should see. A workspace holds one or more sources — directories from this machine, repos cloned fresh, or scratch space that exists only for the run.",
  S2_BLURB:
    "The sandbox boots from one base image. Wardyn scans your sources to see what the image needs, and suggests images that fit.",
  FLOOR: "Every workspace has at least one source; this scratch directory is the floor.",
  EPH_HELP:
    "A scratch directory that exists only inside the sandbox — nothing on this machine is used, and it's discarded when the run ends.",
  WAIT_NOTE: "Suggestions improve when the scan lands — you can change the image on the workspace's page any time.",
  REC_SUB: "Wardyn builds and caches this image from the scan, and rebuilds it when the profile changes.",
  CUSTOM_SUB: "Wardyn builds and caches your custom image like the recommended one.",
  STEPS_HELP:
    "Dockerfile instructions appended after the tools above (RUN, ENV, ARG). They execute during the image build, inside the build sandbox — never on this machine.",
  CRED_WARN:
    "Anything baked into an image can be read by every run that uses it — and by anyone who can pull the image. Prefer a brokered secret instead: it's injected at use time and never stored in the image.",
  IMG_NO_INJECT: "Wardyn doesn't inspect the image and never injects tools into it.",
};

// ============================ Run-deltas canon (verbatim) ============================
// mockup/wardyn-rundeltas.js's `RD` — the Integrations knock-on copy for the
// workspace wizard's base-image step, New Run's Access step, and the Composer
// picker (model access RESOLVES: run override → workspace pin → server default
// → honest none; it is never configured on these surfaces).
export const RD = {
  ADVISORY: "This is advisory only — it never gets the run's credentials.",
  NONE_LINE: "No integration can drive Claude Code. This run launches; its first model call fails.",
  EXEC_LINE: "Governed command — no model access is wired, and nothing suggests otherwise.",
};

// ============================ Power-source line (verbatim, ×3 variants) ============================
// mockup/wardyn-rundeltas.js's powerLine(): a fixed "Agent runs here use: "
// lead-in followed by one of three resolved-source clauses. The mock bolds the
// clause in JSX; this module carries the plain text only (no React) — flattened
// into one string per variant so the words stay byte-exact regardless of how a
// later page chooses to style them.
// ============================ S3 record-last canon (verbatim) ============================
// mockup/wardyn-workspaces.js's `RD2` — the requirements step's dependency-order
// redesign: Reach · Secrets · Files & services · Verify. Verify consumes what
// the other three declare, so it reads last; the power source lives on Reach.
// "Verify" is the owner's reframe of Record: a session drives the real
// container, anything not already in the contract is HELD at the door for a
// live approve/deny (approve writes the row into THIS workspace immediately —
// the decide() hook), adjust and retry as needed, then save.
export const RD2 = {
  REACH_LEAD:
    "Everything outside the sandbox this workspace touches. Named systems first — an integration is the reason a host is on the allowlist at all.",
  EGRESS_TIP: "Managed by the resolved integration; change it on the power-source line above.",
  SECRET_TIP: "Managed by the resolved integration — not edited here.",
  ESCAPE: "Not sure what it needs? Verify with a session first →",
  CARRY: "What a verify session will carry",
  CARRY_FROM: "Derived from the contract as it stands — Reach, Secrets and Files & services, as of now.",
  RECORD_LEAD:
    "Verify what you've set up — and discover what you couldn't declare. A session drives the workspace for real; anything not already in the contract is held at the door for you to approve or deny, live.",
  RECORD_NEEDS:
    "Nothing resolves for this image's agent tool, so an agent-driven verify session has nothing driving it. The power source lives on the Reach tab. A terminal session needs no model.",
  RECORD_LOOP:
    "Approving a held host writes it into this workspace's contract immediately. Everything else a session observes comes back as \u201cfrom session\u201d suggestions in Reach, Secrets and Files & services — reviewed row by row, never promoted for you.",
  RECORD_SUB: "Drive it once; approve or deny what it asks for at the door, adjust, retry — then save.",
  // The wizard reach card's own lines (the proto's power card, display-only —
  // "its page" = the workspace detail page, where the real pin control lives).
  POWER_NONE_BODY:
    "Nothing pinned here and no server default — governed commands still run; an agent-driven session has nothing driving it. Pin one on its page.",
  POWER_RESOLVES_BODY: "Resolves from Integrations — run override → workspace pin → server default. Pin one on its page.",
  TERMINAL_ONLY: "A terminal verify session drives the sandbox by hand — no model, no agent tool.",
};
