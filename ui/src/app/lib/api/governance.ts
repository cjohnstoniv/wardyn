/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Governance profiles (0.7) — the seven securityOps routes behind the
// Governance screen. Mirrors internal/api/governance.go; every route is under
// /api/v1 via wfetch.
//
// The wire types live HERE rather than in lib/types/ because this is a NEW
// domain with exactly one consumer (the Governance screen) — a second module
// holding four interfaces nothing else imports is a file to keep in sync for no
// reader. Move them to lib/types/governance.ts the day a second domain needs
// them.
import type { CapabilitySubjectType, ConfinementClass, RunPolicySpec } from "../types";
import { asJson, errText, HttpError, unwrapList, wfetch } from "./core";

// types.GovernanceLimits. ALL are `omitempty` on the wire, so an unrestricted
// profile arrives with the keys absent — optional here for the same reason,
// and `!!limits.deny_x` is how every read is written.
export interface GovernanceLimits extends RunLimits {
  deny_task_mode_exec?: boolean;
  deny_interactive?: boolean;
  // types.GovernanceLimits.DenyUserDrive (0.7 user drives) — the door the
  // profile editor's third LimitRow writes. A run under this profile mounts no
  // user drive even when one is allocated to the person; denyMemberDrive's 403
  // is what enforces it, and this is only what the editor authors.
  deny_user_drive?: boolean;
  // 0/absent is unlimited (R4/F032). The editor's LimitNumberRow writes this.
  max_concurrent_runs?: number;
  // types.GovernanceLimits.MaxEphemeralDiskMiB (0.7.2) — the ephemeral scratch
  // ceiling. 0/absent is unlimited. A CLAMP, not a refusal: a run asking for
  // more is capped at dispatch and warned, never 403'd.
  max_ephemeral_disk_mib?: number;
  // types.GovernanceLimits.MaxDriveSizeMiB (0.7.2) — the per-principal user
  // drive ceiling, folded with the deployment's own in one min(). 0/absent is
  // unlimited.
  max_drive_size_mib?: number;
  // types.GovernanceLimits.AutonomyRubric (0.8, #77/#99) — maps a run's
  // posture to a permitted autonomy level. Absent means no rubric, same as
  // every field above: the Go side is a pointer so an unrestricted profile
  // still marshals `limits: {}`. Resolution/enforcement land in #97; this
  // mirror exists so the type is in step from the day the field appears.
  autonomy_rubric?: AutonomyRubric;
}

// types.RunLimits (long-holds rev 4 §2.2) — embedded in GovernanceLimits, so
// the seven keys sit flat on `limits`; a run carries the same set, captured at
// create (AgentRun.run_limits). Absent/0/false keeps today's behaviour: no end,
// the deployment's approval expiry as the wait, no idle pause.
export interface RunLimits {
  max_end_ahead_sec?: number;
  default_end_sec?: number;
  allow_no_end?: boolean;
  max_wait_sec?: number;
  default_wait_sec?: number;
  user_changes_limits?: boolean;
  pause_idle_after_sec?: number;
}

// types.AutonomyLevel — L0 (most supervised) through L3 (least). The codes
// stay internal; the console renders plain labels (#93), not spelled here yet.
export type AutonomyLevel = "L0" | "L1" | "L2" | "L3";

// types.AutonomyRubric — nine closed fields, three egress postures, three
// secret postures, three confinement classes, each absent (caps nothing) or
// one of the four levels. See internal/types/governance.go for what each
// posture means.
export interface AutonomyRubric {
  egress_open?: AutonomyLevel;
  egress_reviewed?: AutonomyLevel;
  egress_sealed?: AutonomyLevel;
  secrets_powerful?: AutonomyLevel;
  secrets_baseline?: AutonomyLevel;
  secrets_none?: AutonomyLevel;
  confinement_cc1?: AutonomyLevel;
  confinement_cc2?: AutonomyLevel;
  confinement_cc3?: AutonomyLevel;
}

// One of AutonomyRubric's nine own field names — what internal/composer/
// autonomy.go's applicableAutonomyCaps names a cap by, and what
// AutonomyResolution.BoundBy (below) carries. keyof, not a hand-typed union,
// so the two can never drift apart.
export type AutonomyRubricRowKey = keyof AutonomyRubric;

// applicableAutonomyCaps' own fixed field order (internal/composer/
// autonomy.go) — egress, then secrets, then confinement — the order
// FoldAutonomy lists tied causes in, so a rendered bound_by sentence reads the
// same on Review and at launch. Also the row order profile-rubric.tsx draws
// the editor's three groups in.
export const AUTONOMY_RUBRIC_ROW_KEYS: AutonomyRubricRowKey[] = [
  "egress_open",
  "egress_reviewed",
  "egress_sealed",
  "secrets_powerful",
  "secrets_baseline",
  "secrets_none",
  "confinement_cc1",
  "confinement_cc2",
  "confinement_cc3",
];

