/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { HttpError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import { health } from "../../../lib/api/health";
import { runs } from "../../../lib/api/runs";
import type { RunDetail, RunOutput } from "../../../lib/types";
import { RUN_WAIT, RUN_WAIT_REASON } from "../../../lib/runner-pool-refusals";
import { makeMacOSVMRun, makeRun } from "../../../../test/factories";
import { OperatorProvider } from "../../wardyn/operator-context";
import { RUN_COCKPIT, RUN_MODE, RUN_OUTPUT } from "../../wardyn/copy";
import { DESKTOP } from "../../wardyn/copy/desktop";
import { TerminalPane } from "./terminal-notice";

vi.mock("../../../lib/api/health", () => ({ health: { health: vi.fn().mockResolvedValue({}) } }));

// The last keepsIn the pane handed the terminal.
const attach = vi.hoisted(() => ({ keepsIn: undefined as boolean | "unreadable" | undefined }));
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: (p: { keepsIn?: boolean | "unreadable" }) => {
    attach.keepsIn = p.keepsIn;
    return null;
  },
}));
vi.mock("../../../lib/api/runner-pools", () => ({
  runnerPools: { list: () => Promise.resolve({ pools: [{ id: "pool-1", name: "CI jobs", slots: { max: 40 } }] }) },
}));
const getOutput = vi.fn();
vi.mock("../../../lib/api/run-output", () => ({ runOutput: { get: (...a: unknown[]) => getOutput(...a) } }));
vi.mock("../../wardyn/terminal-player", () => ({ TerminalPlayer: () => <div data-testid="player" /> }));

const LINK = "Open the Recording tab →";

