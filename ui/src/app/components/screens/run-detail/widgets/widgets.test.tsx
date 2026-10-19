/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { AgentRun, AuditEvent, RunDetail, CredentialGrant, EgressDecision } from "../../../../lib/types";

const getFilesMock = vi.fn();
const getResourcesMock = vi.fn();
vi.mock("../../../../lib/api/runs", () => ({
  runs: {
    getFiles: (...a: unknown[]) => getFilesMock(...a),
    getResources: (...a: unknown[]) => getResourcesMock(...a),
  },
}));

// The Sync rows ask /healthz whether SSH is on; every other suite here answers "no SSH".
const healthMock = vi.fn();
vi.mock("../../../../lib/api/health", () => ({ health: { health: (...a: unknown[]) => healthMock(...a) } }));

// The Sync rows read their audit rows by action too; every other suite here has none.
const listAuditMock = vi.fn();
vi.mock("../../../../lib/api/audit", async (importOriginal) => {
  const m = await importOriginal<Record<string, unknown>>();
  return { ...m, audit: { ...(m.audit as object), listAudit: (...a: unknown[]) => listAuditMock(...a) } };
});

import { HttpError } from "../../../../lib/api/core";
import { fmtBytes } from "../../../../lib/format";
import { RUN_COCKPIT } from "../../../wardyn/copy";
import { EgressWidget } from "./egress";
import type { SpooledEgressDecision } from "../../../../lib/api/audit";
import { FilesChangedWidget } from "./files-changed";
import { SandboxWidget } from "./sandbox";
import { CredentialsWidget } from "./credentials";
import { IdentityWidget } from "./identity";
import { SYNC_REREAD_MS } from "./identity-sync";
import { aheadByHours } from "../../../../lib/test-clock";
import { OperatorProvider } from "../../../wardyn/operator-context";

beforeEach(() => {
  getFilesMock.mockReset();
  getResourcesMock.mockReset();
  healthMock.mockReset().mockResolvedValue({ status: "ok" });
  listAuditMock.mockReset().mockResolvedValue([]);
});

