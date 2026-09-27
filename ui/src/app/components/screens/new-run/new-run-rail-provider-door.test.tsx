/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #543 (design §5.8, packet 1 door-cards.html state 4): which door a New Run
// launch refusal opens — the one its own `provider` names (#532), never the
// agent or provider selected on screen (#146's ruling). Its own file: the
// rail's main suite is at the file-size gate.
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("../../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({ components: { recording: { selected: "fs" } } }) },
}));
vi.mock("../settings/harness-login-pane", () => ({
  HarnessLoginPane: ({ onDone, modelProvider }: { onDone: () => void; modelProvider?: string }) => (
    <button type="button" data-model-provider={modelProvider ?? ""} onClick={onDone}>
      fake pane
    </button>
  ),
}));

import { RunRail } from "./new-run-rail";
import { MODEL_ACCESS_BANNER } from "../../wardyn/model-access-copy";
import { KEY_DOOR } from "../../wardyn/copy/door";
import { MODEL_PROVIDERS, providerStatus } from "../../../lib/test-fixtures";
import { WithDoor } from "../../../../test/door-harness";

const { bedrock, gateway } = MODEL_PROVIDERS;
// The claude-code default: the AWS door a refusal keyed by anything but its own
// provider would open.
const bedrockDev = { ...bedrock, id: "bedrock-dev", name: "Bedrock (dev)" };
const STATUS = providerStatus([
  { provider: bedrockDev, defaultFor: ["claude-code"] },
  { provider: bedrock },
  { provider: gateway },
]);

function renderRail(opts: { refusedProvider?: string; credentialRefused?: boolean; error?: string; onLaunch?: () => void }) {
  window.history.pushState({}, "", "/runs/new");
  render(
    <WithDoor status={STATUS} path="/runs/new" operator={false} principal="bob@acme.example">
      <RunRail
        cc="CC1"
        showModelWarning={false}
        startup="It starts."
        showHoldNote={false}
        toolRules={null}
        launch={{
          onLaunch: opts.onLaunch ?? (() => {}),
          disabled: false,
          spinning: false,
          inFlight: false,
          problem: null,
          error: opts.error ?? null,
          errorSeq: 1,
          credentialRefused: opts.credentialRefused ?? true,
          refusedProvider: opts.refusedProvider,
          warnings: [],
          onOpenRun: null,
        }}
        preflight={{ error: null, errorSeq: 0, result: null }}
        adoDialog={{
          open: false,
          connecting: false,
          org: "",
          blockedUrl: null,
          onConfirm: () => {},
          onFallbackClick: () => {},
          onCancel: () => {},
        }}
      />
    </WithDoor>,
  );
}

afterEach(() => window.history.pushState({}, "", "/"));

describe("state 4 — a New Run refusal opens its own provider's door", () => {
  it("a Codex CLI run on the gateway refused over its token opens the token door, not AWS (#146 defect 2)", async () => {
    const sentence = `This run's model provider is ${gateway.name}, and you have not added your token for it — connect it from Getting started in the console, or from the banner the console shows on every page. Wardyn does not substitute a different model provider.`;
    renderRail({ refusedProvider: gateway.id, error: sentence });
    expect(await screen.findByRole("dialog", { name: KEY_DOOR.TITLE(true, gateway.name) })).toBeInTheDocument();
    expect(screen.queryByRole("dialog", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE })).toBeNull();
    // The rail's launch-error line still carries the server's sentence.
    expect(screen.getByText(sentence)).toBeInTheDocument();
  });

  it("an AWS refusal opens THAT provider's door, not the default's, and launches again after sign-in", async () => {
    const onLaunch = vi.fn();
    renderRail({ refusedProvider: bedrock.id, onLaunch });
    const dialog = await screen.findByRole("dialog", { name: MODEL_ACCESS_BANNER.DIALOG_TITLE });
    expect(dialog).toHaveTextContent(`For ${bedrock.name}`);
    const pane = await screen.findByRole("button", { name: "fake pane" });
    expect(pane).toHaveAttribute("data-model-provider", bedrock.id);
    await userEvent.click(pane);
    expect(onLaunch).toHaveBeenCalledTimes(1);
  });

  it("a credential refusal naming no provider opens nothing under a provider block", async () => {
    renderRail({ refusedProvider: "" });
    await act(async () => {});
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("off, not available, not granted: no door — the sentence stands on the launch-error line", async () => {
    const sentence = `This run's model provider is ${gateway.name}, and it is turned off — choose another model provider, or ask your admin. Wardyn does not substitute a different model provider.`;
    renderRail({ credentialRefused: false, refusedProvider: "", error: sentence });
    await act(async () => {});
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByText(sentence)).toBeInTheDocument();
  });
});
