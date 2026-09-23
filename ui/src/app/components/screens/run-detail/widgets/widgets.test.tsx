/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import type { AgentRun, AuditEvent, CredentialGrant, EgressDecision } from "../../../../lib/types";

const getFilesMock = vi.fn();
const getResourcesMock = vi.fn();
vi.mock("../../../../lib/api/runs", () => ({
  runs: {
    getFiles: (...a: unknown[]) => getFilesMock(...a),
    getResources: (...a: unknown[]) => getResourcesMock(...a),
  },
}));

import { HttpError } from "../../../../lib/api/core";
import { fmtBytes } from "../../../../lib/format";
import { RUN_COCKPIT } from "../../../wardyn/copy";
import { EgressWidget } from "./egress";
import { FilesChangedWidget } from "./files-changed";
import { SandboxWidget } from "./sandbox";
import { CredentialsWidget } from "./credentials";
import { IdentityWidget } from "./identity";

beforeEach(() => {
  getFilesMock.mockReset();
  getResourcesMock.mockReset();
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
        time: "2026-08-16T12:00:00Z",
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
        time: "2026-08-16T12:00:00Z",
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
  // APPEND-ONLY (0001_init.sql, chained by 0047), so an egress.pending row is a
  // historical EVENT and never stops being one: deriving the chip from those
  // rows counted a request approved an hour ago, forever, on the one surface
  // whose entire job is to be the alarm. The row still renders as history; only
  // the NUMBER moved to the live derivation (isHeld, live-approvals.tsx).
  it("does not count an egress.pending ROW whose approval has since been APPROVED", () => {
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
  };

  it("carries the full task and the description when the run has them", () => {
    render(<IdentityWidget run={{ ...run, title: "Refund flow", description: "ticket 4412" }} />);
    expect(screen.getByText("Fix the flaky auth tests")).toBeInTheDocument();
    expect(screen.getByText("ticket 4412")).toBeInTheDocument();
  });

  // An interactive run has no task at all and most runs have no description —
  // empty labelled rows would be worse than none.
  it("shows neither label when there is nothing to say", () => {
    render(<IdentityWidget run={{ ...run, task: "", description: "" }} />);
    expect(screen.queryByText("Task")).not.toBeInTheDocument();
    expect(screen.queryByText("Why")).not.toBeInTheDocument();
  });
});