describe("SandboxWidget", () => {
  it("renders the unavailable copy for an absent metric, never a literal 0", async () => {
    // memory_used_bytes is absent on purpose; every other metric is non-zero
    // so a stray "0" in the DOM can only have come from the memory metric.
    getResourcesMock.mockResolvedValue({
      cpu_percent: 38,
      disk_used_bytes: 2_000_000_000,
      process_count: 14,
    });
    render(<SandboxWidget runId="r1" live={true} />);

    expect(await screen.findByText(RUN_COCKPIT.metricUnavailable)).toBeInTheDocument();
    expect(screen.queryByText("0")).not.toBeInTheDocument();
    expect(screen.queryByText("0%")).not.toBeInTheDocument();
  });

  it("shows the exec-unsupported copy on a 501", async () => {
    getResourcesMock.mockRejectedValue(new HttpError(501, "no exec primitive"));
    render(<SandboxWidget runId="r1" live={true} />);
    expect(await screen.findByText(RUN_COCKPIT.execUnsupported)).toBeInTheDocument();
  });

  // The same widgets render on a run that has ended, so the sentence must not say "while it's running" (#2914).
  it("words the exec-unsupported copy for a run in any state", async () => {
    getResourcesMock.mockRejectedValue(new HttpError(501, "no exec primitive"));
    render(<SandboxWidget runId="r1" live={false} />);
    const line = await screen.findByText(RUN_COCKPIT.execUnsupported);
    expect(line.textContent).not.toMatch(/while it's running/);
  });

  // RL-13 (long-holds design rev 4 §8): "a warning at 80% where the cap is
  // enforced" — the disk_cap_bytes backend sends ONLY when a driver actually
  // enforces one.
  it("warns at >=80% of disk_cap_bytes, when the backend sent one", async () => {
    getResourcesMock.mockResolvedValue({
      disk_used_bytes: 900,
      disk_cap_bytes: 1000, // 90%
    });
    render(<SandboxWidget runId="r1" live={true} />);
    expect(await screen.findByText(RUN_COCKPIT.diskNearCap)).toBeInTheDocument();
  });

  it("does not warn below 80%, even with a cap present", async () => {
    getResourcesMock.mockResolvedValue({
      disk_used_bytes: 500,
      disk_cap_bytes: 1000, // 50%
    });
    render(<SandboxWidget runId="r1" live={true} />);
    await screen.findByText("Disk");
    expect(screen.queryByText(RUN_COCKPIT.diskNearCap)).not.toBeInTheDocument();
  });

  it("never warns with no disk_cap_bytes, however high disk_used_bytes is — an unenforced cap is not a denominator", async () => {
    getResourcesMock.mockResolvedValue({
      disk_used_bytes: 999_000_000_000,
    });
    render(<SandboxWidget runId="r1" live={true} />);
    await screen.findByText("Disk");
    expect(screen.queryByText(RUN_COCKPIT.diskNearCap)).not.toBeInTheDocument();
  });

  it("falls back to disk_written_bytes, labeled written, when disk_used_bytes is absent", async () => {
    getResourcesMock.mockResolvedValue({
      disk_written_bytes: 2_000_000_000,
      disk_cap_bytes: 1000, // never a denominator without a used reading
    });
    render(<SandboxWidget runId="r1" live={true} />);
    await screen.findByText("Disk");
    expect(screen.getByText(`${fmtBytes(2_000_000_000)} ${RUN_COCKPIT.diskWrittenSuffix}`)).toBeInTheDocument();
    expect(screen.queryByText(RUN_COCKPIT.diskNearCap)).not.toBeInTheDocument();
  });
});

describe("FilesChangedWidget", () => {
  it("renders a binary file as 'binary' — never a +0/−0 stat", async () => {
    getFilesMock.mockResolvedValue({
      vcs: "git",
      files: [{ path: "assets/logo.png", status: "M", binary: true }],
      truncated: false,
    });
    render(<FilesChangedWidget runId="r1" live={true} />);

    expect(await screen.findByText("binary")).toBeInTheDocument();
    expect(screen.queryByText(/\+0/)).not.toBeInTheDocument();
    expect(screen.queryByText(/−0/)).not.toBeInTheDocument();
  });

  it("shows the no-VCS copy when the workspace isn't a git repo", async () => {
    getFilesMock.mockResolvedValue({ vcs: "none", files: [], truncated: false });
    render(<FilesChangedWidget runId="r1" live={true} />);
    expect(await screen.findByText(RUN_COCKPIT.noVcs(undefined))).toBeInTheDocument();
  });

  // The mount target is configurable per workspace source, so "not a git
  // repository" alone cannot be told apart from "we looked in the wrong
  // directory" — and that mistake would otherwise be completely silent. When
  // the daemon reports which path it inspected, the widget must NAME it.
  it("names the inspected directory when the daemon reports one", async () => {
    getFilesMock.mockResolvedValue({
      vcs: "none",
      files: [],
      truncated: false,
      path: "/srv/custom-target",
    });
    render(<FilesChangedWidget runId="r1" live={true} />);
    expect(await screen.findByText(/\/srv\/custom-target/)).toBeInTheDocument();
  });

  it("shows the exec-unsupported copy on a 501", async () => {
    getFilesMock.mockRejectedValue(new HttpError(501, "no exec primitive"));
    render(<FilesChangedWidget runId="r1" live={true} />);
    expect(await screen.findByText(RUN_COCKPIT.execUnsupported)).toBeInTheDocument();
  });

  it("surfaces a server-truncated list instead of rendering it as complete", async () => {
    getFilesMock.mockResolvedValue({
      vcs: "git",
      files: [{ path: "a.go", status: "M", added: 3, deleted: 1 }],
      truncated: true,
    });
    render(<FilesChangedWidget runId="r1" live={true} />);
    expect(await screen.findByText(/partial/i)).toBeInTheDocument();
  });
});

describe("CredentialsWidget", () => {
  it("does NOT render a denied credential.mint as brokered", async () => {
    const grants: CredentialGrant[] = [];
    const audit: AuditEvent[] = [
      {
        id: "e1",
        time: aheadByHours(-1),
        actor_type: "system",
        actor: "broker",
        action: "credential.mint",
        outcome: "denied",
        target: "api_key:api.anthropic.com",
      },
    ];
    render(<CredentialsWidget grants={grants} audit={audit} />);

    expect(screen.queryByText("brokered")).not.toBeInTheDocument();
    expect(await screen.findByText(/0 eligible · 0 minted/)).toBeInTheDocument();
  });

  it("renders a successful credential.mint as brokered", async () => {
    const grants: CredentialGrant[] = [];
    const audit: AuditEvent[] = [
      {
        id: "e2",
        time: aheadByHours(-1),
        actor_type: "system",
        actor: "broker",
        action: "credential.mint",
        outcome: "success",
        target: "api_key:api.anthropic.com",
      },
    ];
    render(<CredentialsWidget grants={grants} audit={audit} />);
    expect(await screen.findByText("brokered")).toBeInTheDocument();
  });

  // #3037: a grant reads as a sentence; the API's JSON is only behind Details.
  it("reads a GitHub token grant as a sentence, with the JSON only under Details", async () => {
    const grants: CredentialGrant[] = [
      {
        id: "g2",
        scope: 'github_token {"repos":[],"permissions":{"contents":"read"}}',
        audience: "github_token",
        state: "active",
        scopeData: { repos: [], permissions: { contents: "read" } },
      },
    ];
    render(<CredentialsWidget grants={grants} audit={[]} />);
    const row = await screen.findByTestId("credential-grant");
    expect(within(row).getByText("GitHub token · read contents · no repositories listed")).toBeInTheDocument();
    const details = row.querySelector("details")!;
    expect(details).not.toHaveAttribute("open");
    expect(within(details).getByText(RUN_COCKPIT.credentialDetails)).toBeInTheDocument();
    expect(details.querySelector("pre")).toHaveTextContent('"contents": "read"');
    // The visible line outside Details carries no braces.
    expect(row.querySelector("span.truncate")).not.toHaveTextContent(/[{}]/);
  });

  // A GitHub CLI grant carries capabilities, not permissions; a writing capability shows the amber chip.
  it("flags a GitHub CLI grant that may write", async () => {
    const grants: CredentialGrant[] = [
      {
        id: "g3",
        scope: "github_token",
        audience: "github_token",
        state: "active",
        scopeData: { repos: ["acme/api"], api: true, capabilities: ["code_read", "pr"] },
      },
      {
        id: "g4",
        scope: "github_token",
        audience: "github_token",
        state: "active",
        scopeData: { repos: ["acme/web"], api: true, capabilities: ["code_read"] },
      },
    ];
    render(<CredentialsWidget grants={grants} audit={[]} />);
    const rows = await screen.findAllByTestId("credential-grant");
    expect(within(rows[0]).getByText("grants write")).toBeInTheDocument();
    expect(within(rows[1]).queryByText("grants write")).not.toBeInTheDocument();
  });

  it("the lede says what the run may request and that the kind decides whether a credential enters the sandbox", async () => {
    render(<CredentialsWidget grants={[]} audit={[]} />);
    expect(await screen.findByText(RUN_COCKPIT.credentialsEligibility)).toBeInTheDocument();
    expect(RUN_COCKPIT.credentialsEligibility).toMatch(/readable inside the sandbox/);
    expect(RUN_COCKPIT.credentialsEligibility).not.toMatch(/never/i);
  });

  it("claims no delivery for an ssh_key grant, whose key the sandbox can read", async () => {
    const grants: CredentialGrant[] = [
      { id: "g1", scope: "ssh_key:git.example.com", audience: "git.example.com", state: "active" },
    ];
    render(<CredentialsWidget grants={grants} audit={[]} />);
    expect(await screen.findByText(RUN_COCKPIT.credentialsEligibility)).toBeInTheDocument();
    expect(screen.queryByText(/mints a short-lived|never sees your real keys/)).not.toBeInTheDocument();
  });
});

describe("EgressWidget", () => {
  it("surfaces a pending decision as held, in the header count and the row", () => {
    const egress: EgressDecision[] = [
      { id: "e1", time: new Date().toISOString(), domain: "api.github.com", decision: "pending" },
      { id: "e2", time: new Date().toISOString(), domain: "api.anthropic.com", decision: "allow" },
    ];
    render(<EgressWidget egress={egress} heldCount={1} />);

    expect(screen.getByText(RUN_COCKPIT.held(1))).toBeInTheDocument();
    expect(screen.getByText("pending")).toBeInTheDocument();
    expect(screen.getByText("api.github.com")).toBeInTheDocument();
  });

  // B3 — the finding, written as the case that used to fail. The audit trail is
  // APPEND-ONLY (0001_init.sql, chained by 0047), so an egress.hold row is a
  // historical EVENT and never stops being one: deriving the chip from those
  // rows counted a request approved an hour ago, forever, on the one surface
  // whose entire job is to be the alarm. The row still renders as history; only
  // the NUMBER moved to the live derivation (isHeld, live-approvals.tsx).
  it("does not count an egress.hold ROW whose approval has since been APPROVED", () => {
    const egress: EgressDecision[] = [
      {
        id: "e1",
        time: new Date().toISOString(),
        domain: "api.github.com",
        decision: "pending",
        approval_id: "apr_1",
      },
    ];
    // The run's live approvals hold nothing — apr_1 came back APPROVED.
    render(<EgressWidget egress={egress} heldCount={0} />);

    // No chip at all, and in particular not a "1 held" derived from the row.
    expect(screen.queryByText(/held/)).not.toBeInTheDocument();
    // The history itself is untouched — the row and its decision still render.
    expect(screen.getByText("pending")).toBeInTheDocument();
    expect(screen.getByText("api.github.com")).toBeInTheDocument();
  });

  // Absent reads as ZERO, never as "fall back to counting the rows" — that
  // fallback IS the bug, and a widget mounted without the cockpit above it
  // (the catalog preview) must not resurrect it.
  it("an unpassed heldCount is no chip, even with pending rows in the trail", () => {
    const egress: EgressDecision[] = [
      { id: "e1", time: new Date().toISOString(), domain: "api.github.com", decision: "pending" },
    ];
    render(<EgressWidget egress={egress} />);
    expect(screen.queryByText(/held/)).not.toBeInTheDocument();
  });

  // 0.9 (M-RN-3 RS7): a decision a runner made while it was offline shows when it made it, not when it reached us;
  // the refusal a full offline log forces says so.
  it("shows a spooled row's own time and the full-log reason, and an ordinary row's age", () => {
    const observed = aheadByHours(-5);
    const egress: SpooledEgressDecision[] = [
      { id: "e1", time: new Date().toISOString(), domain: "api.example.com", decision: "allow", spooled_at: observed },
      { id: "e2", time: new Date().toISOString(), domain: "blocked.example.com", decision: "deny", spooled_at: observed, spool_full: true },
      { id: "e3", time: new Date().toISOString(), domain: "plain.example.com", decision: "allow" },
    ];
    render(<EgressWidget egress={egress} />);
    expect(screen.getAllByText(/^Recorded offline at /)).toHaveLength(2);
    expect(screen.getByText("Refused — the runner's offline log was full")).toBeInTheDocument();
    expect(screen.getByText("plain.example.com").parentElement).toHaveTextContent("just now");
  });
});

// The command bar's h1 is the run's TITLE now, and it truncates in the
// bar's 52px single row at xl and up (wraps below — review R-16) — so the
// task (the prompt the agent was actually given) and the description have
// nowhere else to live on the page. Overview is the canvas cockpit; this
// widget is the only prose surface left.
describe("IdentityWidget", () => {
  const run: AgentRun = {
    id: "run-1",
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
    created_by: "me",
    agent: "claude-code",
    repo: "acme/widgets",
    task: "Fix the flaky auth tests",
    confinement_class: "CC2",
    state: "RUNNING",
    spiffe_id: "spiffe://x",
    runner_target: "docker",
    placement: "",
  };

  it("carries the full task and the description when the run has them", () => {
    render(<IdentityWidget run={{ ...run, title: "Refund flow", description: "ticket 4412" }} onGoPolicy={() => {}} />);
    expect(screen.getByText("Fix the flaky auth tests")).toBeInTheDocument();
    expect(screen.getByText("ticket 4412")).toBeInTheDocument();
  });

  // An interactive run has no task at all and most runs have no description —
  // empty labelled rows would be worse than none.
  it("shows neither label when there is nothing to say", () => {
    render(<IdentityWidget run={{ ...run, task: "", description: "" }} onGoPolicy={() => {}} />);
    expect(screen.queryByText("Task")).not.toBeInTheDocument();
    expect(screen.queryByText("Why")).not.toBeInTheDocument();
  });

  // UT-7a — "Ran as {type}", named by GET /runs/{id}'s user_type_name.
  it("shows Ran as with the type's name when the run carries one", () => {
    render(<IdentityWidget run={{ ...run, user_type: "portfolio-manager", user_type_name: "Portfolio manager" }} onGoPolicy={() => {}} />);
    expect(screen.getByText("Portfolio manager")).toBeInTheDocument();
    expect(screen.getByText("Ran as")).toBeInTheDocument();
  });

  // #1425: every run has a policy, so the row is unconditional. It opens the
  // Policy tab; the saved policy's id moves to the hover title.
  it("always shows a Policy row whose View link opens the Policy tab", () => {
    const onGoPolicy = vi.fn();
    render(<IdentityWidget run={run} onGoPolicy={onGoPolicy} />);
    expect(screen.getByText("Policy")).toBeInTheDocument();
    const view = screen.getByRole("button", { name: "View" });
    expect(view).not.toHaveAttribute("title");
    fireEvent.click(view);
    expect(onGoPolicy).toHaveBeenCalledTimes(1);
  });

  it("keeps the saved policy's id on the View link's title, never as visible text", () => {
    render(<IdentityWidget run={{ ...run, policy_id: "3f1c9a2e-7d44-4b1e-9a0c-52e8b6d1f0a7" }} onGoPolicy={() => {}} />);
    expect(screen.getByRole("button", { name: "View" })).toHaveAttribute("title", "3f1c9a2e-7d44-4b1e-9a0c-52e8b6d1f0a7");
    expect(screen.queryByText("3f1c9a2e-7d44-4b1e-9a0c-52e8b6d1f0a7")).not.toBeInTheDocument();
  });

  it("renders no row for a run with no stamped type", () => {
    render(<IdentityWidget run={run} onGoPolicy={() => {}} />);
    expect(screen.queryByText("Ran as")).not.toBeInTheDocument();
  });

  it("renders no row for a type the server could not name (deleted) — never the raw id", () => {
    render(<IdentityWidget run={{ ...run, user_type: "deleted-type" }} onGoPolicy={() => {}} />);
    expect(screen.queryByText("Ran as")).not.toBeInTheDocument();
    expect(screen.queryByText("deleted-type")).not.toBeInTheDocument();
  });

  // GET /user-types is securityOps: a user-tier owner's run page asking for it
  // is a 403 and an authz.denied audit row on every visit.
  it("issues no /user-types request", () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal("fetch", fetchSpy);
    try {
      render(<IdentityWidget run={{ ...run, user_type: "portfolio-manager", user_type_name: "Portfolio manager" }} onGoPolicy={() => {}} />);
      expect(fetchSpy.mock.calls.filter(([u]) => String(u).includes("/user-types"))).toEqual([]);
    } finally {
      vi.unstubAllGlobals();
    }
  });
});