// AutonomyLevel's own weakest -> strongest ladder (internal/types/
// governance.go's AutonomyLevel.Rank()) — mirrors lib/types/runs.ts's
// CC_ORDER for the same reason: lib/ may not import from components/, so the
// rank order lives here beside the type it orders, and the display labels
// (AUTONOMY_META) live in components/wardyn/autonomy-meta.ts instead.
export const AUTONOMY_LEVEL_ORDER: AutonomyLevel[] = ["L0", "L1", "L2", "L3"];

// types.AutonomyEgressPosture / types.AutonomySecretsPosture — the two graded
// halves of a run's three-axis posture (internal/composer/autonomy.go).
export type AutonomyEgressPosture = "open" | "reviewed" | "sealed";
export type AutonomySecretsPosture = "powerful" | "baseline" | "none";

// types.AutonomyPosture — the three-axis shape #97's resolveRunAutonomy folds
// against a profile's AutonomyRubric.
export interface AutonomyPosture {
  egress: AutonomyEgressPosture;
  secrets: AutonomySecretsPosture;
  confinement: ConfinementClass;
}

// types.AutonomyResolution (0.8 #97/#93) — what resolveRunAutonomy decided for
// one run: the level, the posture that produced it, and every rubric row that
// bound the result.
//
// bound_by IS A LIST, not a string — a wire decision, not a rendering choice
// (#96's ruling on #93). The fold is a min() over three axes, so rows TIE at
// the resolved level routinely; naming only the first would send an admin to
// raise a row the level would not actually move on. governance-copy.ts's
// autonomyBoundSentence composes the FULL list into one sentence, never just
// bound_by[0].
export interface AutonomyResolution {
  level: AutonomyLevel;
  posture: AutonomyPosture;
  bound_by?: AutonomyRubricRowKey[];
}

// types.GovernanceProfile — one named, assignable ceiling.
export interface GovernanceProfile {
  id: string;
  name: string;
  ceiling: RunPolicySpec;
  limits: GovernanceLimits;
  created_at: string;
  updated_at: string;
  created_by?: string;
}

// types.GovernanceAssignment — one subject bound to one profile.
export interface GovernanceAssignment {
  id: string;
  subject_type: CapabilitySubjectType;
  subject: string;
  profile_id: string;
  priority: number;
  created_at: string;
  created_by?: string;
}

// GET /governance's body: the whole picture in one read.
export interface GovernanceSnapshot {
  profiles: GovernanceProfile[];
  assignments: GovernanceAssignment[];
}

// The POST/PUT body. id/created_at/updated_at/created_by are never sent — the
// server assigns provenance (governanceProfileRequest).
export interface GovernanceProfileInput {
  name: string;
  ceiling: RunPolicySpec;
  limits: GovernanceLimits;
}

// governanceProfileResponse: the saved row plus the OMISSION warnings the
// server composed. `warnings` is `omitempty` on the wire, so it is coerced to
// an array here and the screen never guards it.
//
// The console renders those strings VERBATIM, in response order — they are the
// server's own prose, more specific than any frozen sentence could be
// (governance-prompt.md §7.4).
export interface GovernanceProfileWrite {
  profile: GovernanceProfile;
  warnings: string[];
}

// The POST /governance/assignments body.
export interface GovernanceAssignmentInput {
  subject_type: CapabilitySubjectType;
  subject: string;
  profile_id: string;
  priority: number;
}

// The POST /governance/preview body — the two claim lists
// ResolveGovernanceProfile itself takes, so the server has nothing to derive.
//
// The console's claims field is kind-LESS (one textarea, the People step's own
// shape), so it sends every typed line in BOTH lists and the server offers each
// to both tiers. That is exactly what POST /access/preview already receives
// ({roles: lines, groups: lines}), and it is honest because the ANSWER names
// the tier that matched.
export interface GovernancePreviewInput {
  user_subjects: string[];
  groups: string[];
}

// One kind-LESS claims textarea onto the two typed wire lists both previews
// take — the governance one and the user-drive one, which post the identical
// body and are folded by the identical server-side normalizer.
//
// The ONE thing decided here is ORDER, never precedence. The user tier's
// tie-break is POSITION in user_subjects (the resolver's array_position) and
// the enforcement path builds it sign-in-subject first, email second
// (capabilitySubjects) — so an "@" line sorts last and a preview agrees with
// what actually binds the member. The drive resolver reads the same order
// positionally to pick which claim an `email_local` home is named from. Array
// .prototype.sort is stable, so every other line keeps the order it was typed
// in. The ranking itself stays in SQL.
export function previewClaims(claims: string): GovernancePreviewInput {
  const lines = claims
    .split("\n")
    .map((c) => c.trim())
    .filter(Boolean);
  return {
    user_subjects: [...lines].sort((a, b) => Number(a.includes("@")) - Number(b.includes("@"))),
    groups: lines,
  };
}

// governancePreviewResponse: which profile would bind a principal carrying
// those claims, and the tier of the assignment that won.
//
// EVERY field is `omitempty` on the wire, so an empty object is the answer for
// "no assignment matched" — the deployment ceiling. Same absent-key doctrine as
// DefaultPolicy's governance_profile_name; the two present fields ship
// together, so the screen tests `profile_name` and reads the tier beside it.
export interface GovernancePreview {
  profile_id?: string;
  profile_name?: string;
  matched_tier?: CapabilitySubjectType;
}

