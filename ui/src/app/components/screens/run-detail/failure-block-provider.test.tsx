/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #543 (design §5.7, packet 1 door-cards.html states 2 and 3): a provider run's
// credential refusal opens the door of the run's OWN provider — the one its
// audit row names — for the run's owner in the User view; everyone else, and
// every run in the Admin view, reads whose credential it was and gets no door.
// Strings are asserted through the copy modules, never retyped.
import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("../settings/harness-login-pane", () => ({
  HarnessLoginPane: (p: { modelProvider?: string }) => <div data-testid="fake-pane" data-model-provider={p.modelProvider ?? ""} />,
}));

import type { AgentRun, AuditEvent } from "../../../lib/types";
import { makeRun } from "../../../../test/factories";
import { runEndingFromAudit } from "../../../lib/api/audit";
import { RunFailureBlock } from "./failure-block";
import { MODEL_ACCESS_BANNER, MODEL_ACCESS_RUN_DOOR } from "../../wardyn/model-access-copy";
import { CONNECTIONS, KEY_DOOR } from "../../wardyn/copy/door";
import { CONSOLE_VIEW } from "../../wardyn/copy/console-view";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { MODEL_PROVIDERS, providerStatus } from "../../../lib/test-fixtures";
import { WithDoor } from "../../../../test/door-harness";

const { bedrock, claude, gateway, anthropicKey } = MODEL_PROVIDERS;
// A second AWS provider, the claude-code default — the one a door keyed by
// anything but the refusal would open.
const bedrockDev = { ...bedrock, id: "bedrock-dev", name: "Bedrock (dev)" };

// The server's sentences (canon Table 3), rendered verbatim off failure_hint.
const refusal = (name: string, state: string, remedy: string) =>
  `This run's model provider is ${name}, and ${state} — ${remedy} Wardyn does not substitute a different model provider.`;
const CONNECT = "connect it from Getting started in the console, or from the banner the console shows on every page.";
const CHOOSE = "choose another model provider, or ask your admin.";

function failedRun(hint: string, createdBy = "bob@acme.example"): AgentRun {
  return makeRun({ id: "run_1", state: "FAILED", created_by: createdBy, failure_hint: hint, model_provider_id: "x" });
}

/** The dispatch refusal's run.create/failure row (#532's refuseProviderDispatch). */
function trail(provider: string, kind: string, credential = true): AuditEvent[] {
  const data: Record<string, unknown> = { error: "…", provider, kind, mechanism: kind };
  if (credential) data.reason = "model_credential";
  return [{ id: "e1", time: "2026-09-25T10:00:00Z", actor_type: "system", actor: "wardynd", action: "run.create", outcome: "failure", data }];
}

function renderBlock(opts: {
  hint: string;
  audit: AuditEvent[];
  principal?: string;
  createdBy?: string;
  path?: string;
}) {
  const path = opts.path ?? "/runs/run_1";
  window.history.pushState({}, "", path);
  render(
    <WithDoor
      path={path}
      principal={opts.principal ?? "bob@acme.example"}
      operator={path.startsWith("/admin")}
      status={providerStatus([
        { provider: bedrockDev, defaultFor: ["claude-code"] },
        { provider: bedrock },
        { provider: claude },
        { provider: gateway },
        { provider: anthropicKey },
      ])}
    >
      <RunFailureBlock run={failedRun(opts.hint, opts.createdBy)} audit={opts.audit} onGoAudit={() => {}} />
    </WithDoor>,
  );
}

afterEach(() => window.history.pushState({}, "", "/"));

describe("runEndingFromAudit — a provider row is read by `provider`, not `mechanism`", () => {
  it("names the provider and drops the legacy key", () => {
    const ending = runEndingFromAudit("FAILED", trail("bedrock-prod", "bedrock_sso"));
    expect(ending).toMatchObject({ kind: "credential", provider: "bedrock-prod" });
    expect(ending?.mechanism).toBeUndefined();
  });

  it("a legacy row keeps its mechanism", () => {
    const legacy: AuditEvent[] = [
      { ...trail("", "")[0], data: { error: "…", reason: "model_credential", mechanism: "bedrock_sso" } },
    ];
    expect(runEndingFromAudit("FAILED", legacy)).toMatchObject({ kind: "credential", mechanism: "bedrock_sso" });
  });
});

