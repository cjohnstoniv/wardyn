/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { health } from "../../../lib/api/health";
import type { RunDetail } from "../../../lib/types";
import { RUN_WAIT, RUN_WAIT_REASON } from "../../../lib/runner-pool-refusals";
import { makeRun } from "../../../../test/factories";
import { OperatorProvider } from "../../wardyn/operator-context";
import { RUN_COCKPIT, RUN_MODE } from "../../wardyn/copy";
import { DESKTOP } from "../../wardyn/copy/desktop";
import { TerminalPane } from "./terminal-notice";

vi.mock("../../../lib/api/health", () => ({ health: { health: vi.fn().mockResolvedValue({}) } }));

vi.mock("../../attach-terminal", () => ({ AttachTerminal: () => null }));
vi.mock("../../../lib/api/runner-pools", () => ({
  runnerPools: { list: () => Promise.resolve({ pools: [{ id: "pool-1", name: "CI jobs", slots: { max: 40 } }] }) },
}));
vi.mock("../../wardyn/terminal-player", () => ({ TerminalPlayer: () => <div data-testid="player" /> }));

const LINK = "Open the Recording tab →";

function pane(recState: "idle" | "loading" | "error" | "ready", recordingDisabled = false, onGoRecording = vi.fn()) {
  render(
    <OperatorProvider operator principal="me">
      <TerminalPane
        run={makeRun({ id: "run-1", state: "COMPLETED", created_by: "me" })}
        terminal
        recording={null}
        recState={recState}
        recordingDisabled={recordingDisabled}
        onGoRecording={onGoRecording}
        execMode={false}
      />
    </OperatorProvider>,
  );
  return onGoRecording;
}

// #1906: the notice's one action is a text link, so it wears the information
// token the two sibling links in this file wear, never the teal "press this".
describe("TerminalPane — the finished run's Recording link", () => {
  it.each([
    ["loading", "loading", false, RUN_COCKPIT.recordingLoading],
    ["idle", "idle", false, RUN_COCKPIT.recordingLoading],
    ["error", "error", false, RUN_COCKPIT.recordingError],
    ["ready without a recording", "ready", false, RUN_COCKPIT.recordingMissing],
    ["recording disabled", "ready", true, RUN_COCKPIT.recordingDisabled],
  ] as const)("%s: same sentence, same name, text-info and not text-primary", (_n, recState, disabled, text) => {
    const go = pane(recState, disabled);
    expect(screen.getByText(text)).toBeInTheDocument();
    const link = screen.getByRole("button", { name: LINK });
    expect(link).toHaveClass("text-info");
    expect(link).not.toHaveClass("text-primary");
    fireEvent.click(link);
    expect(go).toHaveBeenCalledOnce();
  });
});

// #2910: the pane is near-black in both themes, so its header and body copy must
// resolve their tokens from the dark block, not the light theme's.
describe("TerminalPane — always-dark token scope", () => {
  it("scopes the pane frame to the dark tokens", () => {
    pane("ready", false);
    const frame = screen.getByTestId("run-pane-frame");
    expect(frame).toHaveClass("dark");
  });
});

