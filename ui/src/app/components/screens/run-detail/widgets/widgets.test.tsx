/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import type { AuditEvent, CredentialGrant, EgressDecision } from "../../../../lib/types";

const getFilesMock = vi.fn();
const getResourcesMock = vi.fn();
vi.mock("../../../../lib/api/runs", () => ({
  runs: {
    getFiles: (...a: unknown[]) => getFilesMock(...a),
    getResources: (...a: unknown[]) => getResourcesMock(...a),
  },
}));

import { HttpError } from "../../../../lib/api/core";
import { RUN_COCKPIT } from "../../../wardyn/copy";
import { EgressWidget } from "./egress";
import { FilesChangedWidget } from "./files-changed";
import { SandboxWidget } from "./sandbox";
import { CredentialsWidget } from "./credentials";

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
      disk_written_bytes: 2_000_000_000,
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
    render(<EgressWidget egress={egress} />);

    expect(screen.getByText("1 held")).toBeInTheDocument();
    expect(screen.getByText("pending")).toBeInTheDocument();
    expect(screen.getByText("api.github.com")).toBeInTheDocument();
  });
});
