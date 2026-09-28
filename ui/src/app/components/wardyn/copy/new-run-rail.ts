/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// DRAFT (M2 canon pending) — the New Run rail's TRUTH block
// Appendix A finding 1: the rail rendered two unconditional security claims —
// "Minted at launch, injected by the proxy. Never written into the sandbox." and
// "Every keystroke and every outbound connection." — neither of which consulted
// anything. The first is a FALSE ASSURANCE on a bedrock_sso deployment, read at
// the moment someone decides whether a per-user AWS credential may sit inside a
// CC1 container; the second promises recording that a stock Helm install
// (persistence.enabled=false) never captures.
//
// So every sentence below is SCOPED to the model credential and selected by what
// the SERVER resolved (SetupHarnessTool.credential_residency, overridden by
// preflight's model_credential). There is deliberately no default: an unresolved
// lane renders RESOLVED_AT_LAUNCH, never the proxy sentence.

// Recording, deduplicated: the same two facts were spelled three times
// (recording.tsx's local consts, run-detail.tsx's inline EmptyState literals)
// and had already drifted — "will ever produce one" vs "captures one". One
// spelling, and the rail reads it too.
export const RECORDING_DISABLED_TITLE = "Session recording is disabled on this deployment";
// The switch is WARDYN_RECORDING_STORE, not WARDYN_RECORDING_DIR: the DIR only
// moves the `fs` store's path and turns nothing on (docs/ENV.md), while the
// chart renders STORE=off whenever persistence.enabled=false.
export const RECORDING_DISABLED_DESC =
  "No run on this server will ever produce one — set persistence.enabled (Helm) or WARDYN_RECORDING_STORE=pg to turn it on.";

// #459 — the launch and preflight failure lines already show the server's own
// sentence, unchanged and visible; a screen reader arriving on the rail heard
// neither, since both were a bare <p> with no role="alert". These sr-only
// prefixes are spoken FIRST (Q459-1), so "what failed" precedes "why", and are
// never rendered visibly — the visible text stays exactly the server's sentence.
export const RAIL = {
  LAUNCH_ERROR_LABEL: "Launch failed",
  PREFLIGHT_ERROR_LABEL: "Preflight failed",
} as const;

export const RAIL_CREDENTIAL = {
  // residency "proxy": late-bound, swapped onto the wire, never resident.
  // U-15: "minted" was true of the Bedrock exchange this lane is NOT — a static
  // API key or a stored bearer is injected as it stands, nothing is minted for
  // it. What every proxy lane shares is where the credential goes and where it
  // does not.
  PROXY: "Model credential — injected by the proxy at launch; never written into the sandbox.",
  // residency "proxy" + staged_placeholder: the ~/.claude mount with injection
  // ON. "Proxy" is the deployment's STATED mode, not something Wardyn verified —
  // the sentinel is written by an operator-run script (scripts/stage-claude-creds.sh)
  // the daemon never reads back — so the mount is named rather than denied.
  PROXY_STAGED:
    "Model credential — this deployment injects it at the proxy; the sign-in mounted into the sandbox is staged as a placeholder.",
  // residency "sandbox", Bedrock family. The operator's own sentence.
  SANDBOX_BEDROCK:
    "Model credential — AWS credentials sign inside the sandbox, so this run holds them for its lifetime.",
  // Whose credential that is — the per_user/shared distinction, in the Barrier
  // chip's shape because it is the same kind of fact: a bound, stated up front.
  // OWNERSHIP, not status. This chip must be painted from the ROSTER ROW
  // alone — the rail never reads model_access — so it must not reuse "Your
  // AWS sign-in" (Getting Started's retired "Your model key" card's own
  // SIGNED-IN success chip, #541), which would tell a member who had not
  // signed in that they had. The row's fact is whose credential the lane
  // uses, and that is what it says.
  SANDBOX_BEDROCK_CHIP_PER_USER: "Per-person AWS sign-in",
  SANDBOX_BEDROCK_CHIP_SHARED: "Admin's credential",
  // residency "sandbox", subscription: WARDYN_SUBSCRIPTION_INJECT=off, which is
  // the COMPOSE stack's own default (threatmodel/THREAT-MODEL.md).
  SANDBOX_SUBSCRIPTION:
    "Model credential — this deployment mounts the Claude sign-in into the sandbox, so this run holds it for its lifetime.",
  // residency "image" (a `none` roster row, BYOA). The server's own
  // llmMechanismWords wording for that lane, said once in both places.
  IMAGE: "Wardyn wires no model credential — the image brings its own, and Wardyn cannot say where it lives.",
  // Nothing resolved, and the absent-row doctrine in one line: the rail states
  // no residency it was not given. This is the COMMON case, not an error — a
  // roster cannot settle residency, so only a dry run of this exact body can.
  RESOLVED_AT_LAUNCH: "Resolved at launch.",
  // …and therefore the way to find out, said where the absence is. Without it
  // "Resolved at launch." reads as "nothing to see", when the precise answer is
  // one click away on the panel directly to the left.
  RUN_PREFLIGHT_HINT: "Run Preflight to see where this run's model credential will live.",
} as const;

