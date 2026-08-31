/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Access / role-mapping wire types (0.7 SSO Phase 3) — mirror
// internal/api/access.go's response/request shapes exactly. The People step's
// role-mappings editor (setup/access-panel.tsx) is the sole consumer.

export type AccessRole = "admin" | "member";

// A single row of GET /access's merged table — a chart row (no id/created_*)
// or a console row. shadow_cause is "" unless shadowed is true.
export interface AccessMapping {
  id?: string;
  value: string;
  role: string;
  source: "chart" | "console";
  shadowed: boolean;
  shadow_cause: "" | "chart" | "operator_allowlist";
  created_at?: string;
  created_by?: string;
}

// The arm-1-vs-arm-2 outcome pair accessRolePosture (access.go) computes —
// used BOTH to render the Defaults block honestly and to pre-empt the
// posture-flip guard client-side before a write, per the doc's own note that
// GET /access "carries the same before/after ... so the console can render
// the identical warning inline instead of a bare message". `before`/`after`
// are noun/verb phrases per §7.3, not role constants — interpolate them
// verbatim into GUARD.FIRST_ROW_BODY / GUARD.LAST_ROW_BODY (swapped for the
// delete direction — see access-panel.tsx).
export interface AccessPosture {
  // REAL merged-map emptiness (chart + console rows, applying mergeRoleMaps'
  // own collision/shadow rules) — NOT a raw row count. Can be TRUE even with
  // console rows present (e.g. the only console row is entirely shadowed by
  // the operator allowlist and so contributes nothing to the merged map).
  // Never assume map_empty <=> mappings.filter(source==="console").length===0.
  map_empty: boolean;
  before: string;
  after: string;
  changes: boolean;
}

export interface AccessResponse {
  mappings: AccessMapping[];
  default_role: string;
  operator_emails_present: boolean;
  // The actual addresses (Config.OperatorEmails) — the Defaults block renders
  // these directly; operator_emails_present stays for the guard-note logic
  // that only needs presence.
  operator_emails: string[];
  allow_email_mappings: boolean;
  // Whether WARDYN_OIDC_EMAIL_DOMAINS is set — EMAIL_KEY_BODY's
  // email_verified clause only applies when this is false (that claim is
  // untrue once a domains list is configured).
  email_domains_configured: boolean;
  posture: AccessPosture;
}

// POST /access/mappings body. DELETE takes acknowledge_access_change as a
// QUERY param instead (access.go's handleDeleteRoleMapping reads
// r.URL.Query().Get, not a JSON body) — see api/access.ts's deleteMapping.
export interface RoleMappingWriteInput {
  value: string;
  role: string;
  acknowledge_access_change?: boolean;
}

// The structured 400 body BOTH posture-flip guards write (accessPostureFlipBody,
// access.go) when the write is missing acknowledge_access_change — carries the
// same before/after GET /access's posture already exposes, so a race against a
// stale client-side posture snapshot can still render the parameterized guard
// copy rather than a bare message.
export interface AccessPostureFlipBody {
  error: string;
  required_acknowledgement: boolean;
  before: string;
  after: string;
}

// The structured 400 body POST /access/mappings writes on a collision
// (accessCollisionBody, access.go) — cause is the SAME "chart" |
// "operator_allowlist" vocabulary accessMappingView.shadow_cause already
// uses, so the client keys the two frozen §7.4 strings off it directly
// instead of reconstructing which source collided.
export interface AccessCollisionBody {
  error: string;
  cause: "chart" | "operator_allowlist";
  value: string;
}

export interface AccessPreviewRequest {
  roles?: string[];
  groups?: string[];
  email?: string;
  use_session?: boolean;
}

export interface AccessPreviewMatch {
  value: string;
  role: string;
  source: string;
}

// error is set only to "role_check_unavailable" today (handlePreviewRole's
// one failure arm) — kept as a plain string, not a union, since the server
// doesn't document any other value and a future one should render rather than
// vanish.
export interface AccessPreviewResponse {
  role: string;
  ok: boolean;
  matched: AccessPreviewMatch[];
  error?: string;
}
