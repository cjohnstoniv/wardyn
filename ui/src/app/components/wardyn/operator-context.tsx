/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";

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

export function OperatorProvider({
  operator,
  principal = "",
  memberLocalDirRoot = null,
  children,
}: {
  operator: boolean;
  principal?: string;
  memberLocalDirRoot?: string | null;
  children: React.ReactNode;
}) {
  return (
    <OperatorContext.Provider value={operator}>
      <PrincipalContext.Provider value={principal}>
        <MemberLocalDirRootContext.Provider value={memberLocalDirRoot}>{children}</MemberLocalDirRootContext.Provider>
      </PrincipalContext.Provider>
    </OperatorContext.Provider>
  );
}

// The member's local_dir root constraint label — see MemberLocalDirRootContext above.
export function useMemberLocalDirRoot(): string | null {
  return React.useContext(MemberLocalDirRootContext);
}

// Whether the signed-in caller may perform operator-only actions (secret
// writes, policy/workspace CRUD, site-config, approval decisions, the managed
// harness credential, sandbox attach). UX only — never the enforcement point;
// the server's requireOperator middleware is what actually refuses a write.
export function useOperator(): boolean {
  return React.useContext(OperatorContext);
}

// The signed-in principal — see PrincipalContext above.
export function usePrincipal(): string {
  return React.useContext(PrincipalContext);
}

// The B1/B2-derived Wardyn role (GET /api/v1/me's `role`) — "admin" or
// "member". `operator` above stays the legacy boolean every existing gate
// reads (member === !operator); Role is additive, for UX that needs the named
// tier itself (nav filtering, the account-menu chip) rather than a yes/no.
export type Role = "admin" | "member";

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
