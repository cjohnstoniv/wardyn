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
        powerSource={{ kind: "default" }}
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
        powerSource={{ kind: "default" }}
        onOpenDetail={vi.fn()}
        onRescan={vi.fn()}
      />,
    );
    expect(screen.getByText(/1 required secret isn't stored yet/)).toBeInTheDocument();
    expect(screen.getByText(/2 suspected committed secrets/)).toBeInTheDocument();
  });

  it("the three strengthen cards deep-link with a focus hint, and Model access reflects a pinned power source", () => {
    const onOpenDetail = vi.fn();
    render(
      <StepDone
        name="payments-service"
        variant="usable"
        requirements={{}}
        storedSecretNames={[]}
        leakCount={0}
        powerSource={{ kind: "pinned", integrationId: "ai:anthropic_api_key", name: "Anthropic (API key)" }}
        onOpenDetail={onOpenDetail}
        onRescan={vi.fn()}
      />,
    );
    expect(screen.getByText(/Bound: Anthropic \(API key\)/)).toBeInTheDocument();
    fireEvent.click(screen.getByText("Record a session"));
    expect(onOpenDetail).toHaveBeenCalledWith("record");
    fireEvent.click(screen.getByText("Env as code"));
    expect(onOpenDetail).toHaveBeenCalledWith("env");
    fireEvent.click(screen.getByText("Model access"));
    expect(onOpenDetail).toHaveBeenCalledWith("model");
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
        powerSource={{ kind: "default" }}
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
        powerSource={{ kind: "none" }}
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
