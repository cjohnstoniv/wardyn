/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { health as api, type Me, type MeUserDrive } from "../../lib/api/health";
import { getAuthGeneration } from "../../lib/api/core";
import type { Role, UserTypeMeta } from "../wardyn/operator-context";
import type { ViewUserType } from "../wardyn/view-switch";

// useMeta fetches the real trust boundary (/healthz) + signed-in principal
// (/api/v1/me) so the shell never shows placeholder identity/tenant values.
export interface ShellMeta {
  trustDomain: string;
  identityProvider: string;
  // principal is the OWNERSHIP key (the OIDC sub, "admin-token", "local:…"):
  // it feeds MeIdentity.principal and every `usePrincipal() === run.created_by`
  // gate. email and name are DISPLAY ONLY — what the header shows for "you",
  // in that order of preference — and are "" outside SSO or when the IdP sent
  // none, so the header falls back to the principal there (0.7.1).
  principal: string;
  identityRevision: number;
  authGeneration: number;
  email: string;
  name: string;
  method: string;
  // True once the /me fetch has SETTLED — success or failure. This is the
  // landing gate's signal (App.tsx's FirstRunLanding), and it must not be
  // derived from `method` ("" after a failed /me) or a failed fetch strands
  // "/" on a spinner forever; the role it then reads is the fail-open admin.
  resolved: boolean;
  // True only when /me actually ANSWERED (a body came back), which `resolved`
  // above deliberately does not say — whoami() swallows every failure and
  // returns null, so `resolved` flips on a FAILED fetch too. Anything that
  // picks a lane off `operator` rather than merely offering a control needs
  // this one instead (R4-F110; see operator-context.tsx's
  // MeIdentity.operatorResolved for the attach-WS case that named it).
  identityResolved: boolean;
  // Fail-open (see operator-context.tsx): starts true and stays true unless
  // /me resolves and explicitly says otherwise — an unresolved or failed
  // fetch must never read as "viewer".
  operator: boolean;
  // The SECOND server predicate (admin OR security_admin — /me's
  // `security_operator`), gating the security-governance surfaces. Same
  // fail-open default as `operator`, and deliberately a separate field: with
  // three role values the two booleans are not complements of each other.
  securityOperator: boolean;
  // #1335 — /me's `user_view_super_admin`: the person inside the User view is
  // stamped a super admin, the one bit that says /admin/setup would open for
  // them while `operator` reads false. Fail CLOSED (false) when /me has not
  // answered or an older daemon never sends it — it only gates a link.
  userViewSuperAdmin: boolean;
  // The same B1-derived tier as `operator`, named directly (B3) — fail-open
  // "admin" for the identical three cases (unresolved /me, a failed fetch, an
  // unwrapped test). Kept alongside `operator` rather than replacing it: every
  // existing operator-only gate stays exactly as it was.
  role: Role;
  // When the SSO session dies outright (no refresh) — null for
  // local/token auth, which has no session to expire.
  sessionExpiresAt: Date | null;
  // M3 — see operator-context.tsx's MeIdentity.memberLocalDirRoot. null until /me
  // resolves and stays null (fail-closed: unavailable) if it never does.
  memberLocalDirRoot: string | null;
  // 0.7 — the caller's own allocation and the profile door beside it, the same
  // /me body every other field here comes from. New Run and the member Getting
  // Started page read them off the context rather than issuing a second and a
  // third GET /me of their own; same fail-closed default as memberLocalDirRoot.
  // The trade that buys is FRESHNESS PER PAGE LOAD, not per navigation — a
  // member paused mid-session keeps the offer until they reload and learns at
  // launch, which is the direction of error this feature can afford (see
  // operator-context.tsx's UserDriveMeta for the whole argument).
  userDrive: MeUserDrive | null;
  userDriveDeniedByProfile: string;
  // R4/F091 — the THIRD drive key: WHY /me could not answer, "" when it could.
  // The server always sends it on 0.7 and suppresses the allocation alongside
  // it; without it four distinguishable answers reach the member as the single
  // one whose remedy is wrong for all of them (operator-context.tsx's
  // UserDriveMeta.unavailable carries the whole argument). Same fail-closed
  // default as the two above: an unresolved or failed /me reads as "" —
  // nothing is claimed about a drive that is also null.
  userDriveUnavailable: string;
  // 0.8 (UT-7a) — the caller's own user type, the same /me body every other
  // field here comes from (operator-context.tsx's MeIdentity.userType).
  // Absent reads as null.
  userType?: UserTypeMeta | null;
  /** #912 — the type the switch's dropdown preselects before the view is
   *  entered (/me's user_view_preselect_type). "" means nothing to preselect. */
  userViewPreselectType: string;
  /** #912 (UT-13) — the type whose deletion turned this session's user view
   *  off, until the next switch (/me's user_view_dropped). null otherwise. */
  userViewDropped: { user_type: string; user_type_name?: string } | null;
  /** #912 (H2) — /me's user_view_types: the org's user types, present only
   *  for a caller whose STAMPED role is admin or security_admin (never a
   *  real member). This is the switch/eyebrow/notice's ONLY source for the
   *  type list — it must never call GET /user-types itself, which a clamped
   *  in-view session cannot reach (securityOps). Empty for anyone who never
   *  renders a picker. */
  userViewTypes: ViewUserType[];
  /** 0.7.4 "view as member" — an admin whose role is paused for this session. */
  memberMode: boolean;
  /** 0.7.5 — WHICH posture of that mode: the no-credential preview, in which
   *  this admin's own model credential reads as not signed in. Implies
   *  memberMode, so only the banner's wording changes on it. */
  memberModeNoCredential: boolean;
  /** 0.7.5 — whether this deployment's roster makes the no-credential preview
   *  mean anything. False hides the second account-menu entry entirely. */
  memberPreviewAvailable: boolean;
  /** #162 — /healthz's `runner` ("docker" / "k8s" / "" on a pre-mount default
   *  or an older daemon) and `network_policy` ("enforced" / "acknowledged" /
   *  "unenforced", absent as ""). Neither is read directly by a screen — both
   *  feed resolveConfinementPosture (confinement-posture.tsx), which is the
   *  only place that may tell "not applicable" (Docker) from "could not
   *  confirm" (a k8s daemon that omitted the verdict) apart. */
  runner: string;
  networkPolicy: string;
  /** #510-F6 — /healthz's `demo_video_base_url`, read once here rather than
   *  once per episode card (use-demo-video-base-url.ts's old per-hook fetch
   *  issued 24 requests on one Getting Started mount). undefined means "no
   *  mirror configured"; lib/demo-videos.ts's episodeUrl falls back to its
   *  own hardcoded GitHub base either way. */
  demoVideoBaseUrl?: string;
  /** /healthz's `sso`: OIDC is configured. With it, the admin token is not a
   *  person and has no User view (console-view.tsx#viewAccess). */
  sso: boolean;
}

