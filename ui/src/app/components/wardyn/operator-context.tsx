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

export function OperatorProvider({
  operator,
  principal = "",
  children,
}: {
  operator: boolean;
  principal?: string;
  children: React.ReactNode;
}) {
  return (
    <OperatorContext.Provider value={operator}>
      <PrincipalContext.Provider value={principal}>{children}</PrincipalContext.Provider>
    </OperatorContext.Provider>
  );
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