// 0.9.1: the Identity widget says where the run ran for every run that has it, a successful one included.
describe("IdentityWidget: where the run ran", () => {
  const run = {
    id: "run-1",
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
    created_by: "me",
    agent: "claude-code",
    repo: "acme/widgets",
    task: "",
    confinement_class: "CC2",
    state: "COMPLETED",
    spiffe_id: "spiffe://x",
    runner_target: "docker",
    placement: "",
  } as RunDetail;
  const rows = (over: Partial<RunDetail>) => {
    render(<IdentityWidget run={{ ...run, ...over }} onGoPolicy={() => {}} />);
    const read = (label: string) => screen.queryByText(label)?.nextElementSibling?.textContent ?? null;
    return { placement: read("Runs on"), runner: read("Runner"), pool: read("Pool") };
  };

  it("names the remote-hosted runner and its pool on a completed run that was not filled", () => {
    expect(rows({ placement: "remote", remote_runner_id: "k8s", runner_name: "Cluster A", runner_pool_id: "p1", runner_pool_name: "Build farm" })).toEqual({
      placement: "Remote-hosted runner",
      runner: "Cluster A",
      pool: "Build farm",
    });
  });

  it("names the person's own computer, and falls back to the ids when no names are known", () => {
    expect(rows({ placement: "local", runner_id: "7c1d2e3f-aaaa", runner_pool_id: "p2" })).toEqual({
      placement: "Runner 7c1d2e3f · me's runner",
      runner: "7c1d2e3f-aaaa",
      pool: "p2",
    });
  });

  it("shows no Runs on or Pool row for a run with no recorded place, and keeps the Runner row", () => {
    expect(rows({})).toEqual({ placement: null, runner: "docker", pool: null });
  });
});

