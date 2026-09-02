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
// refused server-side). Never the enforcement point — ValidateMemberMountSource
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
}

const NO_USER_DRIVE: UserDriveMeta = { drive: null, deniedByProfile: "" };
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

export function OperatorProvider({
  operator,
  securityOperator = true,
  principal = "",
  memberLocalDirRoot = null,
  userDrive = null,
  userDriveDeniedByProfile = "",
  children,
}: {
  operator: boolean;
  // Optional, defaulting TRUE: every existing caller that passes only
  // `operator` keeps today's fail-open behavior rather than silently becoming
  // the restricted case.
  securityOperator?: boolean;
  principal?: string;
  memberLocalDirRoot?: string | null;
  userDrive?: MeUserDrive | null;
  userDriveDeniedByProfile?: string;
  children: React.ReactNode;
}) {
  // Memoised: the two /me fields are a fresh object literal on every shell
  // render otherwise, which would re-render every drive consumer on each
  // heartbeat tick for a value that never changed.
  const drive = React.useMemo<UserDriveMeta>(
    () => ({ drive: userDrive ?? null, deniedByProfile: userDriveDeniedByProfile }),
    [userDrive, userDriveDeniedByProfile],
  );
  return (
    <OperatorContext.Provider value={operator}>
      <SecurityOperatorContext.Provider value={securityOperator}>
        <PrincipalContext.Provider value={principal}>
          <MemberLocalDirRootContext.Provider value={memberLocalDirRoot}>
            <UserDriveContext.Provider value={drive}>{children}</UserDriveContext.Provider>
          </MemberLocalDirRootContext.Provider>
        </PrincipalContext.Provider>
      </SecurityOperatorContext.Provider>
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

// The B1/B2-derived Wardyn role (GET /api/v1/me's `role`) — "admin",
// "security_admin" or "member". `operator` above stays the legacy boolean every
// existing gate reads; Role is additive, for UX that needs the named tier
// itself (nav filtering, the account-menu chip) rather than a yes/no.
//
// NOTE the two booleans are no longer complementary now that there are three
// values: member === !operator is FALSE for a security admin. Gate on the
// predicate that matches the surface (useOperator / useSecurityOperator), never
// on a role comparison of your own.
export type Role = "admin" | "security_admin" | "member";

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
