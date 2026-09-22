/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Workspace providers copy canon (0.7.2) — the frozen canonical-strings tables
// from docs/design/workspace-providers-prompt.md §7.2-§7.5 + §7.7, transcribed
// verbatim. The /providers screen (Git tab, Storage tab, the card, the funnel
// step, the Agents tab), the workspace-row "not an enabled provider" state, and
// the run-detail "Effective policy" widget all read these instead of retyping
// the copy, so the shipped wording can't drift from the reviewed mock.
//
// Pure TS — no React, no fetch, no DOM. Same discipline as user-drives-copy.ts
// and governance-copy.ts: the components that consume this add NO copy of
// their own.
//
// workspace-providers-copy.test.ts PARSES §7.2-§7.5 + §7.7 back out of the
// prompt doc and compares all 98 keys below against them (§7.6 is STAGING —
// field-report strings owned by other lanes — and is excluded, the way this
// doc's own header says: `/^### 7\.[2-57]\b/`), so a swapped hyphen, a dropped
// ellipsis or a new doc row fails a gate instead of shipping.
//
// Backtick-mono rule (§7 header note): a backticked substring inside a frozen
// string (a URL, a wire value, a secret or env name, a field) is PLAIN TEXT
// here — the mono span is a DISPLAY concern the consuming component applies,
// uniformly at every recurrence, never baked into the string.
//
// §7.1's REUSED canon (S.GIT_FOOTER, CAPABILITY.*, LANE_META.*, PERM.*,
// DRIVES.*, GOV.*, MEMBER_GETTING_STARTED.*, ...) is imported by the consuming
// screens directly from its own home — never re-exported here, and never
// re-frozen (§5 #1, #10): this module carries no HONESTY key and no second
// wording of where a credential goes.
//
// DELIBERATELY ABSENT — §7.1's second table (server-composed admin-facing
// refusals and run-time details: PROVIDERS_400.*, ADMIT.OPERATOR/LANE_DROPPED/
// LEGACY_HOST, DRIVES.DISABLED/CEILING, the two claim-refusal replacements,
// the agent_providers 400s, the injection rule's 403, the brokered:llm
// details). Those are rendered from the wire, verbatim, one Go constants block
// per lane — a second copy here would be a claim rather than canon (§5 #3,
// #10). PROVIDER_MEMBER below is the one place that shape is inverted: its
// three strings are server-composed too, but §7 froze them as canon keys (the
// DRIVE_MEMBER precedent in user-drives-copy.ts), so they are transcribed here
// as the wording the Go side must emit.

import { absoluteTime } from "./format";
// The two model-access POLICY exports moved to lib/model-access.ts (0.7.6) —
// re-exported here so every existing import site is untouched. They had to
// leave: this module carries the whole AGENTS copy table, and the shell's eager
// graph now reads that policy, which put 16 kB of copy into the entry chunk
// (bundle-split.test.ts). Copy lives here; the rule lives there.
export { MODEL_ACCESS_ACTIONABLE, isPerUserSsoRow } from "./model-access";

// §7.2-§7.5 — PROVIDERS, the admin screen

