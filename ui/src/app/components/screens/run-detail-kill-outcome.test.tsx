/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #1487: what the run page says about a KILLED run, end to end through the
// page's own fetches — the latest run.kill row decides the outcome, Kill is
// offered again where the trail does not prove the teardown, and the per-kind
// lines come from the run's credential.mint rows joined to its grants (never
// from the capped general trail, and never a value). The copy itself is pinned
// in run-detail/failure-block.test.tsx.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { AuditEvent } from "../../lib/types";

const getRunMock = vi.fn();
const getGrantsMock = vi.fn();
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: (...a: unknown[]) => getRunMock(...a),
    getGrants: (...a: unknown[]) => getGrantsMock(...a),
    killRun: vi.fn(),
    getFiles: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getResources: vi.fn().mockRejectedValue(new Error("no runner in this test")),
    getAttachHolder: vi.fn().mockResolvedValue({ held: false }),
    takeoverAttach: vi.fn(),
  },
}));
vi.mock("../../lib/api/approvals", () => ({
  approvals: { listApprovals: vi.fn().mockResolvedValue([]), approve: vi.fn(), deny: vi.fn() },
}));
const listAuditMock = vi.fn();
vi.mock("../../lib/api/audit", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../lib/api/audit")>()),
  audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) },
}));
vi.mock("../../lib/api/recordings", () => ({ recordings: { getRecording: vi.fn().mockResolvedValue(null) } }));
vi.mock("../../lib/api/health", () => ({ health: { health: vi.fn().mockResolvedValue({}) } }));
vi.mock("sonner", () => ({ toast: { warning: vi.fn(), error: vi.fn(), success: vi.fn() } }));

import { RunDetailScreen } from "./run-detail";

const NOW = Date.now();
const RUN = {
  id: "run-1",
  created_at: new Date(NOW - 20 * 60_000).toISOString(),
  updated_at: new Date(NOW - 5 * 60_000).toISOString(),
  created_by: "me",
  agent: "claude-code",
  repo: "acme/widgets",
  task: "audit the egress proxy",
  confinement_class: "CC2",
  state: "KILLED",
  spiffe_id: "spiffe://wardyn.local/agent-run/run-1",
  runner_target: "docker",
  interactive: false,
};

const kill = (outcome: "success" | "failure", minutesAgo: number): AuditEvent => ({
  id: `k-${outcome}-${minutesAgo}`,
  time: new Date(NOW - minutesAgo * 60_000).toISOString(),
  actor_type: "human",
  actor: "sam@acme.io",
  action: "run.kill",
  outcome,
});
const mint = (grantID: string, extra: Record<string, unknown> = {}): AuditEvent => ({
  id: `m-${grantID}`,
  time: new Date(NOW - 10 * 60_000).toISOString(),
  actor_type: "agent",
  actor: "spiffe://x",
  action: "credential.mint",
  outcome: "success",
  data: { grant_id: grantID, ...extra },
});
const grantRecord = (id: string, kind: string, scope: Record<string, unknown> = {}) => ({
  id,
  created_at: new Date(NOW - 20 * 60_000).toISOString(),
  spec: { kind, scope },
});

// listAudit answers per action filter, as the server does.
function seed(opts: { kills?: AuditEvent[]; mints?: AuditEvent[] | Error; grants?: unknown[] | Error }) {
  getRunMock.mockResolvedValue(RUN);
  getGrantsMock.mockImplementation(async () => {
    if (opts.grants instanceof Error) throw opts.grants;
    // The real client maps records to CredentialGrant; this file mocks the
    // client, so hand back the mapped shape.
    return (opts.grants ?? []).map((g) => {
      const r = g as ReturnType<typeof grantRecord>;
      const scope = r.spec.scope as Record<string, unknown>;
      return { id: r.id, scope: r.spec.kind, audience: r.spec.kind, state: "active", host: scope.host };
    });
  });
  listAuditMock.mockImplementation(async (_id: string, o?: { action?: string }) => {
    if (o?.action === "run.kill") return opts.kills ?? [];
    if (o?.action === "credential.mint") {
      if (opts.mints instanceof Error) throw opts.mints;
      return opts.mints ?? [];
    }
    return [];
  });
}

function renderRun() {
  return render(
    <MemoryRouter initialEntries={["/runs/run-1"]}>
      <Routes>
        <Route path="/runs/:id" element={<RunDetailScreen />} />
      </Routes>
    </MemoryRouter>,
  );
}

