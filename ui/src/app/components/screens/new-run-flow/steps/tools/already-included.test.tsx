/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { BEDROCK_SSO_SWITCH_OFF, HARNESS_NAMES, SCENARIO_ACCESS } from "../../../../../lib/new-run-fixtures/scenarios/tools";
import type { RunIntent } from "../../draft/run-intent";
import { AlreadyIncluded } from "./already-included";

afterEach(cleanup);

const NO_CREDENTIAL = "No credential is set up for this repository, so the run starts without one.";
const TWO_HARNESSES: RunIntent = { tools: [{ id: "claude-code", kind: "harness" }, { id: "codex-cli", kind: "harness" }] };
const rowOf = (name: string) => screen.getByText(name, { selector: "strong" }).closest("li") as HTMLElement;
const open = async (name: string) => userEvent.click(within(rowOf(name)).getByRole("button", { name: NR_TOOLS.DETAILS_FOR(name) }));

describe("Already included", () => {
  it("lists what the earlier steps bring, each with its chip and sub-line, and offers nothing to add or remove", () => {
    render(<AlreadyIncluded names={HARNESS_NAMES} answered access={SCENARIO_ACCESS.member} intent={{ tools: [{ id: "claude-code", kind: "harness" }], startup: { kind: "harness", tool: "claude-code" } }} />);
    expect(screen.getByRole("heading", { level: 2, name: NR_TOOLS.INCLUDED_TITLE })).toBeInTheDocument();
    expect(screen.getByText(NR_TOOLS.INCLUDED_BODY)).toBeInTheDocument();
    const harness = within(rowOf("Claude Code"));
    expect(harness.getByText(NR_TOOLS.FROM_HARNESS)).toBeInTheDocument();
    expect(harness.getByText(NR_TOOLS.SUB_STARTS)).toBeInTheDocument();
    expect(within(rowOf("acme/web")).getByText(NR_TOOLS.FROM_REPOSITORY)).toBeInTheDocument();
    expect(within(rowOf("acme/web")).getByText(NR_TOOLS.REPO_SUB)).toBeInTheDocument();
    expect(within(rowOf("Your drive")).getByText(NR_TOOLS.FROM_DRIVE)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: NR_TOOLS.ROW_REMOVE })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: NR_TOOLS.CARD_ADD })).not.toBeInTheDocument();
  });

  it("starts the startup harness with the run and leaves the others available", () => {
    const intent: RunIntent = { ...TWO_HARNESSES, startup: { kind: "harness", tool: "codex-cli" } };
    render(<AlreadyIncluded names={HARNESS_NAMES} answered access={SCENARIO_ACCESS.interactive2} intent={intent} />);
    expect(within(rowOf("Codex CLI")).getByText(NR_TOOLS.SUB_STARTS)).toBeInTheDocument();
    expect(within(rowOf("Claude Code")).getByText(NR_TOOLS.SUB_AVAILABLE)).toBeInTheDocument();
  });

  it("starts a background agent task's agent with the run, and no harness with a command (DR-0230)", () => {
    const task: RunIntent = { ...TWO_HARNESSES, workload: { kind: "agent_task", agent: "codex-cli" } };
    render(<AlreadyIncluded names={HARNESS_NAMES} answered access={SCENARIO_ACCESS.interactive2} intent={task} />);
    expect(within(rowOf("Codex CLI")).getByText(NR_TOOLS.SUB_STARTS)).toBeInTheDocument();
    expect(within(rowOf("Claude Code")).getByText(NR_TOOLS.SUB_AVAILABLE)).toBeInTheDocument();
    cleanup();
    render(<AlreadyIncluded names={HARNESS_NAMES} answered access={SCENARIO_ACCESS.interactive2} intent={{ ...TWO_HARNESSES, startup: { kind: "command", command: "make test" } }} />);
    expect(within(rowOf("Claude Code")).getByText(NR_TOOLS.SUB_AVAILABLE)).toBeInTheDocument();
    expect(screen.queryByText(NR_TOOLS.SUB_STARTS)).not.toBeInTheDocument();
  });

  it("marks a row that still needs setup, and a ready one", () => {
    render(<AlreadyIncluded names={HARNESS_NAMES} answered access={SCENARIO_ACCESS.interactive2} intent={TWO_HARNESSES} />);
    expect(within(rowOf("Codex CLI")).getByText(NR_TOOLS.STATUS_NEEDS_SETUP)).toBeInTheDocument();
    expect(within(rowOf("Claude Code")).getByText(NR_TOOLS.STATUS_READY)).toBeInTheDocument();
  });

  it("opens a row to its hosts and credentials, closed until asked", async () => {
    render(<AlreadyIncluded names={HARNESS_NAMES} answered access={SCENARIO_ACCESS.member} intent={{}} />);
    expect(screen.queryByText(NR_TOOLS.DETAIL_NETWORK)).not.toBeInTheDocument();
    const toggle = within(rowOf("Claude Code")).getByRole("button", { name: NR_TOOLS.DETAILS_FOR("Claude Code") });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    const detail = within(rowOf("Claude Code"));
    expect(detail.getByText("bedrock-runtime.us-east-1.amazonaws.com")).toBeInTheDocument();
    expect(detail.getByText(NR_TOOLS.DELIVERY_PROXY)).toBeInTheDocument();
    await userEvent.click(toggle);
    expect(screen.queryByText(NR_TOOLS.DETAIL_NETWORK)).not.toBeInTheDocument();
  });

  it("says a drive needs no network or credentials, and a command's registry access comes from the policy", async () => {
    render(<AlreadyIncluded names={HARNESS_NAMES} answered access={SCENARIO_ACCESS.app} intent={{}} />);
    await open("Your drive");
    expect(within(rowOf("Your drive")).getByText(NR_TOOLS.NOTE_NONE_NEEDED)).toBeInTheDocument();
    await open("npm test");
    expect(within(rowOf("npm test")).getByText(NR_TOOLS.SUB_COMMAND)).toBeInTheDocument();
    expect(within(rowOf("npm test")).getByText(NR_TOOLS.NOTE_COMMAND)).toBeInTheDocument();
  });

  it("opens a Bedrock SSO harness (switch off) to both of its credentials", async () => {
    render(<AlreadyIncluded names={HARNESS_NAMES} answered access={BEDROCK_SSO_SWITCH_OFF} intent={{}} />);
    await open("Claude Code");
    const detail = within(rowOf("Claude Code"));
    expect(detail.getByText(NR_TOOLS.DELIVERY_SHORT_LIVED)).toBeInTheDocument();
    expect(detail.getByText(NR_TOOLS.DELIVERY_FILE)).toBeInTheDocument();
  });

  it("holds Ready and Needs setup as Requested while the answer is stale, and leaves aria-controls off a closed row", () => {
    render(<AlreadyIncluded names={HARNESS_NAMES} answered={false} access={SCENARIO_ACCESS.interactive2} intent={TWO_HARNESSES} />);
    expect(within(rowOf("Claude Code")).getByText(NR_TOOLS.NET_REQUESTED)).toBeInTheDocument();
    expect(screen.queryByText(NR_TOOLS.STATUS_READY)).not.toBeInTheDocument();
    expect(screen.queryByText(NR_TOOLS.STATUS_NEEDS_SETUP)).not.toBeInTheDocument();
    expect(within(rowOf("Claude Code")).getByRole("button", { name: NR_TOOLS.DETAILS_FOR("Claude Code") })).not.toHaveAttribute("aria-controls");
  });

  it("shows nothing but its heading before the server has answered", () => {
    render(<AlreadyIncluded names={HARNESS_NAMES} answered access={undefined} intent={{}} />);
    expect(screen.getByRole("heading", { name: NR_TOOLS.INCLUDED_TITLE })).toBeInTheDocument();
    expect(screen.queryByRole("list")).not.toBeInTheDocument();
  });

  it("says why a repository that create gives no credential needs setup (FUG-L369)", () => {
    const access = { ...SCENARIO_ACCESS.member, rows: SCENARIO_ACCESS.member.rows.map((r) => (r.name === "acme/web" ? { ...r, status: "needs_setup" as const, reason: "repo_no_credential" } : r)) };
    const { rerender } = render(<AlreadyIncluded names={HARNESS_NAMES} answered access={access} intent={{}} />);
    expect(within(rowOf("acme/web")).getByText(NO_CREDENTIAL)).toBeInTheDocument();
    expect(within(rowOf("acme/web")).getByText(NR_TOOLS.STATUS_NEEDS_SETUP)).toBeInTheDocument();
    // Until the server has answered this draft, the old answer's reason is not claimed.
    rerender(<AlreadyIncluded names={HARNESS_NAMES} answered={false} access={access} intent={{}} />);
    expect(screen.queryByText(NO_CREDENTIAL)).not.toBeInTheDocument();
  });
});