export const PROVIDERS = {
  // ---- §7.2 the screen header + the Git tab ----
  // TITLE is one string for four places — the screen heading, the Settings
  // card's title, the funnel step's summary card, and the tier row — the way
  // DRIVES.TITLE serves its four.
  TITLE: "Workspace providers",
  LEAD: "Where work can come from, and how big it can get. Enable a git provider to bound which repositories a run may clone; set the storage ceilings every run and every drive is held to.",
  GIT_TITLE: "Git providers",
  GIT_LEAD:
    "With no provider rows, any host with a stored credential can be cloned. Add a row to bound a host to the addresses you list; a row turned off refuses its host.",
  STORAGE_TAB: "Storage",
  KIND_GITHUB: "GitHub",
  KIND_AZURE_DEVOPS: "Azure DevOps",
  FIELD_ENABLED: "Enabled",
  ROW_ABSENT_HINT: "Not configured. Its host follows the legacy list, if listed there — every address on it, no bound.",
  ROW_DISABLED_HINT: "Off: this host is refused. Turn it on to admit the addresses below again.",
  // The off row's neutral chip. Its own key rather than ROW_DISABLED_HINT
  // sliced at its colon (the AGENT_ROW_DISABLED_CHIP precedent) — a reworded
  // hint must not silently reword a chip.
  ROW_DISABLED_CHIP: "Off",
  ADD_ROW_CTA: "Add provider",
  // Two keys, not one sentence split at its question mark: Radix's
  // AlertDialog renders a title and a description, and slicing one frozen
  // string on "? " reflows a canon edit into the wrong slot (or dumps the
  // whole sentence into the title).
  REMOVE_CONFIRM_TITLE: (kind: string) => `Remove the ${kind} row?`,
  REMOVE_CONFIRM_BODY:
    "Its host goes back to the legacy list — admitted if listed there, with no address bound. Stored credentials stay.",
  FIELD_BASE_URLS: "Allowed addresses",
  BASE_URLS_HINT: "One per line, over HTTPS. A repository is admitted when its URL starts with one of these.",
  // The sentence must not claim "at least one path segment": display.tsx
  // deliberately does NOT require one on a self-hosted host (a bare
  // `https://git.corp.example` GHES-style host is valid) — only the two
  // well-known hosts constrain the path, and only dev.azure.com REQUIRES one.
  BASE_URL_INVALID:
    "Must be an https:// URL with a host; an organisation path where the host is shared (github.com/<org>, dev.azure.com/<org>) — no port, no credentials, no trailing wildcard.",
  // The ZERO-address arm of the same pre-attempt mirror: the server refuses a
  // row with no base URLs outright (validateWorkspaceProviders' own "name at
  // least one address"), so an emptied field is invalid rather than a saveable
  // no-op — BASE_URL_INVALID's sentence diagnoses a typed LINE and reads wrong
  // for none.
  BASE_URLS_REQUIRED: "Name at least one address. A row with none admits nothing and is refused at save.",
  FIELD_LANES: "Permitted lanes",
  LANES_HINT: "Which credential a run may use for this provider. Turning one off does not delete its stored secret.",
  // #381: Azure DevOps DOES publish a token-lifecycle API (it's the PAT lane's
  // path there) — the stale claim was that no comparable API exists at all.
  // What's actually true today: Wardyn hasn't built a repo-scoped App-style
  // broker against it, so say that without promising a lane that isn't built.
  LANE_APP_UNAVAILABLE: "Not available: the App broker mints repository-scoped tokens for github.com only — Wardyn doesn't broker Azure DevOps's own token API this way (yet). Use the PAT lane there.",
  LANE_SSH_UNAVAILABLE:
    "Not available: SSH over port 443 is offered for github.com and dev.azure.com only — a self-hosted host clones over HTTPS.",
  // #380 F5: the CONSOLE half of the SSH path-scoping ceiling — an SSH clone
  // URL carries no org path, so it admits the whole host regardless of what
  // this row's addresses declare (internal/api/workspace_providers.go's
  // sshLaneExceedsPathScope, the same server rule that refuses this at save).
  // Shown as the checkbox's OWN disabled-reason (never a raw server 400)
  // whenever the row's SSH-capable addresses all carry a path — which, for
  // Azure DevOps, is EVERY legal row: its org segment is mandatory, so this
  // lane is never selectable there. Leaving lanes at their default still
  // clones over SSH host-wide, with the runtime warning unaffected.
  LANE_SSH_PATH_SCOPED:
    "Not available: this row's addresses carry an organisation path, and SSH has none to bound — it would admit the whole host. Leave lanes at their default, or drop the path.",
  // The credential lanes are keyed by the host of the row's FIRST address. With
  // no parseable address there is no host, so there is no secret name to store
  // under: every lane renders disabled with this reason rather than defaulting
  // the host — the default wrote an Azure DevOps PAT into git-pat-github-com.
  LANES_NEED_ADDRESS: "Add an allowed address first — a credential is stored under its host.",
  LEGACY_OPEN_TITLE: "No git provider rows",
  LEGACY_OPEN_BODY: "Runs clone whatever host has a credential stored, as they do today. Add a provider to bound that to addresses you name.",
  LEGACY_OPEN_OTHER_HOSTS: "A GitLab or Bitbucket token has no provider row yet — store and rotate it on the Secrets page.",
  SAVED_ELSEWHERE_TITLE: "Someone else saved providers since you loaded this page",
  SAVED_ELSEWHERE_BODY: "Reload to see their version before saving yours.",
  SAVED_TOAST: "Providers saved.",
  // Inline pluralisation — the PERM.ENFORCE_ON_BODY shape, not a second
  // helper (§5 #9). Stays on the page as an amber note until the next save,
  // in addition to the transient toast.
  SAVED_NARROWED: (n: number) =>
    n === 1
      ? `${n} onboarded source is now outside every enabled provider — runs can't clone it until an admin widens the addresses or turns its host on.`
      : `${n} onboarded sources are now outside every enabled provider — runs can't clone them until an admin widens the addresses or turns their host on.`,
  SAVE_CTA: "Save providers",
  SAVE_ERROR: "Couldn't save these providers.",
  // SAVE_REFUSED_TITLE heads every 400 the tab can meet, over the server's
  // text (§7.1's second table, unparsed); SAVE_ERROR is the unreachable-server
  // arm, not a refusal.
  SAVE_REFUSED_TITLE: "These providers can't be saved as written",
  // Replaces the Add-workspace dialog's hint that named the retired
  // GitHostCard.
  ADD_WORKSPACE_REPO_HINT: "Cloned into the sandbox when a run starts. Private repos use the credential stored under Settings → Providers for their host.",

  // ---- §7.3 the Storage tab ----
  EPHEMERAL_TITLE: "Ephemeral scratch",
  EPHEMERAL_LEAD: "The writable layer a run gets when it mounts no drive. It is wiped when the sandbox exits.",
  FIELD_DEFAULT_DISK: "Default size (MiB)",
  // F4-F8 (Appendix A V8, reclassified Low copy): numberField (storage-tab.tsx)
  // renders 0 as blank — the control is correct (0 IS "unbounded" on the wire,
  // runs_dispatch_ceiling.go), but "0 leaves..." named a value the field can't
  // display. "Leave blank" is what the operator can actually do.
  DEFAULT_DISK_HINT:
    "Fills a run that asks for no size. Leave blank for such a run to run unbounded — the maximum below binds requests, not silence.",
  FIELD_MAX_DISK: "Maximum size (MiB)",
  MAX_DISK_HINT: "A run asking for more is clamped to this, not refused. Leave blank for no ceiling.",
  // Renders under all three disk fields (the two here and the profile
  // editor's MaxEphemeralDiskMiB row) whenever the enforcement word is not
  // `filesystem` — read from /setup/status's runner block, the AUTHORING
  // daemon's driver, never a laptop's (§6.2).
  DOCKER_UNCAPPED_WARN:
    "This host's storage driver cannot enforce a size. A number filled or clamped from here runs uncapped, with a warning on the run; a size a policy or a profile writes still fails the run at create on this host.",
  DRIVE_CEILING_TITLE: "Drive ceiling",
  FIELD_DRIVES_ENABLED: "User drives",
  DRIVES_ENABLED_HINT:
    "Off means this deployment offers no drives: nothing is mounted and every drive write is refused. Existing drives and allocations are kept.",
  // DRIVES_OFF_BANNER MOVED to DRIVES.DRIVES_OFF_BANNER
  // (user-drives-copy.ts) — the banner it describes renders on /drives, and
  // that module already owns every other string that screen renders.
  FIELD_MAX_DRIVE: "Largest drive (MiB)",
  MAX_DRIVE_HINT:
    "An allocation or override above this is refused at write and clamped at resolve. Leave blank for no ceiling.",
  // Renders once, as the plain note under FIELD_MAX_DRIVE. The drives
  // HONESTY sentence (DRIVES.HONESTY) stays on /drives, unchanged.
  CEILING: "A ceiling bounds what an admin may allocate. It does not bound what the volume will hold.",

  // ---- §7.4 write refusals and states (console-rendered half) ----
  FETCH_FAILED_TITLE: "Couldn't load workspace providers",
  FETCH_FAILED_BODY:
    "Something went wrong reaching the server. The providers already saved still bound every run — this page just can't show them right now.",
  // Renders on the workspace row and as the New Run Workspace <Select>'s
  // reason line, from the server's per-source `admitted` flag (U3's wire).
  CARD_NOT_ADMITTED: "Not an enabled git provider — runs can't clone this until an admin enables its host.",
  // REHOME_TITLE MOVED to DRIVES.REHOME_TITLE (user-drives-copy.ts) — the
  // re-home confirm dialog it heads lives on /drives, not this screen.

  // ---- §7.5 the card, the step, and the entry points ----
  CARD_LEAD: "Which git hosts a run may clone, and the storage ceilings it works inside.",
  CARD_EMPTY: "No providers enabled.",
  CARD_PROVIDERS: (n: number) => `${n} git provider${n === 1 ? "" : "s"}`,
  CARD_AGENTS: (n: number) => `${n} agent${n === 1 ? "" : "s"}`,
  CARD_SUMMARY: (providers: string, agents: string) => `${providers} · ${agents}`,
  CARD_OPEN: "Manage providers",
  STEP_LABEL: "Providers",
  STEP_HEADING: "What runs can be built from",
  STEP_BADGE_READY: (n: number) => `Ready · ${n} provider${n === 1 ? "" : "s"}`,
  STEP_BADGE_READY_AGENTS: (n: number) => `Ready · ${n} agent${n === 1 ? "" : "s"}`,
} as const;

