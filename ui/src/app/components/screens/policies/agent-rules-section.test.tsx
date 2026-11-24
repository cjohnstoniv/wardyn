/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Agent rules section on Policies: the list and the empty state, a save that sends the version it read, the three
// answers a write can get (409, 422, 202), and a delete that asks first and sends the version. The api module is mocked at
// its routes; the shell's setup status supplies the harness names.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError, PendingChangeError } from "../../../lib/api/core";
import type { GovernanceChange } from "../../../lib/types";
import type { AgentRules } from "../../../lib/types/agent-rules";
import { AGENT_RULES as A } from "../../wardyn/copy/agent-rules";
import { ADD_ACCESS } from "../../wardyn/copy/components";
import { CHANGES } from "../../../lib/governance-copy";
import { WALL_PRESETS as W } from "../../wardyn/copy/wall-presets";

const listMock = vi.fn<() => Promise<AgentRules[]>>();
const putMock = vi.fn<(harness: string, write: unknown) => Promise<AgentRules>>();
const removeMock = vi.fn<(harness: string, version: number) => Promise<void>>();
vi.mock("../../../lib/api/agent-rules", async (orig) => ({
  ...(await orig<typeof import("../../../lib/api/agent-rules")>()),
  agentRules: { list: () => listMock(), put: (h: string, w: unknown) => putMock(h, w), remove: (h: string, v: number) => removeMock(h, v) },
}));
vi.mock("../../wardyn/model-access-context", () => ({
  useShellSetupStatus: () => ({
    status: { harnesses: [{ id: "claude-code", display: "Claude Code" }, { id: "codex-cli", display: "Codex CLI" }, { id: "none", display: "Your own tools" }] },
    refresh: () => undefined,
  }),
}));

import { AgentRulesSection } from "./agent-rules-section";

const user = userEvent.setup();
const claude: AgentRules = {
  harness: "claude-code",
  spec: { denied_domains: ["ads.example.net"], min_confinement_class: "CC2" },
  inner_walls: { approvals: "harness", sandbox_mode: "read-only" },
  version: 3,
  updated_by: "alex@example.com",
  updated_at: new Date(Date.now() - 3 * 86400_000).toISOString(),
};
const codex: AgentRules = { harness: "codex-cli", spec: { allowed_domains: ["docs.example.com"] }, version: 1, updated_at: new Date().toISOString() };
const held = { id: "chg-1", status: "pending" } as unknown as GovernanceChange;
const refused = (status: number, reason: string, message: string) => new HttpError(status, message, reason);

beforeEach(() => {
  listMock.mockResolvedValue([claude, codex]);
  putMock.mockResolvedValue(claude);
  removeMock.mockResolvedValue();
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const open = async (name: RegExp | string) => {
  render(<AgentRulesSection />);
  await screen.findByTestId("agent-rules-claude-code");
  await user.click(within(screen.getByTestId("agent-rules-claude-code")).getByRole("button", { name }));
};

describe("AgentRulesSection: the list", () => {
  it("lists one row per agent with its walls, its own walls in capitalised Review words, and who saved it", async () => {
    render(<AgentRulesSection />);
    const row = await screen.findByTestId("agent-rules-claude-code");
    expect(within(row).getByText("Claude Code")).toBeInTheDocument();
    expect(within(row).getByText("1 sites never reached")).toBeInTheDocument();
    expect(within(row).getByText("Approvals in the harness")).toBeInTheDocument();
    expect(within(row).getByText("Harness sandbox read-only")).toBeInTheDocument();
    expect(row).toHaveTextContent("Version 3 · saved by alex@example.com");
    expect(screen.getByTestId("agent-rules-codex-cli")).toHaveTextContent("Codex CLI");
    expect(screen.getByText(A.LEDE)).toBeInTheDocument();
  });

  it("shows the empty state with the Add action, and no second Add in the header", async () => {
    listMock.mockResolvedValue([]);
    render(<AgentRulesSection />);
    expect(await screen.findByText(A.EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.getByText(A.EMPTY_BODY)).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: A.ADD })).toHaveLength(1);
  });

  it("says what could not be loaded, with Retry, and offers no Add", async () => {
    listMock.mockRejectedValueOnce(refused(500, "internal_error", "boom"));
    render(<AgentRulesSection />);
    expect(await screen.findByText(A.LOAD_FAILED_TITLE)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: A.ADD })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByTestId("agent-rules-claude-code")).toBeInTheDocument();
  });

  it("is absent where the store keeps no agent rules", async () => {
    listMock.mockRejectedValueOnce(refused(501, "agent_rules_unavailable", "no"));
    const { container } = render(<AgentRulesSection />);
    await waitFor(() => expect(listMock).toHaveBeenCalled());
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });
});

