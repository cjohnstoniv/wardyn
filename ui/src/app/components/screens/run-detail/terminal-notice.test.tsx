/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { makeRun } from "../../../../test/factories";
import { OperatorProvider } from "../../wardyn/operator-context";
import { RUN_COCKPIT } from "../../wardyn/copy";
import { TerminalPane } from "./terminal-notice";

vi.mock("../../attach-terminal", () => ({ AttachTerminal: () => null }));
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
    const frame = screen.getByText(RUN_COCKPIT.recordingMissing).closest(".bg-\\[\\#0d1117\\]");
    expect(frame).not.toBeNull();
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
