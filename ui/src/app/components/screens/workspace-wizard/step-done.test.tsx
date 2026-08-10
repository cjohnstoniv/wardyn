/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { StepDone } from "./step-done";
import type { WorkspaceRequirementsMap } from "./wizard-types";

const REQS: WorkspaceRequirementsMap = {
  "secret:DATABASE_URL": { level: "required", provenance: "scan_seeded" },
  "secret:REDIS_URL": { level: "optional", provenance: "scan_seeded" },
  "egress:registry.npmjs.org": { level: "required", provenance: "scan_seeded" },
};

describe("StepDone — usable (happy path)", () => {
  it("shows the usable headline, the Always/On request summary, and no warnings when everything's stored", () => {
    render(
      <StepDone
        name="payments-service"
        variant="usable"
        requirements={REQS}
        storedSecretNames={["DATABASE_URL"]}
        leakCount={0}
        onOpenDetail={vi.fn()}
        onRescan={vi.fn()}
      />,
    );
    expect(screen.getByText("payments-service is usable.")).toBeInTheDocument();
    expect(screen.getByText("Runs can attach it now.")).toBeInTheDocument();
    // One required secret + one required host land in "Always"; the optional
    // secret lands in "On request" — distinct lines, not a combined count.
    expect(screen.getByText("Always:").closest("p")).toHaveTextContent("Always: 1 secret · 1 host");
    expect(screen.getByText("On request:").closest("p")).toHaveTextContent("On request: 1 secret");
    expect(screen.queryByText(/isn't stored yet/)).not.toBeInTheDocument();
  });

  it("carries forward the unmet-required and leak lines when they apply", () => {
    render(
      <StepDone
        name="payments-service"
        variant="usable"
        requirements={REQS}
        storedSecretNames={[]}
        leakCount={2}
        onOpenDetail={vi.fn()}
        onRescan={vi.fn()}
      />,
    );
    expect(screen.getByText(/1 required secret isn't stored yet/)).toBeInTheDocument();
    expect(screen.getByText(/2 suspected committed secrets/)).toBeInTheDocument();
  });

  it("offers exactly the two hardening cards — model access is not one of them — and both just open the workspace's page (no focus target exists to jump to)", () => {
    const onOpenDetail = vi.fn();
    render(
      <StepDone
        name="payments-service"
        variant="usable"
        requirements={{}}
        storedSecretNames={[]}
        leakCount={0}
        onOpenDetail={onOpenDetail}
        onRescan={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByText("Verify with a session"));
    fireEvent.click(screen.getByText("Env as code"));
    // The EFFECT, not a dropped argument: both cards independently reach
    // onOpenDetail (2 calls), and neither passes anything — workspace-detail
    // has no focus/query-param handling to receive an argument, so the prior
    // per-card "focus" (fed straight into onOpenWorkspace's
    // (workspaceId) => void and silently discarded) was removed rather than
    // asserted on.
    expect(onOpenDetail.mock.calls).toEqual([[], []]);
    // Binding a provider is configuration, not hardening: it lives on step ③
    // Integrations and the workspace's own page, and access resolves down a
    // four-tier ladder, so most workspaces never pin anything.
    expect(screen.queryByText("Model access")).not.toBeInTheDocument();
  });
});

describe("StepDone — scan-still-running variant", () => {
  it("shows the running note instead of a contract summary or warnings", () => {
    render(
      <StepDone
        name="payments-service"
        variant="scanning"
        requirements={REQS}
        storedSecretNames={[]}
        leakCount={1}
        onOpenDetail={vi.fn()}
        onRescan={vi.fn()}
      />,
    );
    expect(screen.getByText("payments-service is usable.")).toBeInTheDocument();
    expect(screen.getByText(/scan is still running/)).toBeInTheDocument();
    expect(screen.queryByText(/Always:/)).not.toBeInTheDocument();
    expect(screen.queryByText(/isn't stored yet/)).not.toBeInTheDocument();
  });
});

describe("StepDone — scan-failed variant", () => {
  it("shows the failed headline, no-contract note, and a working Rescan action", () => {
    const onRescan = vi.fn();
    render(
      <StepDone
        name="payments-api"
        variant="failed"
        requirements={{}}
        storedSecretNames={[]}
        leakCount={0}
        onOpenDetail={vi.fn()}
        onRescan={onRescan}
      />,
    );
    expect(screen.getByText("payments-api exists — its scan failed.")).toBeInTheDocument();
    expect(screen.getByText("Runs can attach it.")).toBeInTheDocument();
    expect(screen.getByText("No contract yet — nothing will be attached automatically.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Rescan on its page →" }));
    expect(onRescan).toHaveBeenCalled();
  });
});
