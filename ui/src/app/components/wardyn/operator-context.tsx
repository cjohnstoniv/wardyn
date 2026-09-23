/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import type { MeUserDrive } from "../../lib/api/health";

// Whether the signed-in caller holds the operator role (see GET /api/v1/me's
// `operator` field, sourced from the same isOperator predicate the server
// gates writes with — internal/api/http.go). Default TRUE: fail OPEN. That
// covers three cases identically —
//   - /me hasn't resolved yet (app-shell's useMeta seeds `operator: true`),
//   - the /me fetch failed (network blip, old daemon without the field),
//   - a component rendered with no <OperatorProvider> above it at all (every
//     existing test that mounts a screen directly, and any future one) —
// so a transient hiccup or an unwrapped test can never lock an operator out
// of their own console. Never "harden" this default to false.
const OperatorContext = React.createContext<boolean>(true);

// The signed-in principal (GET /api/v1/me's `principal`), for UX that needs
// to compare "is this MY run/resource" (e.g. the run-detail "Connect via
// SSH" card, owner-only like the gateway itself). Default "": empty reads as
// "not mine" everywhere it's compared, the fail-closed direction for an
// advisory UI check backed by a real server-side owner-only enforcement
// point (sshgateway.go's sshAuth) — same three-case rationale as
// OperatorContext above (unresolved /me, a failed fetch, an unwrapped test).
const PrincipalContext = React.createContext<string>("");

// M3 — presentational label of the WARDYN_MEMBER_WORKSPACE_ROOTS/_MAP
// constraint that applies to this signed-in member (GET /me's
// `member_local_dir_root`), e.g. "/home/agent-projects" (bare — the "under "
// word comes from permissions-copy.ts's ROOT_HINT template, not this value).
// null when no
// root applies (§DECISIONS O1: no per-member map entry AND the shared list
// is empty) — the fail-closed default (unresolved /me, a failed fetch, an
// unwrapped test all read as "no root", which shows AddWorkspaceDialog's
// local_dir-unavailable state rather than a path field that would just be
// refused server-side). Never the enforcement point — ValidateUserMountSource
// at bind time is (member-role-desktop.md §c).
const MemberLocalDirRootContext = React.createContext<string | null>(null);

// 0.7 — the caller's OWN user drive and the profile door beside it, both from
// GET /me (see lib/api/health.ts's MeUserDrive for why the door is a sibling
// field rather than a property of the allocation).
//
// They ride the shell's ONE /me read for the same reason
// MemberLocalDirRootContext does: New Run and the member Getting Started page
// each want them, app-shell's useMeta already holds the whole body, and two
// more GET /me round trips per navigation buy nothing the shell's read has not
// already bought.
//
// THE TRADE, named rather than assumed: one read per PAGE LOAD, not per
// navigation. Inside a mount the consumers cannot disagree — they read one
// object — but that object AGES. An admin who pauses a member's allocation
// mid-session leaves that member still looking at the checkbox until they
// reload, and the console never learns otherwise on its own. That is the cheap
// direction of the error and the reason it is accepted: the offer is UX, the
// resolver re-decides at dispatch, and the member is told by the door
// (DRIVE_MEMBER.REFUSED_PAUSED, user-drives-prompt.md §7.7) rather than by a
// checkbox that quietly disappeared. §2.6's member row is that list of states,
// launch refusal included.
//
// Fail-CLOSED, unlike the tier defaults above: null / "" is "no allocation and
// no door", which is exactly what an unresolved /me, a failed read and a
// pre-0.7 daemon all honestly mean. The server re-decides at launch either
// way, so an offer withheld here costs a member nothing but a page refresh,
// while an offer INVENTED here is a mount the launch would refuse.
export interface UserDriveMeta {
  drive: MeUserDrive | null;
  /** The governance profile's NAME when its DenyUserDrive limit refuses this
   *  caller a drive; "" when it does not. */
  deniedByProfile: string;
  /** WHY /me could not answer for this caller's drive; "" when it could.
   *
   *  R4/F091 — THE THIRD KEY, and the reason there are three rather than two
   *  (internal/api/me.go:126-137). `drive: null` alone means "you have no
   *  allocation", which is ADVICE ("ask an admin for one") — but it is also
   *  what a member gets when their group snapshot is stale, when their
   *  allocation could not name a directory, or when the store is down. The
   *  server ships the reason as a closed vocabulary beside the null
   *  (`groups_snapshot_stale` / `unmountable` / `unavailable` /
   *  `governance_unavailable`, user_drives_resolve.go) and suppresses the
   *  allocation alongside it; without reading that key, all four arrive as
   *  the one answer whose remedy is wrong for every one of them.
   *
   *  Carried here so a consumer CAN tell them apart. Non-empty means the drive
   *  affordance must not be offered — the server has not said the mount would
   *  work. RENDERING the per-reason remedy is a copy change and a mock round
   *  (CONSOLE-RULES §12); this is the plumbing it will read, and until it lands
   *  the consumers behave exactly as they do today, because `drive` is null in
   *  every one of these states anyway. */
  unavailable: string;
}