// 0.9 (M-RN-3 RS6): a live interactive run on a person's computer that is offline cannot be attached to, so
// the pane says so by name; the captured replay is still offered, and the terminal stays when it is online.
describe("TerminalPane — a run on an offline computer", () => {
  const live = (over: Record<string, unknown> = {}) =>
    makeRun({ id: "run-1", state: "RUNNING", created_by: "me", interactive: true, placement: "local", runner_name: "laptop-7", runner_online: false, ...over });
  const renderPane = (run: ReturnType<typeof live>, onGoRecording = vi.fn(), recordingDisabled = false) => {
    render(
      <OperatorProvider operator principal="me">
        <TerminalPane run={run} terminal={false} recording={null} recState="idle" recordingDisabled={recordingDisabled} onGoRecording={onGoRecording} execMode={false} />
      </OperatorProvider>,
    );
    return onGoRecording;
  };

  it("names the computer, and the replay link still works", () => {
    const go = renderPane(live());
    expect(screen.getByText("laptop-7 is offline, so its terminal can't be reached. The session keeps running there.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Watch the captured session →" }));
    expect(go).toHaveBeenCalledOnce();
  });

  it("offers no replay link where recording is disabled", () => {
    renderPane(live(), vi.fn(), true);
    expect(screen.getByText(/laptop-7 is offline/)).toBeInTheDocument();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("offers no replay link on a run with no recording", () => {
    renderPane(live({ has_recording: false }));
    expect(screen.getByText(/laptop-7 is offline/)).toBeInTheDocument();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("an online computer, an unknown one, and a remote run keep their terminal", () => {
    for (const over of [{ runner_online: true }, { runner_online: undefined }, { placement: "remote" }]) {
      const { unmount } = render(
        <OperatorProvider operator principal="me">
          <TerminalPane run={live(over)} terminal={false} recording={null} recState="idle" recordingDisabled={false} onGoRecording={vi.fn()} execMode={false} />
        </OperatorProvider>,
      );
      expect(screen.queryByText(/can't be reached/)).toBeNull();
      unmount();
    }
  });
});

// #3922: on an unrecorded run the link opened an empty Recording tab.
describe("TerminalPane — the Watch link needs a recording", () => {
  const paneFor = (over: Record<string, unknown>) =>
    render(
      <OperatorProvider operator principal="me">
        <TerminalPane run={makeRun({ id: "run-1", created_by: "me", interactive: false, ...over })} terminal={false} recording={null} recState="idle" recordingDisabled={false} onGoRecording={vi.fn()} execMode={false} />
      </OperatorProvider>,
    );

  it("a task run without a recording has no link", () => {
    paneFor({ state: "WAITING_FOR_CONFIRMATION", has_recording: false });
    expect(screen.queryByText(RUN_MODE.autonomous.blurb)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Watch the captured session/ })).toBeNull();
  });

  it("a STARTING run the person cannot enter, without a recording, has no link", () => {
    render(
      <OperatorProvider operator={false} principal="me">
        <TerminalPane run={makeRun({ id: "run-1", state: "STARTING", created_by: "priya", interactive: true, has_recording: false })} terminal={false} recording={null} recState="idle" recordingDisabled={false} onGoRecording={vi.fn()} execMode={false} />
      </OperatorProvider>,
    );
    expect(screen.getByText(/Only priya/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Watch the captured session/ })).toBeNull();
  });

  it("a run with a recording keeps it", () => {
    paneFor({ state: "WAITING_FOR_CONFIRMATION", has_recording: true });
    expect(screen.getByRole("button", { name: /Watch the captured session/ })).toBeInTheDocument();
  });
});

// Board F-hero: Watch desktop on the hero of a running macOS VM run whose desktop is view only.
describe("TerminalPane — Watch desktop", () => {
  const paneFor = (over: Record<string, unknown>, principal = "me") =>
    render(
      <OperatorProvider operator={false} principal={principal}>
        <TerminalPane run={makeRun({ id: "run-1", state: "RUNNING", created_by: "me", interactive: false, machine_kind: "macos_vm", macos: { desktop: "observe" }, ...over } as Partial<RunDetail>) as RunDetail} terminal={false} recording={null} recState="idle" recordingDisabled={false} onGoRecording={vi.fn()} execMode={false} />
      </OperatorProvider>,
    );
  beforeEach(() => {
    vi.mocked(health.health).mockClear();
    vi.mocked(health.health).mockResolvedValue({ status: "ok", ui_sandbox: { enabled: true, enter_post_url: "http://ui.local/__wardyn/enter" } } as never);
  });

  it("a task run's own person gets the button", async () => {
    paneFor({});
    expect(await screen.findByRole("button", { name: DESKTOP.watch })).toBeInTheDocument();
  });

  it("is not offered to someone who may not enter the run", async () => {
    paneFor({}, "priya");
    await waitFor(() => expect(screen.getByTestId("run-pane-frame")).toBeInTheDocument());
    expect(screen.queryByRole("button", { name: DESKTOP.watch })).toBeNull();
    expect(health.health).not.toHaveBeenCalled();
  });

  it.each<[string, Record<string, unknown>]>([
    ["a Mac with control", { macos: { desktop: "control" } }],
    ["a run that isn't running", { state: "WAITING_FOR_CONFIRMATION" }],
  ])("is not offered on %s", async (_name, over) => {
    paneFor(over);
    await waitFor(() => expect(screen.getByTestId("run-pane-frame")).toBeInTheDocument());
    expect(screen.queryByRole("button", { name: DESKTOP.watch })).toBeNull();
  });

  it("stays away while the gateway is off", async () => {
    vi.mocked(health.health).mockResolvedValue({ status: "ok", ui_sandbox: { enabled: false } } as never);
    paneFor({});
    await waitFor(() => expect(health.health).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: DESKTOP.watch })).toBeNull();
  });
});

// A QUEUED run's hero was an empty frame: it must say why the run waits, and where it stands in line.
describe("TerminalPane — a QUEUED run", () => {
  const queued = (reason: string) =>
    makeRun({
      id: "run-1",
      state: "QUEUED",
      created_by: "me",
      interactive: true,
      queue: { pool_id: "pool-1", queued_at: new Date(Date.now() - 5 * 60_000).toISOString(), deadline_at: new Date(Date.now() + 3 * 3600_000).toISOString(), position: 1, reason },
    });
  const renderPane = (run: ReturnType<typeof queued>) =>
    render(
      <OperatorProvider operator principal="me">
        <TerminalPane run={run} terminal={false} recording={null} recState="idle" recordingDisabled={false} onGoRecording={vi.fn()} execMode={false} />
      </OperatorProvider>,
    );

  it("names the pool's limit, the place in line and when it gives up", async () => {
    renderPane(queued(RUN_WAIT_REASON.AT_CAPACITY));
    expect(await screen.findByText(RUN_WAIT.AT_CAPACITY("CI jobs", 40))).toBeInTheDocument();
    // The approved mock: a Queued title, then ONE line of place, wait and give-up.
    expect(screen.getByRole("heading", { name: "Queued" })).toBeInTheDocument();
    expect(screen.getByTestId("run-queued-wait-line")).toHaveTextContent(
      /^You are 2nd in line · waiting since .+ · gives up in 3 hours$/,
    );
  });

  it("gives the recording-store sentence without needing the pool", async () => {
    renderPane(queued(RUN_WAIT_REASON.RECORDING_STORE_OFF));
    expect(await screen.findByText(RUN_WAIT.RECORDING_STORE_OFF())).toBeInTheDocument();
  });
});
