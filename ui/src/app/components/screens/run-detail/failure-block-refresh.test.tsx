/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, expect, it, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import App from "../../../App";
import { WithDoor } from "../../../../test/door-harness";
import { makeRun } from "../../../../test/factories";
import { setup } from "../../../lib/api/setup";
import { MODEL_PROVIDERS, providerStatus } from "../../../lib/test-fixtures";
import type { AuditEvent } from "../../../lib/types";
import { MODEL_ACCESS_BANNER, MODEL_ACCESS_RUN_DOOR } from "../../wardyn/model-access-copy";
import { CONNECTIONS } from "../../wardyn/copy/door";
import { Cockpit } from "./cockpit";
import { RunFailureBlock } from "./failure-block";

vi.mock("../../attach-terminal", () => ({ AttachTerminal: () => null }));
vi.mock("../../wardyn/terminal-player", () => ({ TerminalPlayer: () => null }));

vi.mock("../settings/harness-login-pane", () => ({
  HarnessLoginPane: (p: { modelProvider?: string }) => <div data-testid="login-pane" data-provider={p.modelProvider} />,
}));

vi.mock("../run-detail", () => ({
  RunDetailScreen: () => (
    <Cockpit
      run={run} audit={audit} view="user" terminal grants={[]} egress={[]} held={undefined}
      outcomeReady pending={[]} recording={null} recState="ready" recordingDisabled
      onGoAudit={() => {}} onGoPolicy={() => {}} onGoRecording={() => {}}
    />
  ),
}));

const provider = MODEL_PROVIDERS.bedrock;
const run = makeRun({ state: "FAILED", created_by: "owner", failure_hint: "Credential unavailable" });
const audit: AuditEvent[] = [{
  id: "refusal", time: run.updated_at, actor_type: "system", actor: "wardynd",
  action: "run.create", outcome: "failure",
  data: { reason: "model_credential", provider: provider.id, kind: provider.kind },
}];

function RefreshingBlock() {
  const [status, setStatus] = React.useState(providerStatus([{ provider }]));
  const refresh = () => setup.getSetupStatus().then(setStatus);
  return (
    <WithDoor status={status} onRefresh={refresh} principal="owner">
      <RunFailureBlock run={run} audit={audit} onGoAudit={() => {}} />
    </WithDoor>
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  window.history.pushState({}, "", "/");
});

it.each(["block", "app"])("%s opens the refused provider with one click across the credential refresh", async (surface) => {
  let resolve!: (response: Response) => void;
  const pending = new Promise<Response>((done) => { resolve = done; });
  let statusReads = 0;
  const json = (body: unknown) => Promise.resolve(new Response(JSON.stringify(body)));
  const fetch = vi.fn((url: RequestInfo | URL) => {
    const path = new URL(String(url), "http://localhost").pathname;
    if (path === "/api/v1/setup/status") {
      statusReads++;
      if (surface === "app" && statusReads === 1) return json(providerStatus([{ provider }]));
      return pending;
    }
    if (path.endsWith("/files")) return json({ vcs: "git", files: [], truncated: false });
    if (path === "/api/v1/me/run-layout") return json({ preset: "finished", layout: [] });
    if (path === "/api/v1/me") return json({ principal: "owner", method: "local", role: "user", operator: false });
    if (path === "/healthz" || path === "/readyz") return json({ status: "ok" });
    return json([]);
  });
  vi.stubGlobal("fetch", fetch);
  window.history.pushState({}, "", "/runs/run-1");
  const user = userEvent.setup();
  render(surface === "block" ? <RefreshingBlock /> : <MemoryRouter initialEntries={["/runs/run-1"]}><App /></MemoryRouter>);
  const button = await screen.findByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA });
  expect(statusReads).toBe(surface === "app" ? 2 : 1);
  const clicked = vi.fn();
  button.addEventListener("click", clicked);
  await user.pointer({ target: button, keys: "[MouseLeft>]" });
  await act(async () => {
    resolve(new Response(JSON.stringify(providerStatus([{ provider, state: "expired_signin" }]))));
    await pending;
  });
  expect(screen.getByText(CONNECTIONS.C6_LINE(provider.name))).toBeVisible();
  await user.pointer({ target: screen.getByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA }), keys: "[/MouseLeft]" });
  expect(clicked).toHaveBeenCalledTimes(1);
  expect(screen.getByRole("dialog", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeVisible();
  expect(await screen.findByTestId("login-pane")).toHaveAttribute("data-provider", provider.id);
  expect(screen.getByRole("button", { name: MODEL_ACCESS_RUN_DOOR.SIGN_IN_ARIA, hidden: true })).toBe(button);
});
