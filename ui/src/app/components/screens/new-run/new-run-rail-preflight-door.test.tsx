/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A preflight sign-in never launches a run nobody clicked: a model-credential
// refusal from preflight opens the provider's door, and that door's sign-in
// re-checks the current body. Only an explicit Launch click can lead to a
// launch after sign-in. Its own file: the provider-door suite pins the launch
// origin; this one pins that the two origins never cross.
import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("../../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({ components: { recording: { selected: "fs" } } }) },
}));
vi.mock("../settings/harness-login-pane", () => ({
  HarnessLoginPane: ({ onDone }: { onDone: () => void }) => (
    <button type="button" onClick={onDone}>
      fake pane
    </button>
  ),
}));

import { RunRail } from "./new-run-rail";
import { MODEL_PROVIDERS, providerStatus } from "../../../lib/test-fixtures";
import { AGENTS } from "../../../lib/workspace-providers-copy";
import { WithDoor } from "../../../../test/door-harness";

const { bedrock } = MODEL_PROVIDERS;
// A default provider that is not signed in: the shell strip says so and carries
// the sign-in button, the "elsewhere" entrance the cancel cases use.
const STATUS = providerStatus([{ provider: bedrock, defaultFor: ["claude-code"], state: "not_configured" }], {
  harnesses: [{ id: "claude-code", display: "Claude Code", has_gateway: false, has_login: true }],
});

interface Handles {
  setBody: (b: string) => void;
  /** The screen's launch refusal, as a 422 answers a click. */
  refuseLaunch: () => void;
  /** Preflight's refusal of the body it was graded on. */
  refusePreflight: (body?: string) => void;
}

// Stands in for the screen: owns the body and the two refusal flags the hook
// would own, and hands the rail the same props the launch panel does.
function Harness({ onLaunch, onPreflight, handles }: { onLaunch: () => void; onPreflight: () => void; handles: { current: Handles | null } }) {
  const [body, setBody] = React.useState("body-1");
  const [launchRefused, setLaunchRefused] = React.useState(false);
  const [refusal, setRefusal] = React.useState<{ body: string; provider: string } | null>(null);
  const bodyRef = React.useRef(body);
  bodyRef.current = body;
  handles.current = {
    setBody,
    refuseLaunch: () => setLaunchRefused(true),
    refusePreflight: (b) => setRefusal({ body: b ?? bodyRef.current, provider: bedrock.id }),
  };
  return (
    <RunRail
      cc="CC1"
      showModelWarning={false}
      startup="It starts."
      showHoldNote={false}
      toolRules={null}
      unattended={false}
      launch={{
        onLaunch: () => {
          setLaunchRefused(false);
          onLaunch();
        },
        disabled: false,
        spinning: false,
        inFlight: false,
        problem: null,
        error: null,
        errorSeq: 0,
        credentialRefused: launchRefused,
        refusedProvider: bedrock.id,
        body,
      }}
      preflight={{ error: null, errorSeq: 0, result: null, onPreflight, refusal }}
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
  );
}

function renderRail() {
  window.history.pushState({}, "", "/runs/new");
  const onLaunch = vi.fn();
  const onPreflight = vi.fn();
  const handles: { current: Handles | null } = { current: null };
  render(
    <WithDoor status={STATUS} path="/runs/new" operator={false} principal="bob@acme.example">
      <Harness onLaunch={onLaunch} onPreflight={onPreflight} handles={handles} />
    </WithDoor>,
  );
  return { onLaunch, onPreflight, h: () => handles.current! };
}

afterEach(() => window.history.pushState({}, "", "/"));

const pane = () => screen.findByRole("button", { name: "fake pane" });

const launchButton = () => screen.getByRole("button", { name: "Launch run" });
const settle = () => act(() => new Promise((r) => setTimeout(r, 30)));

describe("a preflight-origin sign-in re-checks and never launches", () => {
  it("a background preflight (driven from a timer) refused, then sign-in: no launch, one re-check", async () => {
    const { onLaunch, onPreflight, h } = renderRail();
    // A later automatic preflight's shape: nobody clicked anything; a timer lands the refusal.
    setTimeout(() => act(() => h().refusePreflight()), 10);
    await userEvent.click(await pane());
    expect(onPreflight).toHaveBeenCalledTimes(1);
    expect(onLaunch).not.toHaveBeenCalled();
  });

  it("a manual preflight refused, then sign-in: no launch", async () => {
    const { onLaunch, onPreflight, h } = renderRail();
    act(() => h().refusePreflight());
    await userEvent.click(await pane());
    expect(onPreflight).toHaveBeenCalledTimes(1);
    expect(onLaunch).not.toHaveBeenCalled();
  });

  it("opens at most once per body and provider", async () => {
    const { h } = renderRail();
    act(() => h().refusePreflight());
    await userEvent.click(await pane());
    // The re-check is refused again on the same body: the door stays shut.
    act(() => h().refusePreflight());
    await settle();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("a body edit while the door is open, then sign-in: no launch", async () => {
    const { onLaunch, onPreflight, h } = renderRail();
    act(() => h().refusePreflight());
    const p = await pane();
    act(() => h().setBody("body-2"));
    await userEvent.click(p);
    expect(onPreflight).toHaveBeenCalledTimes(1);
    expect(onLaunch).not.toHaveBeenCalled();
  });

  it("a cancelled sign-in: no launch, and a later sign-in from elsewhere does not launch", async () => {
    const { onLaunch, onPreflight, h } = renderRail();
    act(() => h().refusePreflight());
    await pane();
    await userEvent.keyboard("{Escape}");
    await settle();
    expect(screen.queryByRole("dialog")).toBeNull();
    // Sign-in completes elsewhere (the strip): the cancelled door has no callback left.
    await userEvent.click(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS }));
    await userEvent.click(await pane());
    expect(onLaunch).not.toHaveBeenCalled();
    expect(onPreflight).not.toHaveBeenCalled();
  });
});

describe("an explicit Launch click is the only way to launch after sign-in", () => {
  it("Launch -> 422 model_credential -> sign-in launches exactly once", async () => {
    const { onLaunch, onPreflight, h } = renderRail();
    await userEvent.click(launchButton());
    expect(onLaunch).toHaveBeenCalledTimes(1);
    act(() => h().refuseLaunch());
    await userEvent.click(await pane());
    expect(onLaunch).toHaveBeenCalledTimes(2);
    expect(onPreflight).not.toHaveBeenCalled();
  });

  it("a body edit between the click and the sign-in re-checks instead of launching", async () => {
    const { onLaunch, onPreflight, h } = renderRail();
    await userEvent.click(launchButton());
    act(() => h().refuseLaunch());
    const p = await pane();
    act(() => h().setBody("body-2"));
    await userEvent.click(p);
    expect(onLaunch).toHaveBeenCalledTimes(1);
    expect(onPreflight).toHaveBeenCalledTimes(1);
  });

  it("a cancelled sign-in after a Launch click: a later sign-in from elsewhere does not launch again", async () => {
    const { onLaunch, onPreflight, h } = renderRail();
    await userEvent.click(launchButton());
    act(() => h().refuseLaunch());
    await pane();
    await userEvent.keyboard("{Escape}");
    await settle();
    // Sign-in completes elsewhere (the strip): the armed closure died with the door.
    await userEvent.click(screen.getByRole("button", { name: AGENTS.SIGN_IN_AWS }));
    await userEvent.click(await pane());
    expect(onLaunch).toHaveBeenCalledTimes(1);
    expect(onPreflight).not.toHaveBeenCalled();
  });
});
