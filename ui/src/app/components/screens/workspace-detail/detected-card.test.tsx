/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Workspace, WorkspaceProfile } from "../../../lib/types";

const getObservedEgressMock = vi.fn();
const setRequirementsMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    getObservedEgress: (...a: unknown[]) => getObservedEgressMock(...a),
    setRequirements: (...a: unknown[]) => setRequirementsMock(...a),
  },
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn(), info: vi.fn() } }));

import { DetectedCard } from "./detected-card";

function ws(profile: WorkspaceProfile, over: Partial<Workspace> = {}): Workspace {
  return {
    id: "ws-1",
    name: "payments",
    kind: "repo",
    source: "acme/payments",
    status: "scanned",
    created_at: "",
    updated_at: "",
    profile: profile as unknown as Record<string, unknown>,
    ...over,
  };
}

beforeEach(() => {
  getObservedEgressMock.mockReset().mockResolvedValue({ denied: [], runs_examined: 0 });
  setRequirementsMock.mockReset();
});

describe("DetectedCard — leak findings pinned top, content-free, no promote action", () => {
  it("lists path:line — kind with no Approve/Add button anywhere in that block", () => {
    render(
      <DetectedCard
        ws={ws({ leak_findings: [{ path: "src/config.ts", kind: "aws-access-key", line: 12 }] })}
        onWorkspaceUpdated={vi.fn()}
      />,
    );
    const leaks = screen.getByTestId("leak-hot");
    expect(within(leaks).getByText("src/config.ts:12 — aws-access-key")).toBeInTheDocument();
    expect(within(leaks).queryByRole("button")).not.toBeInTheDocument();
    expect(within(leaks).getByText(/never shown or stored/i)).toBeInTheDocument();
  });
});

describe("DetectedCard — observed-but-denied is auto-loaded (no button click needed)", () => {
  it("fetches GET /workspaces/{id}/observed-egress on mount", async () => {
    getObservedEgressMock.mockResolvedValue({ denied: ["metrics.acme.io"], runs_examined: 5 });
    render(<DetectedCard ws={ws({})} onWorkspaceUpdated={vi.fn()} />);
    expect(getObservedEgressMock).toHaveBeenCalledWith("ws-1");
    expect(await screen.findByText("metrics.acme.io")).toBeInTheDocument();
    expect(screen.getByTestId("detected-observed-denied")).toHaveTextContent(/5 examined/);
  });

  it("excludes an already-approved denied host", async () => {
    getObservedEgressMock.mockResolvedValue({ denied: ["already.example.com"], runs_examined: 1 });
    render(<DetectedCard ws={ws({}, { approved_egress: ["already.example.com"] })} onWorkspaceUpdated={vi.fn()} />);
    await waitFor(() => expect(getObservedEgressMock).toHaveBeenCalled());
    expect(screen.queryByTestId("detected-observed-denied")).not.toBeInTheDocument();
    expect(screen.getByTestId("detected-empty")).toBeInTheDocument();
  });
});

describe("DetectedCard — suggested egress promotes behind the untrusted-content confirm", () => {
  it("Add as required confirms first, then PUTs the requirement", async () => {
    const onWorkspaceUpdated = vi.fn();
    const updated = ws({});
    setRequirementsMock.mockResolvedValue(updated);
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(<DetectedCard ws={ws({ suggested_egress: ["telemetry.acme.io"] })} onWorkspaceUpdated={onWorkspaceUpdated} />);

    const group = screen.getByTestId("detected-suggested-egress");
    await user.click(within(group).getByRole("button", { name: /add as required/i }));

    expect(await screen.findByText(/untrusted content/i)).toBeInTheDocument();
    expect(setRequirementsMock).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /approve host/i }));
    await waitFor(() =>
      expect(setRequirementsMock).toHaveBeenCalledWith("ws-1", {
        "egress:telemetry.acme.io": { level: "required", provenance: "operator_set" },
      }),
    );
    await waitFor(() => expect(onWorkspaceUpdated).toHaveBeenCalledWith(updated));
  });

  it("a host already in the requirements contract is not re-suggested", () => {
    const w = ws({ suggested_egress: ["registry.npmjs.org"] });
    (w as unknown as { requirements: Record<string, unknown> }).requirements = {
      "egress:registry.npmjs.org": { level: "required", provenance: "operator_set" },
    };
    render(<DetectedCard ws={w} onWorkspaceUpdated={vi.fn()} />);
    expect(screen.queryByTestId("detected-suggested-egress")).not.toBeInTheDocument();
  });
});

describe("DetectedCard — code/CI names promote directly (no egress confirm)", () => {
  it("Add as optional calls setRequirements immediately for a secret name", async () => {
    const onWorkspaceUpdated = vi.fn();
    setRequirementsMock.mockResolvedValue(ws({}));
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    render(
      <DetectedCard
        ws={ws({ required_secrets: [{ name: "SENTRY_DSN", kind: "code", optional: true }] })}
        onWorkspaceUpdated={onWorkspaceUpdated}
      />,
    );
    const group = screen.getByTestId("detected-code-refs");
    await user.click(within(group).getByRole("button", { name: /add as optional/i }));
    await waitFor(() =>
      expect(setRequirementsMock).toHaveBeenCalledWith("ws-1", {
        "secret:SENTRY_DSN": { level: "optional", provenance: "operator_set" },
      }),
    );
    // Direct — no confirm dialog in the way.
    expect(screen.queryByText(/untrusted content/i)).not.toBeInTheDocument();
  });
});

describe("DetectedCard — empty state", () => {
  it("shows the nothing-pending line when the scan found nothing to promote", async () => {
    render(<DetectedCard ws={ws({})} onWorkspaceUpdated={vi.fn()} />);
    expect(await screen.findByTestId("detected-empty")).toBeInTheDocument();
  });
});
