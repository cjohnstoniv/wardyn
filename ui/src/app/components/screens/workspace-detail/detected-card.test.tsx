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

  // UI-WS-6: a host required only via the EFFECTIVE fold (e.g. a shared
  // library source's own contract), never restated in this workspace's own
  // overlay, used to still read "not in the contract" and get re-offered.
  it("a host required only via the fold (not this workspace's own overlay) is not re-suggested either", () => {
    const w = ws({ suggested_egress: ["registry.npmjs.org"] });
    (w as unknown as { requirements: Record<string, unknown>; effective_requirements: Record<string, unknown> }).requirements = {};
    (w as unknown as { effective_requirements: Record<string, unknown> }).effective_requirements = {
      "egress:registry.npmjs.org": { level: "required", provenance: "scan_seeded" },
    };
    render(<DetectedCard ws={w} onWorkspaceUpdated={vi.fn()} />);
    expect(screen.queryByTestId("detected-suggested-egress")).not.toBeInTheDocument();
  });
});

describe("DetectedCard — code/CI names promote directly (no egress confirm)", () => {
  // UI-WS-1: the raw env-var name is what the server rejects
  // (secretNameRE) — the promote key and the dedupe check must use the
  // STORABLE name (storableSecretName), matching the wizard's own Secrets tab.
  it("Add as optional calls setRequirements with the STORABLE name, not the raw env-var name", async () => {
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
    // The row's face is still the scan's honest, raw fact.
    expect(within(group).getByText("SENTRY_DSN")).toBeInTheDocument();
    expect(within(group).getByText(/stored as/i)).toBeInTheDocument();
    await user.click(within(group).getByRole("button", { name: /add as optional/i }));
    await waitFor(() =>
      expect(setRequirementsMock).toHaveBeenCalledWith("ws-1", {
        "secret:sentry-dsn": { level: "optional", provenance: "operator_set" },
      }),
    );
    // Direct — no confirm dialog in the way.
    expect(screen.queryByText(/untrusted content/i)).not.toBeInTheDocument();
  });

  it("a name with nothing storable (storableSecretName -> '') is skipped, not offered to promote", async () => {
    render(
      <DetectedCard
        ws={ws({ required_secrets: [{ name: "___", kind: "code" }] })}
        onWorkspaceUpdated={vi.fn()}
      />,
    );
    expect(screen.queryByTestId("detected-code-refs")).not.toBeInTheDocument();
    expect(await screen.findByTestId("detected-empty")).toBeInTheDocument();
  });

  // UI-WS-6: a secret already required via the fold (a shared source's own
  // contract) must not still read "advisory — referenced in code, not
  // declared" just because this workspace's own overlay is empty.
  it("a secret already required via the fold is not offered as an advisory code ref", () => {
    const w = ws({ required_secrets: [{ name: "STRIPE_KEY", kind: "code" }] });
    (w as unknown as { requirements: Record<string, unknown> }).requirements = {};
    (w as unknown as { effective_requirements: Record<string, unknown> }).effective_requirements = {
      "secret:stripe-key": { level: "optional", provenance: "scan_seeded" },
    };
    render(<DetectedCard ws={w} onWorkspaceUpdated={vi.fn()} />);
    expect(screen.queryByTestId("detected-code-refs")).not.toBeInTheDocument();
  });
});

describe("DetectedCard — empty state", () => {
  it("shows the nothing-pending line when the scan found nothing to promote", async () => {
    render(<DetectedCard ws={ws({})} onWorkspaceUpdated={vi.fn()} />);
    expect(await screen.findByTestId("detected-empty")).toBeInTheDocument();
  });

  // ui-wsDetail-1: while the observed-egress GET is in flight, `observed` is
  // still null and must not be read as "no denied hosts" — a neutral
  // "checking" state renders instead of the completeness claim.
  it("does not claim 'nothing pending' while the observed-egress fetch is still in flight", async () => {
    let resolveFetch!: (v: { denied: string[]; runs_examined: number }) => void;
    getObservedEgressMock.mockReturnValue(new Promise((resolve) => (resolveFetch = resolve)));
    render(<DetectedCard ws={ws({})} onWorkspaceUpdated={vi.fn()} />);

    expect(screen.getByTestId("detected-loading")).toBeInTheDocument();
    expect(screen.queryByTestId("detected-empty")).not.toBeInTheDocument();

    resolveFetch({ denied: [], runs_examined: 0 });
    expect(await screen.findByTestId("detected-empty")).toBeInTheDocument();
  });

  // ui-wsDetail-1: an unscanned workspace hasn't finished looking, so it
  // must not assert completeness either, even once the (necessarily empty)
  // observed-egress fetch settles.
  it("does not claim 'nothing pending' for a never-scanned workspace", async () => {
    render(<DetectedCard ws={ws({}, { status: "pending_scan" })} onWorkspaceUpdated={vi.fn()} />);
    expect(await screen.findByTestId("detected-unscanned")).toBeInTheDocument();
    expect(screen.queryByTestId("detected-empty")).not.toBeInTheDocument();
  });
});