/** Everything the shell holds that /me decides — `me` null is a failed /me.
 *  One mapping for the mount read and adopt(): a partial copy on adopt kept the
 *  placeholder principal after a resume, so every later lapse in the tab
 *  skipped the different-person check (#483, Q457-12). */
function identityFromMe(me: Me | null) {
  return {
    principal: me?.principal || "unknown",
    email: me?.email ?? "",
    // ?? "": a pre-0.7.1 daemon never sends name — absent must read as
    // "none", which falls back to the email, then the principal.
    name: me?.name ?? "",
    method: me?.method || "",
    identityResolved: me !== null,
    operator: me?.operator ?? true,
    // ?? true, not `?? me?.operator`: an older daemon that never sends
    // this field must fail OPEN like every other identity signal here.
    securityOperator: me?.security_operator ?? true,
    userViewSuperAdmin: me?.user_view_super_admin ?? false,
    role: me?.role ?? "admin",
    // F3-F11: a bad string parses to an Invalid Date, not null — guard
    // NaN here so useSessionExpiry never has to.
    sessionExpiresAt: validExpiry(me?.session_expires_at),
    memberLocalDirRoot: me?.member_local_dir_root ?? null,
    userDrive: me?.user_drive ?? null,
    userDriveDeniedByProfile: me?.user_drive_denied_by_profile ?? "",
    userDriveUnavailable: me?.user_drive_unavailable ?? "",
    userType: me?.user_type ?? null,
    userViewPreselectType: me?.user_view_preselect_type ?? "",
    userViewDropped: me?.user_view_dropped ?? null,
    userViewTypes: me?.user_view_types ?? [],
    memberMode: me?.user_view ?? false,
    memberModeNoCredential: me?.user_view_no_credential ?? false,
    memberPreviewAvailable: me?.user_preview_available ?? false,
  } satisfies Partial<ShellMeta>;
}