const NO_USER_DRIVE: UserDriveMeta = { drive: null, deniedByProfile: "", unavailable: "" };
const UserDriveContext = React.createContext<UserDriveMeta>(NO_USER_DRIVE);

// Whether the signed-in caller holds the SECURITY-governance tier — admin OR
// security_admin (GET /api/v1/me's `security_operator`, sourced from the same
// isSecurityOperator predicate the server gates the securityOps routes with).
// A SECOND context rather than a widened OperatorContext: the two tiers gate
// different surfaces (see useSecurityOperator below), and collapsing them would
// show a security admin controls the server then refuses.
//
// Default TRUE — the SAME fail-open rationale as OperatorContext above, for the
// same three cases (unresolved /me, a failed fetch, a component with no
// provider above it). Never "harden" this default to false either.
const SecurityOperatorContext = React.createContext<boolean>(true);

// Whether `operator` above is the SERVER'S answer yet, or still the fail-open
// default. The default itself is right and must not be hardened — but a caller
// that CHOOSES A LANE on it needs to know which of the two it is holding.
//
// The case that made this a context rather than a comment (R4-F110): whoami()
// swallows every failure and returns null (lib/api/health.ts), so one transient
// /me failure leaves `operator === true` and `principal === "unknown"` for the
// whole page load. attach-terminal picks its WebSocket auth lane off exactly
// those two, and for a MEMBER who owns the run both halves are wrong at once —
// `owned` is false (createdBy !== "unknown") and `operator` is true, so it takes
// the cookie lane, which is ticketOrHumanAuth's admin-only fall-through
// (internal/api/attach_ticket.go). The server refuses with 403 and writes an
// authz.denied/admin_surface audit row against the legitimate owner, five times
// over the reconnect budget — while the TICKET lane, which is owner-or-admin
// and would have worked, is never tried.
//
// Note this is NOT RoleResolvedContext: app-shell sets `resolved` once the /me
// fetch SETTLES, success or failure (it is the landing gate's "stop
// spinning" signal), so it is true after a failed /me too. This one is true only
// when a body actually came back.
//
// Default TRUE, the same fail-open rationale as every other default in this
// file: every component mounted with no provider above it (every existing test)
// must read as "the answer is known", not as "still loading forever".
const OperatorResolvedContext = React.createContext<boolean>(true);