// 0.9 (packet M-RN-3 RS1, RS1b): the Identity widget's Sync rows. Ended sessions are the run's
// ssh.sync.transfer audit rows (bytes_in is INTO the sandbox); open ones are RunDetail.sync.open.
describe("IdentityWidget: Sync rows", () => {
  const run = {
    id: "run-1",
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
    created_by: "me",
    agent: "claude-code",
    repo: "acme/widgets",
    task: "",
    confinement_class: "CC2",
    state: "RUNNING",
    spiffe_id: "spiffe://x",
    runner_target: "docker",
    placement: "remote",
  } as AgentRun;
  const transfer = (dir: string, bytesIn: number, bytesOut: number, hoursAgo = 1, extra: Record<string, unknown> = {}): AuditEvent => ({
    id: `${dir}-${bytesIn}-${hoursAgo}`,
    time: aheadByHours(-hoursAgo),
    actor_type: "human",
    actor: "me",
    action: "ssh.sync.transfer",
    outcome: "success",
    data: { dir, direction: "push", bytes_in: bytesIn, bytes_out: bytesOut, ...extra },
  });
  const renderRows = (over: Record<string, unknown> = {}, audit: AuditEvent[] = [], onGoAudit = () => {}) =>
    render(<IdentityWidget run={{ ...run, ...over } as AgentRun} audit={audit} onGoPolicy={() => {}} onGoAudit={onGoAudit} />);

  it("shows per directory the bytes to and from the sandbox and the last activity, summed over ended sessions", () => {
    renderRows({}, [transfer("/workspace/app", 4_000_000, 1000), transfer("/workspace/app", 200_000, 0, 2)]);
    expect(screen.getByText("Sync")).toBeInTheDocument();
    expect(screen.getByText("/workspace/app")).toBeInTheDocument();
    // 4,200,000 B into the sandbox, 1,000 B out: the direction is the contract.
    expect(screen.getByText(`${fmtBytes(4_200_000)} to the sandbox · ${fmtBytes(1000)} from the sandbox`)).toBeInTheDocument();
    expect(screen.getByText(/^Last activity /)).toBeInTheDocument();
    expect(screen.queryByText("Byte counts update when a session ends.")).not.toBeInTheDocument();
  });

  it("an open session says since when, with the note, and a directory never synced before is only that (RS1b)", () => {
    renderRows({ sync: { open: [{ dir: "/workspace/new-dir", opened_at: aheadByHours(-0.5) }] } });
    expect(screen.getByText("/workspace/new-dir")).toBeInTheDocument();
    expect(screen.getByText(/^Open since /)).toBeInTheDocument();
    expect(screen.getByText("Byte counts update when a session ends.")).toBeInTheDocument();
    expect(screen.queryByText(/to the sandbox/)).not.toBeInTheDocument();
    expect(screen.queryByText(/^Last activity/)).not.toBeInTheDocument();
  });

  it("merges an open session into its directory's ended totals", () => {
    renderRows({ sync: { open: [{ dir: "/w", opened_at: aheadByHours(-0.1) }] } }, [transfer("/w", 10, 20)]);
    expect(screen.getAllByText("/w")).toHaveLength(1);
    expect(screen.getByText(/^Open since /)).toBeInTheDocument();
    expect(screen.getByText(`${fmtBytes(10)} to the sandbox · ${fmtBytes(20)} from the sandbox`)).toBeInTheDocument();
  });

  it("reads the sync rows by action, so syncs past the page trail's row cap still show and never 'nothing synced'", async () => {
    healthMock.mockResolvedValue({ status: "ok", ssh: { enabled: true } });
    listAuditMock.mockImplementation((_id: string, f?: { action?: string }) =>
      Promise.resolve(f?.action === "ssh.sync.transfer" ? [transfer("/workspace/app", 4_000_000, 1000)] : []),
    );
    // The page's trail is full of other rows; the sync row sits past its cap.
    renderRows({}, [{ ...transfer("/x", 0, 0), id: "egress", action: "egress.allow" }]);
    expect(await screen.findByText(`${fmtBytes(4_000_000)} to the sandbox · ${fmtBytes(1000)} from the sandbox`)).toBeInTheDocument();
    expect(listAuditMock).toHaveBeenCalledWith("run-1", { action: "ssh.sync.transfer" });
    expect(screen.queryByText("Nothing synced yet.")).not.toBeInTheDocument();
  });

  it("reads the sync rows again while the run is live, for a session that opened and ended between two polls (DR-0167)", async () => {
    vi.useFakeTimers();
    try {
      healthMock.mockResolvedValue({ status: "ok", ssh: { enabled: true } });
      listAuditMock.mockResolvedValue([]);
      renderRows({});
      await act(async () => { await vi.advanceTimersByTimeAsync(0); });
      const syncReads = () => listAuditMock.mock.calls.filter((c) => (c[1] as { action?: string } | undefined)?.action === "ssh.sync.transfer").length;
      const first = syncReads();
      expect(first).toBeGreaterThan(0);
      await act(async () => { await vi.advanceTimersByTimeAsync(SYNC_REREAD_MS); });
      expect(syncReads()).toBe(first + 1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("a re-read that fails keeps the rows the last good read found (FR-0113)", async () => {
    vi.useFakeTimers();
    try {
      healthMock.mockResolvedValue({ status: "ok", ssh: { enabled: true } });
      listAuditMock.mockImplementation((_id: string, f?: { action?: string }) =>
        Promise.resolve(f?.action === "ssh.sync.transfer" ? [transfer("/workspace/app", 4_000_000, 1000)] : []),
      );
      renderRows({});
      await act(async () => { await vi.advanceTimersByTimeAsync(0); });
      const line = `${fmtBytes(4_000_000)} to the sandbox · ${fmtBytes(1000)} from the sandbox`;
      expect(screen.getByText(line)).toBeInTheDocument();
      listAuditMock.mockRejectedValue(new Error("offline"));
      await act(async () => { await vi.advanceTimersByTimeAsync(SYNC_REREAD_MS); });
      expect(screen.getByText(line)).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it("an ended session's error is shown verbatim under its directory", () => {
    renderRows({}, [transfer("/w", 0, 0, 1, { error: "sftp-server exited 3" })]);
    expect(screen.getByText("Last session ended with an error: sftp-server exited 3")).toBeInTheDocument();
  });

  it("shows at most three directories, most recent first, then a link to the Audit tab for the rest", () => {
    const onGoAudit = vi.fn();
    renderRows({}, [transfer("/d1", 1, 1, 5), transfer("/d2", 1, 1, 4), transfer("/d3", 1, 1, 3), transfer("/d4", 1, 1, 2), transfer("/d5", 1, 1, 1)], onGoAudit);
    expect(["/d5", "/d4", "/d3"].every((d) => screen.queryByText(d))).toBe(true);
    expect(screen.queryByText("/d2")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "and 2 more in the Audit tab" }));
    expect(onGoAudit).toHaveBeenCalledTimes(1);
  });

  it("a plain sftp session is its own row with its own bytes", () => {
    renderRows({}, [{ ...transfer("", 0, 0), action: "ssh.sftp.transfer", data: { bytes: 512, bytes_in: 2_100_000, bytes_out: 512 } }]);
    expect(screen.getByText("sftp")).toBeInTheDocument();
    expect(screen.getByText(`${fmtBytes(2_100_000)} to the sandbox · ${fmtBytes(512)} from the sandbox`)).toBeInTheDocument();
    expect(screen.queryByText("Sync")).not.toBeInTheDocument();
  });

  it("says Nothing synced yet only for a live run while /healthz reports SSH on", async () => {
    healthMock.mockResolvedValue({ status: "ok", ssh: { enabled: true } });
    renderRows();
    expect(await screen.findByText("Nothing synced yet.")).toBeInTheDocument();
  });

  it.each<[string, Record<string, unknown>, Record<string, unknown>, number]>([
    ["SSH off", { status: "ok", ssh: { enabled: false } }, {}, 1],
    ["no answer from /healthz", {}, {}, 1],
    ["a finished run", { status: "ok", ssh: { enabled: true } }, { state: "SUCCEEDED" }, 0],
  ])("shows nothing at all when there is nothing synced and: %s", async (_name, health, over, asked) => {
    healthMock.mockResolvedValue(health);
    renderRows(over);
    await waitFor(() => expect(healthMock).toHaveBeenCalledTimes(asked));
    expect(screen.queryByText("Sync")).not.toBeInTheDocument();
    expect(screen.queryByText("Nothing synced yet.")).not.toBeInTheDocument();
  });

  it("a local run has no Sync rows, whatever its audit trail holds", async () => {
    healthMock.mockResolvedValue({ status: "ok", ssh: { enabled: true } });
    renderRows({ placement: "local", sync: { open: [{ dir: "/w", opened_at: aheadByHours(-1) }] } }, [transfer("/w", 1, 1)]);
    expect(screen.queryByText("Sync")).not.toBeInTheDocument();
    expect(screen.queryByText("/w")).not.toBeInTheDocument();
    expect(healthMock).not.toHaveBeenCalled();
  });
});

// 0.9 (M-RN-3 RS3): where a run runs, and for a local run whose evidence the computer reported, whose word it is.
describe("IdentityWidget: Runs on and Evidence", () => {
  const base = {
    id: "run-1",
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
    created_by: "me",
    agent: "claude-code",
    repo: "acme/widgets",
    task: "",
    confinement_class: "CC2",
    state: "RUNNING",
    spiffe_id: "spiffe://x",
    runner_target: "docker",
    placement: "local",
    runner_id: "11111111-2222-3333-4444-555555555555",
    runner_name: "laptop-7",
    evidence_source: "runner_asserted",
  } as AgentRun;
  const renderAs = (run: AgentRun, principal: string) =>
    render(
      <OperatorProvider operator={false} principal={principal}>
        <IdentityWidget run={run} onGoPolicy={() => {}} />
      </OperatorProvider>,
    );

  it("a local run's owner reads 'laptop-7 · your computer', Evidence 'Reported by laptop-7', and the note", () => {
    renderAs(base, "me");
    expect(screen.getByText("Runs on")).toBeInTheDocument();
    expect(screen.getByText("laptop-7 · your computer")).toBeInTheDocument();
    expect(screen.getByText("Evidence")).toBeInTheDocument();
    expect(screen.getByText("Reported by laptop-7")).toBeInTheDocument();
    expect(screen.getByText("laptop-7 is a computer its owner administers, so this run's barrier, egress decisions made on laptop-7, recording, output and exit code are as laptop-7 reported them.")).toBeInTheDocument();
    expect(screen.queryByText("Placement")).not.toBeInTheDocument();
  });

  it("someone else reads whose runner it is", () => {
    renderAs(base, "admin");
    expect(screen.getByText("laptop-7 · me's runner")).toBeInTheDocument();
  });

  it("no Evidence row or note unless the computer vouched for the evidence", () => {
    renderAs({ ...base, evidence_source: "substrate" } as AgentRun, "me");
    expect(screen.queryByText("Evidence")).not.toBeInTheDocument();
    expect(screen.queryByText(/is a computer its owner administers/)).not.toBeInTheDocument();
  });

  it("names a runner the server could not read by a short id, never an empty name", () => {
    renderAs({ ...base, runner_name: undefined } as AgentRun, "me");
    expect(screen.getByText("Runner 11111111 · your computer")).toBeInTheDocument();
  });

  // C2: a remote run says where it ran (0.9.1: placed or not), and never carries the Evidence row or note.
  it.each(["substrate", "runner_asserted"] as const)("a remote run has no Evidence, whatever its evidence_source (%s)", (evidence_source) => {
    renderAs({ ...base, placement: "remote", runner_id: undefined, runner_name: undefined, evidence_source } as AgentRun, "me");
    expect(screen.getByText("Remote-hosted runner")).toBeInTheDocument();
    expect(screen.queryByText("The only place this run could run.")).not.toBeInTheDocument();
    expect(screen.queryByText("Evidence")).not.toBeInTheDocument();
    expect(screen.queryByText(/your computer/)).not.toBeInTheDocument();
    expect(screen.queryByText(/is a computer its owner administers/)).not.toBeInTheDocument();
  });

  it("a remote run the resolver placed says where, and that it was the only place", () => {
    renderAs({ ...base, placement: "remote", placement_filled: true, runner_id: undefined, runner_name: undefined, evidence_source: "substrate" } as AgentRun, "me");
    expect(screen.getByText("Remote-hosted runner")).toBeInTheDocument();
    expect(screen.getByText("The only place this run could run.")).toBeInTheDocument();
  });
});