/** The shell's identity, the retry that re-fires /me (B1's banner action), and
 *  adopt() for a /me the reauth dialog already read (#483). */
export function useMeta(): [ShellMeta, () => void, (me: Me) => void] {
  // Bumped by retry(), which is the effect's only other dependency: /me is
  // fetched once per load today, so after a failure identityResolved would stay
  // false forever and the banner below would have nothing to offer.
  const [attempt, setAttempt] = React.useState(0);
  const readGeneration = React.useRef(0);
  const [meta, setMeta] = React.useState<ShellMeta>({
    trustDomain: "…",
    identityProvider: "…",
    principal: "…",
    identityRevision: 0,
    authGeneration: getAuthGeneration(),
    email: "",
    name: "",
    method: "",
    resolved: false,
    identityResolved: false,
    operator: true,
    securityOperator: true,
    userViewSuperAdmin: false,
    role: "admin",
    sessionExpiresAt: null,
    memberLocalDirRoot: null,
    userDrive: null,
    userDriveDeniedByProfile: "",
    userDriveUnavailable: "",
    userType: null,
    userViewPreselectType: "",
    userViewDropped: null,
    userViewTypes: [],
    memberMode: false,
    memberModeNoCredential: false,
    memberPreviewAvailable: false,
    runner: "",
    networkPolicy: "",
    demoVideoBaseUrl: undefined,
    sso: false,
  });
  React.useEffect(() => {
    let alive = true;
    const generation = ++readGeneration.current;
    const authGeneration = getAuthGeneration();
    Promise.all([api.health(), api.whoami()])
      .then(([h, me]) => {
        if (!alive || generation !== readGeneration.current || authGeneration !== getAuthGeneration()) return;
        setMeta((previous) => ({
          trustDomain: h.trust_domain || "unknown",
          identityProvider: h.identity_provider || "unknown",
          ...identityFromMe(me),
          identityRevision: previous.identityRevision + 1,
          authGeneration,
          resolved: true,
          runner: h.runner ?? "",
          networkPolicy: h.network_policy ?? "",
          // "" (unset) reads the same as absent: both mean "no mirror".
          demoVideoBaseUrl: h.demo_video_base_url || undefined,
          sso: h.sso ?? false,
        }));
      })
      .catch(() => {
        // health()/whoami() swallow their own errors today, so this is unreachable;
        // it exists so `resolved` never depends on two other functions keeping
        // that promise — a rejection here would strand the landing gate.
        if (alive && generation === readGeneration.current && authGeneration === getAuthGeneration()) setMeta((m) => ({ ...m, resolved: true }));
      });
    return () => {
      alive = false;
    };
  }, [attempt]);
  // The same person signed back in over the page: take the whole identity
  // from the /me the dialog read, rather than re-asking — a failed re-ask
  // would settle as an unknown identity and blank the page it just kept.
  const adopt = React.useCallback((me: Me) => {
    readGeneration.current++;
    // A batched render cannot stamp this answer as confirming a later auth change.
    const authGeneration = getAuthGeneration();
    setMeta((m) => ({ ...m, ...identityFromMe(me), resolved: true, identityRevision: m.identityRevision + 1, authGeneration }));
  }, []);
  return [meta, React.useCallback(() => setAttempt((n) => n + 1), []), adopt];
}

function validExpiry(iso: string | null | undefined): Date | null {
  if (!iso) return null;
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? null : d;
}