// #542 (design §5.6, packet MP-C — approved as drawn 2026-09-25) — the New Run
// rail's provider picker: which of THIS PERSON's own credentials a run
// authenticates with, known from the chosen provider's kind before launch
// rather than resolved only by a dry run. RAIL_CREDENTIAL above still carries
// the residency sentence every kind reuses; these are new to the picker
// itself. R5's states (a disabled or wholly ungranted default) were excluded
// from PR #1036's build (owner ruling) and R5c is now drawn by the rail-gap
// packet below (owner-approved 2026-09-25, docs/design/542-rail-gaps-mock/canon.md) —
// DEFAULT_OFF/DEFAULT_OFF_ONLY.
export const RAIL_PROVIDER = {
  LABEL: "Model provider",
  STATIC: (name: string) => `Model provider · ${name}`,
  OPTION: (name: string, what: string, state: string) => `${name} — your ${what} · ${state}`,
  PLACEHOLDER: "Choose a model provider",
  NOT_SIGNED_IN: (name: string) => `You're not signed in to AWS for ${name}.`,
  NO_TOKEN: (name: string) => `You haven't added your token for ${name}.`,
  LAUNCH_HINT: "Choose a model provider to launch.",
  CHANGED: (next: string, prev: string, harness: string) =>
    `Model provider changed to ${next} — ${prev} isn't available to ${harness}.`,
  // #542 rail-gap packet (canon.md's "R3 — selected, not connected (the three
  // missing kinds)") — the other two credential kinds ProviderNotConnectedLine
  // had no branch for: a stored key (anthropic_api_key/openai_api_key) and the
  // non-Bedrock sign-in kind (anthropic_subscription).
  NO_KEY: (name: string) => `You haven't added your key for ${name}.`,
  NOT_SIGNED_IN_CLAUDE: (name: string) => `You're not signed in to Claude for ${name}.`,
  // canon.md's "R5b — granted none": no candidate serves this person for this
  // harness at all. UNUSED for now (Opus review round 2, #542): the console
  // has no signal to tell this apart from R9's silent shape —
  // setupModelProviderState's capVisible (internal/api/provider_access.go)
  // already narrows `model_providers` to granted providers before the wire,
  // so an ungranted one never reaches the console to read as "all disabled".
  // Kept defined because it is canon-pinned (new-run-rail.test.ts); a
  // follow-up issue gives the console a real signal and wires this in.
  NOT_GRANTED: (harness: string) => `You haven't been granted a model provider for ${harness} — ask your admin.`,
  // canon.md's "R5c — the default is turned off": the admin's own default is a
  // disabled provider. DEFAULT_OFF names it when another candidate remains to
  // choose instead; DEFAULT_OFF_ONLY when none does.
  DEFAULT_OFF: (name: string, harness: string) =>
    `${name}, the default for ${harness}, is turned off. Choose another model provider to launch.`,
  DEFAULT_OFF_ONLY: (name: string, harness: string) =>
    `${name}, the default for ${harness}, is turned off. Ask your admin.`,
} as const;

// DRAFT (M2 canon pending) — U-15: the New Run rail's "recording is on"
// sentence. It was an inline literal in the rail and re-typed in its vitest and
// in ui/e2e/new-run.spec.ts, while its DISABLED twin
// (RECORDING_DISABLED_TITLE, right above) was already a shared constant — so a
// reworded promise would have moved on screen while three copies of the old one
// went on passing. Beside RAIL_CREDENTIAL rather than inside it: the rail's
// Recording section is not a credential fact.
export const RAIL_RECORDING_ON = "Every keystroke and every outbound connection.";
