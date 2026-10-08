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