const kept = (o: Partial<RunOutput> = {}): RunOutput => ({
  output: "$ swift test\nExecuted 42 tests",
  truncated: false,
  complete: true,
  source: "stdout",
  incomplete: false,
  capture_gap: false,
  mask_scope: "run",
  captured_at: aheadByHours(-1),
  ...o,
});
const notKept = () => getOutput.mockRejectedValue(new HttpError(409, "refused", "run_output_not_kept"));
beforeEach(() => {
  getOutput.mockReset();
  notKept();
  // The run's data_out read stays out unless a test answers it.
  vi.spyOn(runs, "getPolicy").mockReturnValue(new Promise(() => {}));
});

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
        onGoOutput={vi.fn()}
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
  ] as const)("%s: same sentence, same name, text-info and not text-primary", async (_n, recState, disabled, text) => {
    const go = pane(recState, disabled);
    expect(await screen.findByText(text)).toBeInTheDocument();
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
  it("scopes the pane frame to the dark tokens", async () => {
    pane("ready", false);
    await act(async () => {});
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
        <TerminalPane run={run} terminal={false} recording={null} recState="idle" recordingDisabled={recordingDisabled} onGoRecording={onGoRecording} onGoOutput={vi.fn()} execMode={false} />
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
          <TerminalPane run={live(over)} terminal={false} recording={null} recState="idle" recordingDisabled={false} onGoRecording={vi.fn()} onGoOutput={vi.fn()} execMode={false} />
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
        <TerminalPane onGoOutput={vi.fn()} run={makeRun({ id: "run-1", created_by: "me", interactive: false, ...over })} terminal={false} recording={null} recState="idle" recordingDisabled={false} onGoRecording={vi.fn()} execMode={false} />
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
        <TerminalPane onGoOutput={vi.fn()} run={makeRun({ id: "run-1", state: "STARTING", created_by: "priya", interactive: true, has_recording: false })} terminal={false} recording={null} recState="idle" recordingDisabled={false} onGoRecording={vi.fn()} execMode={false} />
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

// Watch desktop on the hero of a running macOS VM run whose desktop is view only.
describe("TerminalPane — Watch desktop", () => {
  const paneFor = (over: Record<string, unknown>, principal = "me") =>
    render(
      <OperatorProvider operator={false} principal={principal}>
        <TerminalPane onGoOutput={vi.fn()} run={makeRun({ id: "run-1", state: "RUNNING", created_by: "me", interactive: false, machine_kind: "macos_vm", macos: { desktop: "observe" }, ...over } as Partial<RunDetail>) as RunDetail} terminal={false} recording={null} recState="idle" recordingDisabled={false} onGoRecording={vi.fn()} execMode={false} />
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

  it("is not offered on a background run, whose Watch is the Desktop tile's, and asks for no ticket", async () => {
    paneFor({ experience: "background", background_watch: { state: "open" } });
    await waitFor(() => expect(screen.getByTestId("run-pane-frame")).toBeInTheDocument());
    expect(screen.queryByRole("button", { name: DESKTOP.watch })).toBeNull();
  });

  it("is still offered on a run that says it is interactive, and on one from before the field", async () => {
    paneFor({ experience: "interactive" });
    expect(await screen.findByRole("button", { name: DESKTOP.watch })).toBeInTheDocument();
  });

  it("says when the gateway check failed, and Try again asks again", async () => {
    vi.mocked(health.health).mockResolvedValueOnce({} as never);
    paneFor({});
    expect(await screen.findByText("Couldn't check whether the desktop gateway is on.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("button", { name: DESKTOP.watch })).toBeInTheDocument();
    expect(screen.queryByText("Couldn't check whether the desktop gateway is on.")).toBeNull();
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
        <TerminalPane run={run} terminal={false} recording={null} recState="idle" recordingDisabled={false} onGoRecording={vi.fn()} onGoOutput={vi.fn()} execMode={false} />
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

// #3924: a run with no recording to replay shows the bytes the Output tab reads, read only, in the same frame.
describe("TerminalPane — a run with no recording shows its kept output", () => {
  const mount = async (run: ReturnType<typeof makeRun>, terminal: boolean, recState: "idle" | "loading" | "error" | "ready" = "ready", recording = null as null | object) => {
    const onGoOutput = vi.fn();
    render(
      <OperatorProvider operator principal="me">
        <TerminalPane run={run} terminal={terminal} recording={recording as never} recState={recState} recordingDisabled={false} onGoRecording={vi.fn()} onGoOutput={onGoOutput} execMode={false} />
      </OperatorProvider>,
    );
    await act(async () => {});
    return onGoOutput;
  };
  const mac = (over: Record<string, unknown> = {}) => makeMacOSVMRun({ id: "run-1", state: "RUNNING", created_by: "me", interactive: false, ...over });

  it("a live macOS VM run reads and shows its output, with the tab's chips and a link to the tab", async () => {
    getOutput.mockResolvedValue(kept({ complete: false, captured_at: undefined }));
    const go = await mount(mac(), false, "idle");
    expect(screen.getByTestId("run-pane-output")).toHaveTextContent("Executed 42 tests");
    expect(screen.getByText(RUN_OUTPUT.live)).toBeInTheDocument();
    expect(screen.getByText("Output", { selector: ".label-eyebrow" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: RUN_OUTPUT.openTab }));
    expect(go).toHaveBeenCalledOnce();
  });

  it("the bytes are one text node: markup stays literal", async () => {
    getOutput.mockResolvedValue(kept({ output: "<img src=x onerror=alert(1)>" }));
    await mount(mac(), false, "idle");
    const pre = screen.getByTestId("run-pane-output");
    expect(pre).toHaveTextContent("<img src=x onerror=alert(1)>");
    expect(pre.querySelector("img")).toBeNull();
  });

  it("an ended run whose recording read found none shows the final output, titled Output", async () => {
    getOutput.mockResolvedValue(kept());
    await mount(makeRun({ id: "run-1", state: "COMPLETED", created_by: "me", interactive: false }), true);
    expect(screen.getByTestId("run-pane-output")).toHaveTextContent("Executed 42 tests");
    expect(screen.getByText(RUN_OUTPUT.final)).toBeInTheDocument();
    expect(screen.queryByText(RUN_COCKPIT.recordingMissing)).toBeNull();
  });

  it("says so for empty output, live and final", async () => {
    getOutput.mockResolvedValue(kept({ output: "", complete: false, captured_at: undefined }));
    await mount(mac(), false, "idle");
    expect(screen.getByText(RUN_OUTPUT.emptyLive)).toBeInTheDocument();
  });

  it("an ended run that printed nothing says so", async () => {
    getOutput.mockResolvedValue(kept({ output: "" }));
    await mount(makeRun({ id: "run-1", state: "COMPLETED", created_by: "me", interactive: false }), true);
    expect(screen.getByText(RUN_OUTPUT.emptyFinal)).toBeInTheDocument();
  });

  it("a truncated read says only the end is kept", async () => {
    getOutput.mockResolvedValue(kept({ truncated: true, incomplete: true }));
    await mount(makeRun({ id: "run-1", state: "COMPLETED", created_by: "me", interactive: false }), true);
    expect(screen.getByText(RUN_OUTPUT.truncated)).toBeInTheDocument();
    expect(screen.getByText(RUN_OUTPUT.incomplete)).toBeInTheDocument();
  });

  it("a refused read keeps today's notice", async () => {
    await mount(makeRun({ id: "run-1", state: "COMPLETED", created_by: "me", interactive: false }), true);
    expect(screen.getByText(RUN_COCKPIT.recordingMissing)).toBeInTheDocument();
    expect(screen.queryByTestId("run-pane-output")).toBeNull();
  });

  it("a live macOS VM run whose read is refused keeps the autonomous notice", async () => {
    await mount(mac(), false, "idle");
    expect(screen.queryByTestId("run-pane-output")).toBeNull();
    expect(screen.getByText("Runs unattended — your policy's approval gates still apply.")).toBeInTheDocument();
  });

  it("a pane snapshot or recording-sourced answer is not shown here", async () => {
    getOutput.mockResolvedValue(kept({ source: "recording" }));
    await mount(makeRun({ id: "run-1", state: "COMPLETED", created_by: "me", interactive: false }), true);
    expect(screen.queryByTestId("run-pane-output")).toBeNull();
  });

  it("changes nothing for a run with a recording, a live container run, or an interactive run", async () => {
    getOutput.mockResolvedValue(kept());
    await mount(makeRun({ id: "run-1", state: "COMPLETED", created_by: "me", interactive: false }), true, "ready", { id: "rec" });
    expect(screen.getByTestId("player")).toBeInTheDocument();
    expect(getOutput).not.toHaveBeenCalled();
    await mount(makeRun({ id: "run-2", state: "RUNNING", created_by: "me", interactive: false }), false, "idle");
    expect(getOutput).not.toHaveBeenCalled();
    await mount(mac({ id: "run-3", interactive: true }), false, "idle");
    expect(getOutput).not.toHaveBeenCalled();
  });

  // A live macOS VM run ends before its recording read answers: the pane stays on Output and never shows the
  // live read as the final one.
  it("a macOS VM run that ends keeps the Output pane through the recording read and drops the live read", async () => {
    getOutput.mockResolvedValueOnce(kept({ complete: false, captured_at: undefined, output: "live line" }));
    const ui = (state: string, terminal: boolean, recState: "idle" | "loading" | "ready") => (
      <OperatorProvider operator principal="me">
        <TerminalPane run={mac({ state })} terminal={terminal} recording={null} recState={recState} recordingDisabled={false} onGoRecording={vi.fn()} onGoOutput={vi.fn()} execMode={false} />
      </OperatorProvider>
    );
    const { rerender } = render(ui("RUNNING", false, "idle"));
    await act(async () => {});
    expect(screen.getByTestId("run-pane-output")).toHaveTextContent("live line");

    let finish!: (o: RunOutput) => void;
    getOutput.mockReturnValueOnce(new Promise<RunOutput>((r) => (finish = r)));
    for (const recState of ["idle", "loading"] as const) {
      rerender(ui("COMPLETED", true, recState));
      await act(async () => {});
      expect(screen.queryByText("Recording", { selector: ".label-eyebrow" })).toBeNull();
      expect(screen.queryByText(RUN_COCKPIT.recordingLoading)).toBeNull();
      expect(screen.getByText("Output", { selector: ".label-eyebrow" })).toBeInTheDocument();
      // The read from while the run lived is not shown as the ended run's output.
      expect(screen.queryByTestId("run-pane-output")).toBeNull();
    }
    rerender(ui("COMPLETED", true, "ready"));
    await act(async () => finish(kept({ output: "final line" })));
    expect(screen.getByTestId("run-pane-output")).toHaveTextContent("final line");
    expect(screen.getByText(RUN_OUTPUT.final)).toBeInTheDocument();
  });

  it("a QUEUED macOS VM run reads nothing, with or without a queue on the wire", async () => {
    await mount(mac({ state: "QUEUED" }), false, "idle");
    expect(getOutput).not.toHaveBeenCalled();
  });

  it("a macOS VM run still starting shows its startup steps, not output", async () => {
    await mount(mac({ state: "STARTING" }), false, "idle");
    expect(getOutput).not.toHaveBeenCalled();
  });
});

// A macOS VM run is RUNNING while its setup runs: the strip above the terminal says so, on its own field, and
// goes once setup is done.
describe("TerminalPane — a macOS VM run's setup", () => {
  const live = (over: Partial<RunDetail> = {}) =>
    render(
      <OperatorProvider operator principal="me">
        <TerminalPane
          run={{ ...makeMacOSVMRun({ id: "run-1", state: "RUNNING", created_by: "me", interactive: true }), ...over }}
          terminal={false}
          recording={null}
          recState="idle"
          recordingDisabled={false}
          onGoRecording={vi.fn()}
          onGoOutput={vi.fn()}
          execMode={false}
        />
      </OperatorProvider>,
    );

  it("shows the strip, word for word, while setup runs, whatever mac_window says", () => {
    live({ macos_preparing: true, mac_window: { state: "site_off", message: "off" } });
    expect(screen.getByRole("status")).toHaveTextContent(
      "Setting up this run: cloning the repository and installing the agent's tools. You can use the terminal now. The work folder fills in when setup finishes.",
    );
  });

  it("has no strip once setup is done", () => {
    live();
    expect(screen.queryByTestId("macos-setting-up")).toBeNull();
  });
});

describe("TerminalPane — the terminal's data_out rule", () => {
  function attachable() {
    attach.keepsIn = undefined;
    render(
      <OperatorProvider operator principal="me">
        <TerminalPane run={makeRun({ id: "run-1", state: "RUNNING", created_by: "me", interactive: true })} terminal recording={null} recState="idle" recordingDisabled={false} onGoRecording={vi.fn()} onGoOutput={vi.fn()} execMode={false} />
      </OperatorProvider>,
    );
  }

  it("a failed read of the rule turns the terminal's copying off as unreadable, not as a run that keeps its content in", async () => {
    vi.mocked(runs.getPolicy).mockRejectedValue(new Error("boom"));
    attachable();
    await waitFor(() => expect(attach.keepsIn).toBe("unreadable"));
    expect(runs.getPolicy).toHaveBeenCalledWith("run-1");
  });

  it.each(["review", "blocked"] as const)("a %s rule tells the terminal the run keeps its content in", async (data_out) => {
    vi.mocked(runs.getPolicy).mockResolvedValue({ state: "recorded", spec: { data_out } } as Awaited<ReturnType<typeof runs.getPolicy>>);
    attachable();
    await waitFor(() => expect(attach.keepsIn).toBe(true));
  });

  it("an allowed rule leaves the terminal's copying on", async () => {
    vi.mocked(runs.getPolicy).mockResolvedValue({ state: "recorded", spec: { data_out: "allowed" } } as Awaited<ReturnType<typeof runs.getPolicy>>);
    attachable();
    await waitFor(() => expect(attach.keepsIn).toBe(false));
    await act(async () => {});
    expect(attach.keepsIn).toBe(false);
  });
});