export function OperatorProvider({
  operator,
  operatorResolved = true,
  securityOperator = true,
  principal = "",
  memberLocalDirRoot = null,
  userDrive = null,
  userDriveDeniedByProfile = "",
  userDriveUnavailable = "",
  confinementPosture = "",
  children,
}: {
  operator: boolean;
  /** Whether `operator` is the server's answer rather than the fail-open
   *  default — see OperatorResolvedContext. Optional, defaulting TRUE, so every
   *  existing caller keeps today's behaviour. */
  operatorResolved?: boolean;
  // Optional, defaulting TRUE: every existing caller that passes only
  // `operator` keeps today's fail-open behavior rather than silently becoming
  // the restricted case.
  securityOperator?: boolean;
  principal?: string;
  memberLocalDirRoot?: string | null;
  userDrive?: MeUserDrive | null;
  userDriveDeniedByProfile?: string;
  /** GET /me's `user_drive_unavailable` — see UserDriveMeta.unavailable.
   *  Defaults to "" ("nothing is wrong") so every existing caller that passes
   *  only the first two keeps today's behaviour. */
  userDriveUnavailable?: string;
  /** #162 — see ConfinementPostureContext above. Optional, defaulting "" (no
   *  posture reported), so every existing caller keeps today's silent
   *  behaviour. */
  confinementPosture?: ConfinementPosture;
  children: React.ReactNode;
}) {
  // Memoised: the two /me fields are a fresh object literal on every shell
  // render otherwise, which would re-render every drive consumer on each
  // heartbeat tick for a value that never changed.
  const drive = React.useMemo<UserDriveMeta>(
    () => ({
      drive: userDrive ?? null,
      deniedByProfile: userDriveDeniedByProfile,
      unavailable: userDriveUnavailable,
    }),
    [userDrive, userDriveDeniedByProfile, userDriveUnavailable],
  );
  return (
    <OperatorContext.Provider value={operator}>
      <OperatorResolvedContext.Provider value={operatorResolved}>
      <SecurityOperatorContext.Provider value={securityOperator}>
        <PrincipalContext.Provider value={principal}>
          <MemberLocalDirRootContext.Provider value={memberLocalDirRoot}>
            <UserDriveContext.Provider value={drive}>
              <ConfinementPostureContext.Provider value={confinementPosture}>
                {children}
              </ConfinementPostureContext.Provider>
            </UserDriveContext.Provider>
          </MemberLocalDirRootContext.Provider>
        </PrincipalContext.Provider>
      </SecurityOperatorContext.Provider>
      </OperatorResolvedContext.Provider>
    </OperatorContext.Provider>
  );
}

// The member's local_dir root constraint label — see MemberLocalDirRootContext above.
export function useMemberLocalDirRoot(): string | null {
  return React.useContext(MemberLocalDirRootContext);
}

// This caller's own drive and the profile door beside it — see
// UserDriveContext above. UX only: the launch path re-resolves both server-side
// and is what actually refuses or mounts.
export function useUserDrive(): UserDriveMeta {
  return React.useContext(UserDriveContext);
}

// Whether the signed-in caller may perform SUPER-admin-only actions (secret
// writes, policy/workspace CRUD, site-config, the managed harness credential,
// sandbox attach). UX only — never the enforcement point; the server's
// requireOperator middleware is what actually refuses a write.
//
// Approval DECISIONS and the audit/permissions/governance surfaces are NOT on
// this list since 0.7 — they moved to useSecurityOperator below.
export function useOperator(): boolean {
  return React.useContext(OperatorContext);
}

// Whether useOperator()'s answer came from the server — see
// OperatorResolvedContext. Use it only where the fail-open default would pick a
// WRONG LANE rather than merely offer a control the server will refuse.
export function useOperatorResolved(): boolean {
  return React.useContext(OperatorResolvedContext);
}

// Whether the signed-in caller may perform SECURITY-GOVERNANCE actions:
// decide/list approvals org-wide, read and verify the audit chain, write
// permissions/capability grants, author governance profiles. True for an admin
// too — the tiers overlap on this surface. UX only, never the enforcement
// point; the server's requireSecurityOperator middleware is what refuses.
//
// Use this, NOT useOperator, for those surfaces; use useOperator for the ones
// that stay super-admin (secret writes, the LLM credential, setup mutations,
// workspace writes, run attach). The split mirrors the server's two predicates
// exactly — see internal/api/http.go.
export function useSecurityOperator(): boolean {
  return React.useContext(SecurityOperatorContext);
}

// The signed-in principal — see PrincipalContext above.
export function usePrincipal(): string {
  return React.useContext(PrincipalContext);
}