describe("state 2 — the owner, User view: the door of the run's own provider", () => {
  it("AWS: Sign in to AWS opens THIS provider's door, not the claude-code default's", async () => {
    const hint = refusal(bedrock.name, "you are not signed in to AWS for it", CONNECT);
    renderBlock({ hint, audit: trail(bedrock.id, "bedrock_sso") });
    expect(screen.getByText(hint)).toBeInTheDocument();
    const btn = screen.getByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA });
    expect(btn).toHaveTextContent(AGENTS.SIGN_IN_AWS);
    expect(screen.getByText(MODEL_ACCESS_RUN_DOOR.NOTE)).toBeInTheDocument();
    await userEvent.click(btn);
    const dialog = await screen.findByRole("dialog", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE });
    expect(dialog).toHaveTextContent(`For ${bedrock.name}`);
    expect(await screen.findByTestId("fake-pane")).toHaveAttribute("data-model-provider", bedrock.id);
  });

  it("token: Add your token opens the token door (#146 defect 2: never Sign in to AWS)", async () => {
    const hint = refusal(gateway.name, "you have not added your token for it", CONNECT);
    renderBlock({ hint, audit: trail(gateway.id, "custom_endpoint") });
    const btn = screen.getByRole("button", { name: MODEL_ACCESS_RUN_DOOR.ADD_TOKEN_ARIA });
    expect(btn).toHaveTextContent(CONNECTIONS.ADD_TOKEN);
    expect(screen.getByText(MODEL_ACCESS_RUN_DOOR.NOTE_KEY)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA })).toBeNull();
    await userEvent.click(btn);
    expect(await screen.findByRole("dialog", { name: KEY_DOOR.TITLE(true, gateway.name) })).toBeInTheDocument();
  });

  it("key: Add your key opens the key door", async () => {
    const hint = refusal(anthropicKey.name, "you have not added your key for it", CONNECT);
    renderBlock({ hint, audit: trail(anthropicKey.id, "anthropic_api_key") });
    const btn = screen.getByRole("button", { name: MODEL_ACCESS_RUN_DOOR.ADD_KEY_ARIA });
    expect(btn).toHaveTextContent(CONNECTIONS.ADD_KEY);
    expect(screen.getByText(MODEL_ACCESS_RUN_DOOR.NOTE_KEY)).toBeInTheDocument();
    await userEvent.click(btn);
    expect(await screen.findByRole("dialog", { name: KEY_DOOR.TITLE(false, anthropicKey.name) })).toBeInTheDocument();
  });

  it("Claude: Sign in to Claude opens the Claude door", async () => {
    const hint = refusal(claude.name, "you are not signed in to Claude for it", CONNECT);
    renderBlock({ hint, audit: trail(claude.id, "anthropic_subscription") });
    const btn = screen.getByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_CLAUDE_ARIA });
    expect(btn).toHaveTextContent(CONNECTIONS.SIGN_IN_CLAUDE);
    expect(screen.getByText(MODEL_ACCESS_RUN_DOOR.NOTE)).toBeInTheDocument();
    await userEvent.click(btn);
    const dialog = await screen.findByRole("dialog");
    expect(dialog).toHaveTextContent(`For ${claude.name}`);
  });

  it("a provider this person no longer has a door for: the sentence alone", () => {
    const hint = refusal("gone", "you have not added your key for it", CONNECT);
    renderBlock({ hint, audit: trail("gone", "anthropic_api_key") });
    expect(screen.getByText(hint)).toBeInTheDocument();
    expect(screen.queryByText(MODEL_ACCESS_RUN_DOOR.NOTE_KEY)).toBeNull();
    expect(screen.queryByText(MODEL_ACCESS_RUN_DOOR.NOT_OWNER("bob@acme.example"))).toBeNull();
  });
});

describe("state 3 — anyone but the owner, and every run in the Admin view: no door", () => {
  const hint = refusal(bedrock.name, "you are not signed in to AWS for it", CONNECT);

  it("another user's run: whose credential it ran on, and no door", () => {
    renderBlock({ hint, audit: trail(bedrock.id, "bedrock_sso"), principal: "carol@acme.example" });
    expect(screen.getByText(MODEL_ACCESS_RUN_DOOR.NOT_OWNER("bob@acme.example"))).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA })).toBeNull();
    expect(screen.queryByRole("button", { name: CONSOLE_VIEW.OPEN_IN_USER })).toBeNull();
  });

  it("Admin view, bob's run: the same, no door", () => {
    renderBlock({ hint, audit: trail(bedrock.id, "bedrock_sso"), principal: "ann@acme.example", path: "/admin/runs/run_1" });
    expect(screen.getByText(MODEL_ACCESS_RUN_DOOR.NOT_OWNER("bob@acme.example"))).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA })).toBeNull();
    expect(screen.queryByRole("button", { name: CONSOLE_VIEW.OPEN_IN_USER })).toBeNull();
  });

  it("Admin view, the admin's own run: no door, and Open in user view", () => {
    renderBlock({
      hint,
      audit: trail(bedrock.id, "bedrock_sso"),
      principal: "ann@acme.example",
      createdBy: "ann@acme.example",
      path: "/admin/runs/run_1",
    });
    expect(screen.getByText(MODEL_ACCESS_RUN_DOOR.NOT_OWNER("ann@acme.example"))).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA })).toBeNull();
    expect(screen.getByRole("button", { name: CONSOLE_VIEW.OPEN_IN_USER })).toBeInTheDocument();
  });

  it.each([
    ["turned off", refusal(gateway.name, "it is turned off", CHOOSE)],
    ["not available", refusal(anthropicKey.name, "it is not available to Codex CLI", CHOOSE)],
    ["not granted", refusal(gateway.name, "you are not granted it", CHOOSE)],
  ])("%s: the sentence, for anyone, and no door", (_label, sentence) => {
    renderBlock({ hint: sentence, audit: trail(gateway.id, "custom_endpoint", false) });
    expect(screen.getByText(sentence)).toBeInTheDocument();
    expect(screen.queryByText(MODEL_ACCESS_RUN_DOOR.NOTE_KEY)).toBeNull();
    expect(screen.queryByText(MODEL_ACCESS_RUN_DOOR.NOT_OWNER("bob@acme.example"))).toBeNull();
  });
});