// §7.4 (member table) — PROVIDER_MEMBER

// Server-composed member doors (the DRIVE_MEMBER precedent in
// user-drives-copy.ts): keyed in the module AND byte-checked against the Go
// literal in workspace-providers-copy.test.ts. A member meets these on the
// launch path; the console renders them verbatim under its own heading. NEVER
// names a base URL, a host list, or another person (§5 #2).
export const PROVIDER_MEMBER = {
  ADMIT_MEMBER: "this repository's host is not an enabled git provider — ask an admin",
  AGENT_NOT_ENABLED: (id: string) => `agent: "${id}" is not an enabled agent on this deployment — ask an admin`,
  LLM_MECHANISM_DEAD: (mechanism: string, ts: string) =>
    `this run's model access is configured as ${mechanism}, and that credential expired at ${ts} and could not be renewed — sign in again under Settings → Model provider. Wardyn does not substitute a different model provider.`,
} as const;

// §7.7 — AGENTS

// The Agents tab (C-UI, W4), the member's Getting Started "Model access" chip,
// the New Run agent picker, and the run-detail "Effective policy" widget. Kept
// here (not a separate module) because §7 froze it in the SAME prompt doc as
// PROVIDERS, over the SAME two-column table shape `parseFrozenTables` clones —
// a second file would be a second parser for one doc. `AGENTS_TITLE` /
// `AGENTS_LEAD` / `AGENT_ROW_DISABLED_HINT` are prefixed so no
// key collides with PROVIDERS across the one cross-namespace lookup the test
// uses; the row's own Enabled switch reuses `PROVIDERS.FIELD_ENABLED`.
export const AGENTS = {
  AGENTS_TITLE: "Agents",
  AGENTS_LEAD:
    "Which coding agents this Wardyn offers, how each one reaches its model, and whether that credential is one for everyone or one per person.",
  AGENT_ROW_DISABLED_HINT: "Off: runs naming this agent are refused, and it shows as unavailable in New run.",
  FIELD_MECHANISM: "Model access",
  MECHANISM_HINT: "One lane per agent. A run whose lane is not working is refused — Wardyn never substitutes another provider.",
  MECHANISM_NONE: "None — the image brings its own",
  MECHANISM_NONE_HINT: "Wardyn wires no model credential. The only choice for an agent outside the catalog.",
  MECHANISM_BEDROCK_BEARER: "Bearer key",
  MECHANISM_BEDROCK_SSO: "SSO sign-in",
  MECHANISM_BEDROCK_ENV: "Daemon environment",
  MECHANISM_BEDROCK_AWS_DIR: "Host ~/.aws",
  FIELD_SOURCE: "Credential",
  SOURCE_SHARED: "Shared",
  SOURCE_SHARED_HINT: "One credential, captured by an admin, backs every run.",
  SOURCE_PER_USER: "Per person",
  SOURCE_PER_USER_HINT: "Each person signs in to AWS themselves. Their runs use their own session; an expiry affects one person.",
  PER_USER_UNAVAILABLE: "Not available: only an AWS SSO sign-in is captured per person in this release.",
  FIELD_SSO_START_URL: "AWS access portal start URL",
  SSO_START_URL_HINT: "Everyone signs in against this portal. A sign-in never chooses another.",
  // Replaces the login pane's start-URL FIELD when the sign-in runs under a
  // per_user row: the server signs in against the row's stored sso_start_url and
  // IGNORES a typed one, so the field was a control with no effect.
  SSO_START_URL_MANAGED: "Your admin set this organization's access portal. Your sign-in uses it — there is nothing to enter here.",
  ADMIN_OWN_CHIP_NOTE: "This is your own sign-in — the same one a member makes. Under a shared credential it is the one everyone uses.",
  // The picker item's sub-line for a disabled row (the plan's fragment,
  // sentence-cased per CONSOLE-RULES §10).
  UNAVAILABLE: "Not enabled by your admin",
  MODEL_ACCESS_LIVE: "Model access · Your AWS sign-in",
  MODEL_ACCESS_EXPIRING: "Model access · Expiring",
  MODEL_ACCESS_EXPIRING_ACTION: (ts: string) => `Sign in again before ${ts}`,
  MODEL_ACCESS_EXPIRED: "Model access · Signed out",
  MODEL_ACCESS_NOT_CONFIGURED: "Model access · Not signed in",
  MODEL_ACCESS_SHARED_EXPIRED: "Model access · Your admin's credential expired",
  MODEL_ACCESS_SHARED_EXPIRED_ACTION: "Your admin's model credential expired — ask them to reconnect it",
  // #158: the admin-token principal's own answer ("this caller is a
  // mechanism, not a person") — neutral tone, no action, since there is
  // nothing for a mechanism to sign in as.
  MODEL_ACCESS_NOT_APPLICABLE: "Model access · Not applicable",
  SIGN_IN_AWS: "Sign in to AWS",
  // Renders under the JSON policy field only when a parse succeeds and
  // min_confinement_class names no class; precedence is unchanged.
  FLOOR_UNPARSEABLE: (value: string) => `"${value}" isn't a barrier class, so this policy sets no floor — the barrier above is what launches.`,
  EFFECTIVE_TITLE: "Effective policy",
  EFFECTIVE_LEAD: "What launch narrowed, one line each. Your policy is what you wrote; this is what ran.",
  // The "nothing was narrowed" arm, read by BOTH the run-detail widget and the
  // New Run rail's preflight block — one key so the two can't drift into two
  // spellings of the same sentence.
  EFFECTIVE_NONE: "No adjustments.",
  // The 201's advisory `warnings[]`, inline in the New Run rail. The run
  // LAUNCHED; these are advisories, so the screen holds rather than navigating,
  // and OPEN_RUN_CTA becomes its primary button until the member is done
  // reading (no timer ever moves them).
  LAUNCH_WARNING_TITLE: "Run launched with a warning",
  OPEN_RUN_CTA: "Open run",
  // The off row's neutral chip. Its own key rather than AGENT_ROW_DISABLED_HINT
  // sliced at its colon — a reworded hint must not silently reword a chip.
  AGENT_ROW_DISABLED_CHIP: "Off",
} as const;