describe("AgentRulesSection: the editor", () => {
  it("saves at the version it read, with the rules it showed and the agent's own walls", async () => {
    await open(W.EDIT);
    expect(screen.getByRole("heading", { name: A.EDIT_TITLE("Claude Code") })).toBeInTheDocument();
    expect(screen.getByText(W.EDITING_VERSION(3))).toBeInTheDocument();
    expect(screen.queryByText("GitHub capabilities")).toBeNull();
    await user.click(screen.getByRole("button", { name: A.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalledTimes(1));
    expect(putMock).toHaveBeenCalledWith("claude-code", { spec: claude.spec, inner_walls: claude.inner_walls, version: 3 });
  });

  it("a rule set for an agent that has none starts at version 0", async () => {
    listMock.mockResolvedValue([codex]);
    render(<AgentRulesSection />);
    await screen.findByTestId("agent-rules-codex-cli");
    await user.click(screen.getByRole("button", { name: A.ADD }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(A.ADD_REASON)).toBeInTheDocument();
    expect(within(dialog).getByRole("radio", { name: /Codex CLI/ })).toBeDisabled();
    expect(within(dialog).queryByText("Your own tools")).toBeNull();
    await user.click(within(dialog).getByRole("button", { name: "Continue" }));
    await user.click(await screen.findByRole("radio", { name: A.INNER_NONE, checked: false }).catch(() => screen.getAllByRole("radio", { name: A.INNER_NONE })[1]));
    await user.click(screen.getByRole("radio", { name: "Approvals in the harness" }));
    await user.click(screen.getByRole("button", { name: A.SAVE }));
    await waitFor(() => expect(putMock).toHaveBeenCalledWith("claude-code", { spec: {}, inner_walls: { approvals: "harness" }, version: 0 }));
  });

  it("a 409 says nothing was saved and Reload reopens the stored version", async () => {
    putMock.mockRejectedValueOnce(refused(409, "agent_rules_version_conflict", "stale"));
    await open(W.EDIT);
    await user.click(screen.getByRole("button", { name: A.SAVE }));
    expect(await screen.findByText(A.RELOAD_BODY)).toBeInTheDocument();
    listMock.mockResolvedValue([{ ...claude, version: 4 }, codex]);
    await user.click(screen.getByRole("button", { name: W.RELOAD }));
    expect(await screen.findByText(W.EDITING_VERSION(4))).toBeInTheDocument();
    expect(screen.queryByText(A.RELOAD_BODY)).toBeNull();
  });

  it("a 422 shows the server's sentence under the section it names, without the Agent rules prefix", async () => {
    putMock.mockRejectedValueOnce(refused(422, "agent_rules_invalid", 'Agent rules: denied_domains[0]: "ads example.net" must be a DNS name such as api.example.com, or a wildcard such as *.example.com'));
    await open(W.EDIT);
    await user.click(screen.getByRole("button", { name: A.SAVE }));
    const sentence = await screen.findByText(/^denied_domains\[0\]: "ads example.net" must be a DNS name/);
    expect(sentence).toBeInTheDocument();
    expect(screen.queryByText(/^Agent rules: /)).toBeNull();
    expect(sentence.id).toMatch(/-never$/);
  });

  it("a 422 that names no section shows above the buttons", async () => {
    putMock.mockRejectedValueOnce(refused(422, "agent_rules_invalid", "These agent rules name nothing. Delete them instead."));
    await open(W.EDIT);
    await user.click(screen.getByRole("button", { name: A.SAVE }));
    expect(await screen.findByRole("alert")).toHaveTextContent("These agent rules name nothing. Delete them instead.");
  });

  it("a 202 says the change waits for a second person, turns Save off and Cancel into Close", async () => {
    putMock.mockRejectedValueOnce(new PendingChangeError(held));
    await open(W.EDIT);
    await user.click(screen.getByRole("button", { name: A.SAVE }));
    expect(await screen.findByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: A.SAVE })).toBeDisabled();
    expect(screen.queryByRole("button", { name: W.CANCEL })).toBeNull();
    expect(screen.getAllByRole("button", { name: ADD_ACCESS.CLOSE })).toHaveLength(2); // the dialog's X and the footer's Close
  });
});

describe("AgentRulesSection: delete", () => {
  it("asks first, then deletes at the version it read", async () => {
    await open(W.DELETE);
    expect(removeMock).not.toHaveBeenCalled();
    expect(screen.getByRole("heading", { name: A.DELETE_TITLE("Claude Code") })).toBeInTheDocument();
    expect(screen.getByText(A.DELETE_BODY)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: A.DELETE_CONFIRM }));
    await waitFor(() => expect(removeMock).toHaveBeenCalledWith("claude-code", 3));
  });

  it("a 409 says they were not deleted, offers Reload and keeps Delete off until then", async () => {
    removeMock.mockRejectedValueOnce(refused(409, "agent_rules_version_conflict", "stale"));
    await open(W.DELETE);
    await user.click(screen.getByRole("button", { name: A.DELETE_CONFIRM }));
    expect(await screen.findByText(A.DELETE_RELOAD_BODY)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: A.DELETE_CONFIRM })).toBeDisabled();
    listMock.mockResolvedValue([{ ...claude, version: 4 }, codex]);
    await user.click(screen.getByRole("button", { name: W.RELOAD }));
    await waitFor(() => expect(screen.queryByText(A.DELETE_RELOAD_BODY)).toBeNull());
    await user.click(screen.getByRole("button", { name: A.DELETE_CONFIRM }));
    await waitFor(() => expect(removeMock).toHaveBeenLastCalledWith("claude-code", 4));
  });

  it("a held delete shows the submitted note and Close alone", async () => {
    removeMock.mockRejectedValueOnce(new PendingChangeError(held));
    await open(W.DELETE);
    await user.click(screen.getByRole("button", { name: A.DELETE_CONFIRM }));
    expect(await screen.findByText(CHANGES.SUBMITTED_TITLE)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: A.DELETE_CONFIRM })).toBeNull();
    expect(screen.getAllByRole("button", { name: ADD_ACCESS.CLOSE })).toHaveLength(2); // the dialog's X and the footer's Close
  });
});