const killButton = () => screen.getByRole("button", { name: /^Kill/ });

beforeEach(() => {
  vi.clearAllMocks();
  getRunMock.mockReset();
  getGrantsMock.mockReset();
  listAuditMock.mockReset();
});

describe("RunDetailScreen — a KILLED run says only what the trail proves", () => {
  it("failure then success is confirmed: the clean copy, and Kill stays disabled", async () => {
    seed({ kills: [kill("failure", 15), kill("success", 6)] });
    renderRun();
    expect(await screen.findByText(/Wardyn stopped and removed the sandbox/)).toBeInTheDocument();
    expect(screen.getByTestId("run-failure-block")).toHaveAttribute("data-evidence", "confirmed");
    expect(killButton()).toBeDisabled();
  });

  it("success then failure is partial: teardown unconfirmed, and Kill is offered again", async () => {
    seed({ kills: [kill("success", 15), kill("failure", 6)] });
    renderRun();
    expect(await screen.findByText(/teardown is not confirmed/)).toBeInTheDocument();
    expect(screen.queryByText(/scratch is gone/)).not.toBeInTheDocument();
    expect(killButton()).toBeEnabled();
  });

  it("no kill row is unknown on the first paint (never a flash of the clean claim), and Kill is offered", async () => {
    seed({ kills: [] });
    renderRun();
    expect(await screen.findByText(/audit trail has no kill record/)).toBeInTheDocument();
    expect(screen.queryByText(/stopped and removed the sandbox/)).not.toBeInTheDocument();
    expect(killButton()).toBeEnabled();
  });
});

describe("RunDetailScreen — what a killed run held that Wardyn cannot revoke", () => {
  const GH = "A GitHub token it already held stays valid until it expires, within an hour.";

  it("reads the mint rows with their own action filter, and joins them to the grants for kind and host", async () => {
    seed({
      kills: [kill("success", 6)],
      grants: [grantRecord("g1", "github_token", { repo: "acme/widgets" }), grantRecord("g2", "git_pat", { host: "dev.azure.com" })],
      mints: [mint("g1"), mint("g2", { jti: "J-SECRET", token: "T-SECRET" })],
    });
    renderRun();
    expect(await screen.findByText(GH)).toBeInTheDocument();
    expect(
      screen.getByText("Wardyn can't revoke the git token this run used — it stays live until you rotate it on dev.azure.com."),
    ).toBeInTheDocument();
    expect(listAuditMock).toHaveBeenCalledWith("run-1", { action: "credential.mint" });
    // Only kind and host reach the DOM.
    expect(document.body.textContent).not.toMatch(/J-SECRET|T-SECRET/);
  });

  it("an api_key-only run gets no per-kind line", async () => {
    seed({ kills: [kill("success", 6)], grants: [grantRecord("g1", "api_key")], mints: [mint("g1")] });
    renderRun();
    await screen.findByText(/Wardyn stopped and removed the sandbox/);
    expect(screen.queryByText(GH)).not.toBeInTheDocument();
    expect(screen.queryByText(/couldn't read which credentials/)).not.toBeInTheDocument();
    expect(screen.queryByText(/can't revoke/)).not.toBeInTheDocument();
  });

  it("the same lines show on a partial kill", async () => {
    seed({ kills: [kill("failure", 6)], grants: [grantRecord("g1", "github_token")], mints: [mint("g1")] });
    renderRun();
    expect(await screen.findByText(GH)).toBeInTheDocument();
  });

  it("an environment-secret grant gets the rotation line without any mint row", async () => {
    seed({ kills: [kill("success", 6)], grants: [grantRecord("g1", "env_secret")], mints: [] });
    renderRun();
    expect(await screen.findByText(/received as an environment variable/)).toBeInTheDocument();
  });

  it("a failed mint fetch says Wardyn couldn't read which credentials the run held", async () => {
    seed({ kills: [kill("success", 6)], grants: [grantRecord("g1", "github_token")], mints: new Error("audit store degraded") });
    renderRun();
    expect(await screen.findByText("Wardyn couldn't read which credentials this run held.")).toBeInTheDocument();
    expect(screen.queryByText(GH)).not.toBeInTheDocument();
  });

  it("a failed grants fetch says the same, rather than reading as 'held nothing'", async () => {
    seed({ kills: [kill("success", 6)], grants: new Error("grants down"), mints: [mint("g1")] });
    renderRun();
    await waitFor(() => expect(screen.getByText("Wardyn couldn't read which credentials this run held.")).toBeInTheDocument());
  });
});