// SetupModelAccess.state -> the AGENTS chip label. SIX keys, not the seven
// lifecycle states §7.7 names: `expired_renewable` folds into `live`
// server-side (dispatch renews it) and never reaches a console surface.
//
// A lookup over frozen keys, not new copy — and ONE table rather than two,
// because the two surfaces that render this chip (the member's Getting Started
// and the Agents tab's admin-own chip) each fell back to
// MODEL_ACCESS_NOT_CONFIGURED for a state outside the six, which paints
// "Not signed in" over a credential nobody has any reading of. An ABSENT entry
// is the honest answer: no chip, no CTA. Callers must treat a miss as "no
// chip", never as a default label. `not_applicable` (#158) is the one entry
// with no action either way — it is real, just never a claim about a
// credential that has anything to sign in to.
export const MODEL_ACCESS_CHIP_LABEL: Record<string, string> = {
  live: AGENTS.MODEL_ACCESS_LIVE,
  expiring: AGENTS.MODEL_ACCESS_EXPIRING,
  expired_signin: AGENTS.MODEL_ACCESS_EXPIRED,
  not_configured: AGENTS.MODEL_ACCESS_NOT_CONFIGURED,
  shared_expired: AGENTS.MODEL_ACCESS_SHARED_EXPIRED,
  not_applicable: AGENTS.MODEL_ACCESS_NOT_APPLICABLE,
};

