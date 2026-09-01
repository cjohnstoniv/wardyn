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
import type { CapabilitySubjectType, RunPolicySpec } from "../types";
import { asJson, errText, HttpError, unwrapList, wfetch } from "./core";

// types.GovernanceLimits. ALL are `omitempty` on the wire, so an unrestricted
// profile arrives with the keys absent — optional here for the same reason,
// and `!!limits.deny_x` is how every read is written.
export interface GovernanceLimits {
  deny_task_mode_exec?: boolean;
  deny_interactive?: boolean;
  // 0/absent is unlimited. Mirrored here so the editor's `{ ...limits }` spread
  // round-trips a cap it does not yet draw; the control itself lands with the
  // rest of the Governance UI.
  max_concurrent_runs?: number;
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
  // every run. There is deliberately NO client-side precedence here: this
  // screen used to re-read GET /governance and re-implement the SQL ORDER BY in
  // TypeScript, and a second implementation of the precedence rule is a second
  // implementation of the answer. It saves nothing and enforces nothing, but it
  // must not be able to disagree with what actually binds a member.
  async previewGovernance(input: GovernancePreviewInput): Promise<GovernancePreview> {
    const res = await wfetch("/governance/preview", { method: "POST", body: JSON.stringify(input) });
    return asJson<GovernancePreview>(res);
  },
};

async function writeProfile(res: Response): Promise<GovernanceProfileWrite> {
  const body = await asJson<GovernanceProfileWrite>(res);
  return { profile: body.profile, warnings: unwrapList<string>(body.warnings) };
}
