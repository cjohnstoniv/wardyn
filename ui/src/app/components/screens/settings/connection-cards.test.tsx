/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The two shared connection cards. What matters here is that a lane's
// "Connected" state comes from the SAME derivation readiness.ts uses
// (deriveIntegrations over the real SetupStatus), not a bespoke re-read of raw
// fields — a card that disagrees with the app-shell chip is the exact class of
// bug the old two-model /integrations page kept producing.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const setSecretMock = vi.fn();
const deleteSecretMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: {
    setSecret: (...a: unknown[]) => setSecretMock(...a),
    deleteSecret: (...a: unknown[]) => deleteSecretMock(...a),
  },
}));
// HarnessLoginPane drives a real PTY through AttachTerminal (xterm), which does
// not render in jsdom — the cards only own the button that opens it.
vi.mock("./harness-login-pane", () => ({ HarnessLoginPane: () => <div data-testid="login-pane" /> }));

import { ModelProviderCard, GitHostCard, S } from "./connection-cards";
import { baseStatus } from "../../../lib/test-fixtures";
import type { SetupStatus } from "../../../lib/types";

const user = userEvent.setup({ pointerEventsCheck: 0 });

function model(status: SetupStatus = baseStatus()) {
  return render(<ModelProviderCard status={status} siteConfig={null} onChanged={vi.fn()} />);
}

beforeEach(() => {
  setSecretMock.mockReset().mockResolvedValue(undefined);
  deleteSecretMock.mockReset().mockResolvedValue(undefined);
});

describe("ModelProviderCard", () => {
  it("offers exactly the three lanes the mock settled on — no Azure, no catalog", () => {
    model();
    const group = screen.getByRole("radiogroup", { name: S.MODEL_TITLE });
    expect(screen.getAllByRole("radio").length).toBe(3);
    expect(group).toBeInTheDocument();
    expect(screen.queryByText(/Azure/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /add integration/i })).not.toBeInTheDocument();
  });

  it("nothing stored: no lane reads Connected", () => {
    model();
    expect(screen.queryByText("Connected")).not.toBeInTheDocument();
  });

  // The honesty invariant readiness.ts shares: a stored key IS a real path, and
  // the card must say so without a probe (Wardyn never dials the provider).
  it("an anthropic key secret makes the API key lane read Connected", () => {
    model(baseStatus({ secrets: { present: ["anthropic-api-key"], github_app: false } }));
    expect(screen.getByText("Connected")).toBeInTheDocument();
  });

  it("a captured managed subscription reads Connected and offers Disconnect", () => {
    model(baseStatus({ harness: [{ provider: "anthropic", captured: true }] }));
    expect(screen.getByText("Connected")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /disconnect/i })).toBeInTheDocument();
  });

  // A host-CLI login lives in the operator's own ~/.claude — Wardyn can read it
  // but cannot revoke it, so offering a Disconnect would be a button that lies.
  it("a HOST-CLI subscription reads Connected but offers no Disconnect", () => {
    model(
      baseStatus({ providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "subscription" }] }),
    );
    expect(screen.getByText("Connected")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /disconnect/i })).not.toBeInTheDocument();
  });

  it("saving an API key writes the conventional secret name", async () => {
    model();
    await user.click(screen.getByRole("radio", { name: /API key/ }));
    await user.type(screen.getByLabelText("Anthropic API key"), "sk-ant-test");
    await user.click(screen.getAllByRole("button", { name: /^save$/i })[0]);
    expect(setSecretMock).toHaveBeenCalledWith("anthropic-api-key", "sk-ant-test");
  });

  // Bedrock's region/model are boot-time daemon config (runs_bedrock.go), so the
  // card must state where they come from rather than render an input the server
  // would ignore.
  it("Bedrock names its config source instead of offering region/model inputs", async () => {
    model();
    await user.click(screen.getByRole("radio", { name: /AWS Bedrock/ }));
    expect(screen.getByText(new RegExp(S.BEDROCK_CONFIG_NOTE.slice(0, 40)))).toBeInTheDocument();
    expect(screen.queryByLabelText(/^region$/i)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/^model$/i)).not.toBeInTheDocument();
  });
});

describe("GitHostCard", () => {
  function git(status: SetupStatus = baseStatus()) {
    return render(<GitHostCard status={status} siteConfig={null} onChanged={vi.fn()} />);
  }

  it("offers the three lanes and defaults to github.com", () => {
    git();
    expect(screen.getByRole("radiogroup", { name: S.GIT_TITLE })).toBeInTheDocument();
    expect(screen.getAllByRole("radio").length).toBe(3);
    expect(screen.getByLabelText("Host")).toHaveValue("github.com");
  });

  // The secret name is per-host, so the card can't pretend there is a single
  // global git credential — retyping the host retargets the write.
  it("the PAT write is named for the host, not a global", async () => {
    git();
    await user.type(screen.getByLabelText("Access token"), "ghp_test");
    await user.click(screen.getByRole("button", { name: /^save$/i }));
    expect(setSecretMock).toHaveBeenCalledWith("git-pat-github-com", "ghp_test");
  });

  it("a stored PAT reads Connected and can be disconnected", async () => {
    git(baseStatus({ secrets: { present: ["git-pat-github-com"], github_app: false } }));
    expect(screen.getByText("Connected")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /disconnect/i }));
    expect(deleteSecretMock).toHaveBeenCalledWith("git-pat-github-com");
  });
});