// The same six labels WITHOUT the "Model access · " qualifier, for a chip
// rendered INSIDE the "Your model key" card (U-15): the card's own heading
// already says which credential is being described, so the qualifier read as a
// second subject — "Model access · Your admin's credential expired" under a
// heading that says "Your model key". Derived from the ONE table above rather
// than typed again: a canon edit to a label moves both surfaces at once. A miss
// is still undefined — no chip, never a default label.
const MODEL_ACCESS_CHIP_QUALIFIER = "Model access · ";
export function modelAccessChipBare(state: string): string | undefined {
  const label = MODEL_ACCESS_CHIP_LABEL[state];
  return label?.startsWith(MODEL_ACCESS_CHIP_QUALIFIER)
    ? label.slice(MODEL_ACCESS_CHIP_QUALIFIER.length)
    : label;
}

// The server's action line as the READER's clock renders it — the one place the
// console re-composes a server sentence, and only this one.
//
// `expiring`'s action carries an RFC3339 UTC stamp ("Sign in again before
// 2026-09-19T14:03:22Z", internal/api/modelaccess.go), which a person in
// another timezone misreads — and since 0.7.6 that state rides a banner on
// every screen for its whole 24-hour window. The template re-composed here is
// the SAME frozen §7.7 string the server formats, so only the instant changes.
// Every other state, and an `expiring` from a daemon too old to send
// `deadline`, renders verbatim.
//
// It lives HERE, beside the template, rather than in lib/model-access.ts: this
// module carries the whole AGENTS table, and lib/model-access.ts is imported by
// the shell's eager graph — one reference to AGENTS from there put 16 kB of copy
// into the entry chunk (bundle-split.test.ts). Its two callers are both lazy
// screens.
export function modelAccessActionLine(
  access: { state?: string; action?: string; deadline?: string } | undefined | null,
): string {
  if (!access?.action) return "";
  if (access.state === "expiring" && access.deadline) {
    return AGENTS.MODEL_ACCESS_EXPIRING_ACTION(absoluteTime(access.deadline));
  }
  return access.action;
}


