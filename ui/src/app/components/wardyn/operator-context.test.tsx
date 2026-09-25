/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, renderHook, screen } from "@testing-library/react";

import {
  OperatorProvider,
  useCanMutate,
  useConfinementPosture,
  useMemberLocalDirRoot,
  useOperator,
  useOperatorResolved,
  usePrincipal,
  useSecurityOperator,
  useUserDrive,
} from "./operator-context";

function Probe() {
  return <span>operator:{String(useOperator())}</span>;
}

function RootProbe() {
  return <span>root:{String(useMemberLocalDirRoot())}</span>;
}

function CanMutateProbe({ ownedBy }: { ownedBy?: string }) {
  return <span>canMutate:{String(useCanMutate(ownedBy))}</span>;
}

describe("operator-context", () => {
  // Fail-open baseline (see requireOperator in internal/api/http.go and the
  // module comment): unresolved /me, a failed fetch, and — as pinned here — a
  // component rendered with no <OperatorProvider> at all must all read as
  // operator. This is also what every one of the console's other ~600 tests
  // relies on implicitly: none of them wrap in a Provider, so they all keep
  // exercising today's (operator) behavior unchanged.
  it("defaults to operator with no <OperatorProvider> above it", () => {
    render(<Probe />);
    expect(screen.getByText("operator:true")).toBeInTheDocument();
  });

  // The unset-allowlist deployment (WARDYN_OIDC_OPERATOR_EMAILS unset => /me
  // returns operator:true for everyone) — behaviorally identical to the
  // default above, but pinned explicitly via the Provider this time.
  it("OperatorProvider(operator=true) reads as operator — the unset-list / today's-behavior case", () => {
    render(
      <OperatorProvider operator={true}>
        <Probe />
      </OperatorProvider>,
    );
    expect(screen.getByText("operator:true")).toBeInTheDocument();
  });

  // A real viewer: GET /me resolved operator:false.
  it("OperatorProvider(operator=false) reads as viewer", () => {
    render(
      <OperatorProvider operator={false}>
        <Probe />
      </OperatorProvider>,
    );
    expect(screen.getByText("operator:false")).toBeInTheDocument();
  });

  // M3: fail-closed default — no root applies unless a Provider explicitly
  // says so (unresolved /me, a failed fetch, an unwrapped test all show
  // AddWorkspaceDialog's local_dir-unavailable state, never a path field
  // that would just be refused server-side).
  it("useMemberLocalDirRoot defaults to null with no <OperatorProvider> above it", () => {
    render(<RootProbe />);
    expect(screen.getByText("root:null")).toBeInTheDocument();
  });

  // Every unwrapped default in one table: an unresolved /me, a failed read
  // and every test that mounts a screen directly read exactly these. The
  // operator tiers fail open; principal, root and drive fail closed; the
  // posture fails quiet.
  it.each([
    ["useOperator", useOperator, true],
    ["useOperatorResolved", useOperatorResolved, true],
    ["useSecurityOperator", useSecurityOperator, true],
    ["usePrincipal", usePrincipal, ""],
    ["useMemberLocalDirRoot", useMemberLocalDirRoot, null],
    ["useUserDrive", useUserDrive, { drive: null, deniedByProfile: "", unavailable: "" }],
    ["useConfinementPosture", useConfinementPosture, ""],
  ] as const)("%s reads its default with no <OperatorProvider> above it", (_name, hook, want) => {
    const { result } = renderHook(() => hook());
    expect(result.current).toStrictEqual(want);
  });

  it("OperatorProvider(memberLocalDirRoot=...) threads the configured root through", () => {
    render(
      <OperatorProvider operator={false} memberLocalDirRoot="/home/agent-projects">
        <RootProbe />
      </OperatorProvider>,
    );
    expect(screen.getByText("root:/home/agent-projects")).toBeInTheDocument();
  });
});

// F5-F1 / useCanMutate — the shared "may this caller mutate THIS row"
// predicate: an operator may always; a member may ONLY their own
// (ownedBy === principal). PREDICATE CORRECTION over a naive `owned_by ===
// principal`: an operator-created row carries owned_by:"" while /me.principal
// is non-empty for a signed-in admin too, so without the `operator ||` half
// every admin would lose Delete on the rows THEY created. Gated on
// useOperatorResolved(): with no real answer yet, this must fail CLOSED
// (return false) rather than trust the operator context's own fail-OPEN
// default — an admin briefly seeing a disabled Delete costs a beat; a member
// briefly seeing an ENABLED one is the wrong direction for a destructive
// control.
describe("useCanMutate — operator, or the member who owns this row", () => {
  it("defaults to true (operator + resolved) with no <OperatorProvider> above it", () => {
    render(<CanMutateProbe ownedBy="someone-else" />);
    expect(screen.getByText("canMutate:true")).toBeInTheDocument();
  });

  it("an operator may mutate any row, owned or not", () => {
    render(
      <OperatorProvider operator={true} principal="admin@corp">
        <CanMutateProbe ownedBy="member@corp" />
      </OperatorProvider>,
    );
    expect(screen.getByText("canMutate:true")).toBeInTheDocument();
  });

  it("a member may mutate a row THEY own (owned_by === principal)", () => {
    render(
      <OperatorProvider operator={false} principal="alice">
        <CanMutateProbe ownedBy="alice" />
      </OperatorProvider>,
    );
    expect(screen.getByText("canMutate:true")).toBeInTheDocument();
  });

  it("a member may NOT mutate someone else's row", () => {
    render(
      <OperatorProvider operator={false} principal="alice">
        <CanMutateProbe ownedBy="bob" />
      </OperatorProvider>,
    );
    expect(screen.getByText("canMutate:false")).toBeInTheDocument();
  });

  it("a member may NOT mutate an operator-owned row (owned_by:\"\") — not a flip", () => {
    render(
      <OperatorProvider operator={false} principal="alice">
        <CanMutateProbe ownedBy="" />
      </OperatorProvider>,
    );
    expect(screen.getByText("canMutate:false")).toBeInTheDocument();
  });

  it("with no ownedBy given (operator-only surfaces), a member reads false", () => {
    render(
      <OperatorProvider operator={false} principal="alice">
        <CanMutateProbe />
      </OperatorProvider>,
    );
    expect(screen.getByText("canMutate:false")).toBeInTheDocument();
  });

  it("unresolved /me (operatorResolved=false) reads false even under the fail-open operator default", () => {
    render(
      <OperatorProvider operator={true} operatorResolved={false} principal="alice">
        <CanMutateProbe ownedBy="alice" />
      </OperatorProvider>,
    );
    expect(screen.getByText("canMutate:false")).toBeInTheDocument();
  });
});