// The server composes a grant-bound refusal as "invalid ceiling: " + the
// comparator's own error, and EVERY leg of that comparator opens "eligible
// grant " (governanceGrantWithinCeiling, internal/api/governance_grantbound.go).
// A spec-VALIDATION 400 shares the "invalid ceiling: " prefix but never that
// clause, which is what makes this a safe discriminator: the console heads the
// grant-bound body with GRANT_BOUND_TITLE and renders every other 400 as the
// server's own message under no heading at all.
//
// A discriminator, not copy: it is matched against the wire, never rendered.
const GRANT_BOUND_PREFIX = "invalid ceiling: eligible grant ";

export function isGrantBoundError(e: unknown): boolean {
  return e instanceof HttpError && e.status === 400 && e.message.startsWith(GRANT_BOUND_PREFIX);
}

export const governance = {
  // GET /api/v1/governance -> {profiles, assignments}. Nil Go slices encode as
  // null, so both halves are coerced and every caller can map over them.
  async getGovernance(): Promise<GovernanceSnapshot> {
    const res = await wfetch("/governance", { method: "GET" });
    const body = await asJson<Partial<GovernanceSnapshot>>(res);
    return {
      profiles: unwrapList<GovernanceProfile>(body.profiles),
      assignments: unwrapList<GovernanceAssignment>(body.assignments),
    };
  },

  // POST /api/v1/governance/profiles -> 201 {profile, warnings}. A 400 whose
  // message opens "invalid ceiling: eligible grant " is the monotone-⊆ bound
  // refusing a profile that would MINT credential eligibility; every 4xx
  // surfaces as an HttpError carrying the server's own message.
  async createProfile(input: GovernanceProfileInput): Promise<GovernanceProfileWrite> {
    return writeProfile(await wfetch("/governance/profiles", { method: "POST", body: JSON.stringify(input) }));
  },

  // PUT /api/v1/governance/profiles/{id} -> 200 {profile, warnings}. Rename
  // included: ON DELETE RESTRICT makes delete-and-recreate impossible for an
  // assigned profile, so the update path has to carry the name.
  async updateProfile(id: string, input: GovernanceProfileInput): Promise<GovernanceProfileWrite> {
    return writeProfile(
      await wfetch(`/governance/profiles/${encodeURIComponent(id)}`, { method: "PUT", body: JSON.stringify(input) }),
    );
  },

  // DELETE /api/v1/governance/profiles/{id} -> 204.
  //
  // The 409 MUST reach the caller: it is the ON DELETE RESTRICT refusal, and it
  // is authoritative for the race the console's own assignment count cannot see
  // (another admin assigning the profile a second ago). A 404 is tolerated as
  // "already gone" — the row is absent either way, which is what was asked for.
  async deleteProfile(id: string): Promise<void> {
    const res = await wfetch(`/governance/profiles/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!res.ok && res.status !== 404) {
      throw new HttpError(res.status, await errText(res));
    }
  },

  // POST /api/v1/governance/assignments -> the stored row. Keyed on the natural
  // (subject_type, subject): re-assigning a subject REPOINTS its single row
  // (200) rather than accumulating a second (201). The console reloads either
  // way, so the status is not surfaced.
  async upsertAssignment(input: GovernanceAssignmentInput): Promise<GovernanceAssignment> {
    const res = await wfetch("/governance/assignments", { method: "POST", body: JSON.stringify(input) });
    return asJson<GovernanceAssignment>(res);
  },

  // DELETE /api/v1/governance/assignments/{id} -> 204. This is the SUPPORTED
  // way to widen a principal back to the deployment ceiling.
  async deleteAssignment(id: string): Promise<void> {
    const res = await wfetch(`/governance/assignments/${encodeURIComponent(id)}`, { method: "DELETE" });
    if (!res.ok && res.status !== 404) {
      throw new HttpError(res.status, await errText(res));
    }
  },

  // POST /api/v1/governance/preview -> 200, the resolved profile or {}.
  //
  // The dry run runs THE resolver server-side —
  // Store.ResolveGovernanceProfile, the same call the enforcement path makes on
  // every run. There is deliberately NO client-side precedence here: a second
  // implementation of the SQL ORDER BY precedence rule in TypeScript is a
  // second implementation of the answer. It saves nothing and enforces
  // nothing, but it must not be able to disagree with what actually binds a
  // member.
  async previewGovernance(input: GovernancePreviewInput): Promise<GovernancePreview> {
    const res = await wfetch("/governance/preview", { method: "POST", body: JSON.stringify(input) });
    return asJson<GovernancePreview>(res);
  },
};

async function writeProfile(res: Response): Promise<GovernanceProfileWrite> {
  const body = await asJson<GovernanceProfileWrite>(res);
  return { profile: body.profile, warnings: unwrapList<string>(body.warnings) };
}
