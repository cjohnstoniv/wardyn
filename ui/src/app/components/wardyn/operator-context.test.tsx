/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

import { OperatorProvider, useMemberLocalDirRoot, useOperator } from "./operator-context";

function Probe() {
  return <span>operator:{String(useOperator())}</span>;
}

function RootProbe() {
  return <span>root:{String(useMemberLocalDirRoot())}</span>;
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

  it("OperatorProvider(memberLocalDirRoot=...) threads the configured root through", () => {
    render(
      <OperatorProvider operator={false} memberLocalDirRoot="/home/agent-projects">
        <RootProbe />
      </OperatorProvider>,
    );
    expect(screen.getByText("root:/home/agent-projects")).toBeInTheDocument();
  });
});