// F5-F1 — the shared "may this caller mutate THIS row" predicate: an operator
// may always; a member may ONLY a row they own (ownedBy === principal).
// A correction over the naive `owned_by === principal` a first draft
// would reach for: an operator-created row carries owned_by:"" (see
// Workspace.OwnedBy's doc comment — empty means operator-owned, not
// "unowned"), and /me.principal is non-empty for a signed-in admin too, so
// without the `operator ||` half every admin would lose the ability to
// mutate the rows THEY created. `ownedBy` is optional so an operator-only
// surface (no ownership concept at all — policies, SCM hosts, credentials)
// can call this with just the caller's tier.
//
// Gated on useOperatorResolved(): with no real answer yet this reads FALSE —
// fail CLOSED — rather than trust OperatorContext's own fail-OPEN default.
// That default exists so an unresolved /me never locks an operator out of
// their own console; here the asymmetry runs the other way; a destructive
// control (delete) briefly showing disabled costs an admin one beat, while
// briefly showing ENABLED for a member whose ownership hasn't been confirmed
// yet is the wrong direction to fail in.
export function useCanMutate(ownedBy?: string): boolean {
  const operator = useOperator();
  const operatorResolved = useOperatorResolved();
  const principal = usePrincipal();
  return operatorResolved && (operator || (!!principal && ownedBy === principal));
}

// The B1/B2-derived Wardyn role (GET /api/v1/me's `role`) — "admin",
// "security_admin" or "user". `operator` above stays the legacy boolean every
// existing gate reads; Role is additive, for UX that needs the named tier
// itself (nav filtering, the account-menu chip) rather than a yes/no.
//
// NOTE the two booleans are no longer complementary now that there are three
// values: member === !operator is FALSE for a security admin. Gate on the
// predicate that matches the surface (useOperator / useSecurityOperator), never
// on a role comparison of your own.
export type Role = "admin" | "security_admin" | "user";

// Default "admin": the SAME fail-open rationale as OperatorContext above (an
// unresolved /me, a failed fetch, or a component mounted with no
// <RoleProvider> at all — every existing test — must never read as a
// restricted member).
const RoleContext = React.createContext<Role>("admin");

// Whether `role` above is the REAL, server-answered value yet, or still the
// fail-open default (app-shell's useMeta starts `role: "admin", method: ""`
// and flips only once whoami() resolves). Default true — the SAME fail-open
// rationale as Role's own default: every unwrapped test and every consumer
// that never passes this prop must read as resolved, not as "still loading
// forever". App.tsx's FirstRunLanding is the one consumer that actually
// blocks on it — it must not navigate a member on the admin default before
// the real role is known.
const RoleResolvedContext = React.createContext<boolean>(true);

export function RoleProvider({
  role,
  roleResolved = true,
  children,
}: {
  role: Role;
  roleResolved?: boolean;
  children: React.ReactNode;
}) {
  return (
    <RoleContext.Provider value={role}>
      <RoleResolvedContext.Provider value={roleResolved}>{children}</RoleResolvedContext.Provider>
    </RoleContext.Provider>
  );
}

export function useRole(): Role {
  return React.useContext(RoleContext);
}

export function useRoleResolved(): boolean {
  return React.useContext(RoleResolvedContext);
}

// #162 — the network-confinement posture ConfinementChip (primitives.tsx) and
// ConfinementPostureBanner (confinement-posture.tsx) both read. A context
// rather than a prop: ConfinementChip has a dozen call sites across the
// console, and threading one more prop through every one of them for a value
// that is the SAME for the whole page load is exactly the case a context
// exists for.
//
// "" (not "unknown") is the default and the Docker/not-applicable case both —
// see confinement-posture.tsx's resolveConfinementPosture for the runner +
// network_policy table this value comes from. Fail-quiet, unlike the
// fail-OPEN tiers above: an unresolved /me or a component mounted with no
// <OperatorProvider> (every existing test) must never invent a posture
// warning nothing has actually reported.
export type ConfinementPosture = "enforced" | "acknowledged" | "unenforced" | "unknown" | "";
export const ConfinementPostureContext = React.createContext<ConfinementPosture>("");

// The resolved posture — see ConfinementPostureContext above. A plain string,
// so passing it straight to the Provider's `value` is already the
// memoisation a dozen ConfinementChip consumers need: React compares a
// primitive context value with Object.is, which is a same-string check, not a
// same-object check, so a render that recomputes the identical posture never
// re-renders a consumer that already has it.
export function useConfinementPosture(): ConfinementPosture {
  return React.useContext(ConfinementPostureContext);
}