// R-01 (fix-console-u review): the "is this harness row a per-person AWS SSO
// lane" predicate — `h.enabled !== false` (not truthiness) is load-bearing,
// not decorative: a DISABLED row still legally carries mechanism/
// credential_source (validateAgentCredentialSource never looks at Disabled),
// but the server's login predicate (perUserLoginRow) and its model_access
// scoping (awsSSOScopeFor) both treat a disabled row as NOT per_user —
// grading it in the operator's own namespace and rejecting an empty start
// URL with a 400 a card would otherwise hide the field for. Absent `enabled`
// reads as unknown, never false, so an older daemon that omits the field is
// unaffected. ONE predicate, not two independently-typed copies (the U-03
// recurrence this fixes): connection-cards.tsx's perUserSso reads the
// server's settled row, agents-tab.tsx's perUserSaved reads the same
// `harness` prop — same three-part test, same answer.

// AGENTS_DRAFT — new strings not yet in the frozen §7.7 table
// DRAFT (M2 canon pending): new strings this round, NOT part of the frozen
// §7.7 AGENTS table above — workspace-providers-copy.test.ts's byte-check
// parses only PROVIDERS/PROVIDER_MEMBER/AGENTS out of the doc, so a NEW
// export beside it (never inside it) is what keeps that gate meaningful.
// Canon rows staged for the M2 sitting land in
// docs/design/workspace-providers-prompt.md, the same doc §7.2-§7.5/§7.7
// above were transcribed from (the working sheet itself is gitignored
// campaign evidence, not a path this shipped file can point at).
export const AGENTS_DRAFT = {
  // The per_user sign-in banner (Appendix A finding 4): moves the
  // claude-code model-access block to the TOP of an expanded per_user row so
  // the legacy Settings door stops being the one an admin reaches for.
  PER_USER_SIGN_IN_TITLE: "This lane is per person — including yours",
  PER_USER_SIGN_IN_BODY:
    "Saving declares the lane; it signs nobody in, you included. Sign in to AWS below. Every member does the same from their own Getting Started.",
  // The roster pin (Appendix A finding 1, ask 1) — mirrors FIELD_SSO_START_URL /
  // SSO_START_URL_HINT's shape, one Field each.
  FIELD_SSO_ACCOUNT_ID: "Pinned AWS account id",
  SSO_ACCOUNT_ID_HINT:
    "The 12-digit account a sign-in for this row must resolve to. Set together with the role below, or leave both blank — the sign-in proposes, the roster disposes.",
  FIELD_SSO_ROLE_NAME: "Pinned IAM role name",
  SSO_ROLE_NAME_HINT:
    "The IAM role a sign-in for this row must resolve to. Set together with the account above, or leave both blank.",
  // F4-F9 (Appendix A V8): a per_user bedrock_sso row with no start URL is a
  // guaranteed 400 (agent_providers.go's validateAgentCredentialSource) — the
  // Git tab withholds Save for its own invalid rows; this is the same rule
  // said where the field is authored.
  SSO_START_URL_REQUIRED: "Required for a per-person lane — Save is disabled until this names a real https:// start URL.",
} as const;

// PROVIDERS_DRAFT — new strings not yet in the frozen §7.2 table
// DRAFT (M2 canon pending): new strings this round, NOT part of the frozen
// §7.2 PROVIDERS table — kept in a separate export for the same reason
// AGENTS_DRAFT is (the byte-check parses only PROVIDERS/PROVIDER_MEMBER/
// AGENTS out of the doc).
export const PROVIDERS_DRAFT = {
  // F4-F3 (Appendix A V8, corrected verdict): the keep-draft-mounted 412
  // banner's ONE control — discards the admin's own unsaved edits and reloads
  // the server's version. NO "Save over theirs" arm: a security document is
  // never last-writer-wins from this banner.
  DISCARD_AND_RELOAD: "Discard mine and reload",
  // F6-F6: PUT /site-config's dangling_secret_refs, surfaced in the
  // Corporate-network save toast — an ADDITIONAL warning beside whatever
  // success toast the saving step already shows, never a replacement for it.
  SAVED_DANGLING_REFS: (refs: string[]) =>
    `Saved, but ${refs.length === 1 ? "this secret isn't" : "these secrets aren't"} stored: ${refs.join(", ")}.`,
} as const;
