/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Wardyn boundary routes (internal/api/boundary.go), both securityOps. The PUT replaces the whole document,
// takes the GET's ETag as If-Match (a stale draft is a 412), and is held for a second person where one is
// required (202, thrown as PendingChangeError by asJson). The dry run writes nothing.
import { asJson, wfetch } from "./core";
import type { ConfinementClass } from "../types/runs";
import type { DesktopControls, GrantSpec } from "../types/policy";
import type { PolicyContact } from "../types/site";

export type BoundaryMode = "no_limits" | "hard_limits";

/** types.BoundarySpec: every knob is optional; an unset knob narrows nothing. */
export interface BoundarySpec {
  allowed_domains?: string[];
  denied_domains?: string[];
  allow_all_egress?: boolean;
  min_confinement_class?: ConfinementClass;
  eligible_grants?: GrantSpec[];
  // The desktop directions every run is held to (#2967); a direction left unset is its default.
  desktop?: DesktopControls;
}

export interface BoundaryLimits {
  max_end_ahead_sec?: number;
}

/** types.BoundaryWrite: the PUT body, and the part of the document a write replaces. */
export interface BoundaryWrite {
  mode: BoundaryMode;
  spec: BoundarySpec;
  limits: BoundaryLimits;
  contact?: PolicyContact;
  // The knobs no unit may change while the boundary is in hard_limits; absent keeps the stored locks.
  locked_knobs?: string[];
}

/** types.Boundary. */
export interface Boundary {
  mode: BoundaryMode;
  spec: BoundarySpec;
  limits: BoundaryLimits;
  contact?: PolicyContact;
  locked_knobs?: string[];
  updated_by?: string;
  updated_at?: string;
}

/** The document with the ETag the response carried, which the next PUT sends back as If-Match. */
export type BoundarySnapshot = Boundary & { etag?: string | null };

/** boundaryChange: one knob the drafted boundary narrows, as the read redaction shows it. */
export interface BoundaryChange {
  knob: string;
  before: unknown;
  after: unknown;
}

export interface BoundaryReportRow {
  id: string;
  name: string;
  changes: BoundaryChange[];
}

export interface BoundaryReportAssign {
  id: string;
  subject_type: string;
  subject: string;
  unit_id: string;
  unit_name: string;
  changes: BoundaryChange[];
}

/** boundaryReport: only the rows the drafted boundary would narrow are listed. */
export interface BoundaryReport {
  target: "boundary";
  deployment: BoundaryChange[];
  units: BoundaryReportRow[];
  stored_policies: BoundaryReportRow[];
  memberships: BoundaryReportAssign[];
}

const withEtag = async (res: Response): Promise<BoundarySnapshot> => ({ ...(await asJson<Boundary>(res)), etag: res.headers.get("ETag") });

export const boundary = {
  // GET /api/v1/governance/boundary. No row reads as no_limits.
  async get(): Promise<BoundarySnapshot> {
    return withEtag(await wfetch("/governance/boundary", { method: "GET" }));
  },

  // PUT /api/v1/governance/boundary: 200 -> the stored document, 202 -> PendingChangeError, 400/409/412 -> HttpError.
  async put(body: BoundaryWrite, etag?: string | null): Promise<BoundarySnapshot> {
    return withEtag(
      await wfetch("/governance/boundary", {
        method: "PUT", asPerson: true,
        headers: etag ? { "If-Match": etag } : {},
        body: JSON.stringify(body),
      }),
    );
  },

  // POST /api/v1/governance/dry-run: what the drafted spec and limits would narrow, judged as hard_limits.
  async dryRun(body: Pick<BoundaryWrite, "spec" | "limits">): Promise<BoundaryReport> {
    const res = await wfetch("/governance/dry-run", { method: "POST", asPerson: true, body: JSON.stringify(body) });
    const r = await asJson<Partial<BoundaryReport>>(res);
    return { target: "boundary", deployment: r.deployment ?? [], units: r.units ?? [], stored_policies: r.stored_policies ?? [], memberships: r.memberships ?? [] };
  },
};
