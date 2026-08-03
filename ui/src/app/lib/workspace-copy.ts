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
  WRITE_ONLY: "The value is write-only — it can be replaced or removed, never read back.",
  BLIND_SPOT:
    "Detected from committed files only — runtime hosts hidden behind env-var defaults, secrets mentioned only in docs, and files deeper than 4 levels are not visible to the scan.",
  SUGGESTS: "The scan only suggests a starting point — you set what's required.",
  REQ_DEF: "Required — attached to every run that uses this workspace, no prompting.",
  OPT_DEF: "Optional — off by default; a run can switch it on when it needs it.",
  SEEDED: "Defaults below came from the scan. You decide.",
  SECRET_INJECT: "A required secret is injected proxy-side at launch; it is never written into the sandbox.",
  UNMET_OK: "Runs will start without it; whatever needs it will fail at that point.",
  MODEL_INJECT:
    "The key is injected proxy-side at use time — it is never written into the sandbox, and the API never returns it.",
  MODEL_INHERIT: "Runs that use this image inherit this access.",
  IMAGE_ENV: "Container images aren't scanned — the image is the environment.",
  SERVICES:
    "Wardyn doesn't start these. They're recorded here and written into AGENTS.md so a run knows what it expects to find.",
  HOLDING: "These came from file CONTENT, which is untrusted. Approve one to add it as a requirement.",
  CLOSE_KEEPS: "Closing keeps this workspace — you can pick up from its page any time.",
  NO_CONTRACT:
    "No contract yet — the scan hasn't produced a profile. Runs can attach this workspace; nothing will be attached automatically.",
  CONTINUE_ANYWAY:
    "The workspace exists either way — runs can attach it. Without a scan there's no contract, so nothing is attached automatically.",
  SESSION_SURVIVES:
    "This session keeps running until you click Done — even if you navigate away. You can also stop it from Runs.",
  RESCAN_DESTROYS:
    "Rescanning re-reads the source from scratch. Your requirements and recorded sessions are cleared — they were reviewed against the old content.",
  ENV_CAVEAT:
    "Generated from the scanned profile as it is right now — regenerate after a rescan or a requirements change.",
  DIR_HELPER: "Mounted into the sandbox at run time. Wardyn reads it from this host — it is never uploaded anywhere.",
  IMG_HELPER: "A tag or digest, pulled as the sandbox's base image. Nothing is mounted — there's no path to give.",
  IMG_NO_SCAN:
    "Wardyn doesn't inspect the image. There are no committed files to scan; what's inside is whatever you built into it.",
  SCAN_DIR:
    "Languages, package managers, declared secret names, and the hosts a build would need. Names and hosts only — values are never read.",
  SCAN_REPO:
    "A confined, throwaway run clones the repo and reads its committed files. Nothing is written back, and no file contents leave that sandbox.",
  SSH_GATE:
    "An SSH source needs its key stored first. Wardyn writes the private key into the sandbox at clone time, so it has to exist before this workspace can be created.",
  LIST_LEAD:
    "Add the directories, repos and images your runs may attach. Run creation only ever offers what's added here — a free-text host path is never accepted.",
  STEP_LEAD:
    "A run attaches an added directory, repo or image — never a raw host path. Adding one scans it and makes it attachable straight away. Recording sessions, env-as-code and model access are optional, and live on each workspace's own page.",
  S1_BLURB: "Register a source your runs may attach. Wardyn scans it once and reuses that profile for every run.",
  S3_BLURB:
    "Set what this workspace always carries, and what a run has to ask for. Everything here was read from committed files; none of it has run.",
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
    "The sandbox boots from one base image. Wardyn scans your sources to see what the image needs, and suggests images that fit — including the agent tool it should carry.",
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
  HARNESS_ON: "Claude Code configured — recommended images include its CLI.",
  HARNESS_OFF:
    "No AI integration connected — images without agent tools are fine for governed commands. Add one in Integrations for agent runs.",
};

// ============================ Run-deltas canon (verbatim) ============================
// mockup/wardyn-rundeltas.js's `RD` — the Integrations knock-on copy for the
// workspace wizard's base-image step, New Run's Access step, and the Composer
// picker (model access RESOLVES: run override → workspace pin → server default
// → honest none; it is never configured on these surfaces).
export const RD = {
  RECORD_HINT:
    "Nothing resolves for this image's agent tool — agent-driven recording needs model access. Set it on the Base image step, or record a terminal session instead.",
  EGRESS_TIP: "Managed by the resolved integration; change it on the Base image step.",
  SECRET_TIP: "Managed by the resolved integration — not edited here.",
  ADVISORY: "This is advisory only — it never gets the run's credentials.",
  PIN_NOTE: "Picking one pins it to this workspace.",
  NONE_LINE: "No integration can drive Claude Code. This run launches; its first model call fails.",
  EXEC_LINE: "Governed command — no model access is wired, and nothing suggests otherwise.",
};

// ============================ Power-source line (verbatim, ×3 variants) ============================
// mockup/wardyn-rundeltas.js's powerLine(): a fixed "Agent runs here use: "
// lead-in followed by one of three resolved-source clauses. The mock bolds the
// clause in JSX; this module carries the plain text only (no React) — flattened
// into one string per variant so the words stay byte-exact regardless of how a
// later page chooses to style them.
export const POWER_LINE_NONE =
  "Agent runs here use: nothing yet — this image's agent won't have model access.";
export const POWER_LINE_PINNED = "Agent runs here use: Team API key — pinned to this workspace.";
export const POWER_LINE_DEFAULT = "Agent runs here use: server default — Anthropic (API key)";
