/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The AWS sign-in dialog's step list follows each witness the dialog has that
// the sandbox is up (#3235). The device code is read on its own loop (the
// sign-in read), so a dialog showing a code while the run read is stale and the
// run.interactive.start audit row is absent, or its read refused, must still
// put the list on "Waiting for AWS" rather than on step 1.
import * as React from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("../../attach-terminal", () => ({
  AttachTerminal: React.forwardRef(function FakeTerminal(_props: unknown, ref: React.ForwardedRef<{ sendText: (t: string) => void }>) {
    React.useImperativeHandle(ref, () => ({ sendText: () => {} }), []);
    return <div data-testid="fake-terminal" />;
  }),
}));
const startSignInMock = vi.fn();
vi.mock("../../../lib/api/model-provider-signin", () => ({
  modelProviderSignIn: { startSignIn: (...a: unknown[]) => startSignInMock(...a), captureSignIn: vi.fn() },
}));
vi.mock("../../../lib/api/runs", () => ({ runs: { killRun: vi.fn(), getRun: vi.fn() } }));
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: vi.fn() } }));
const listAuditMock = vi.fn();
vi.mock("../../../lib/api/audit", () => ({ audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) } }));
vi.mock("../../../lib/api/run-sign-in", () => ({ runSignIn: { get: vi.fn() } }));

import { HarnessLoginPane } from "./harness-login-pane";
import { HttpError } from "../../../lib/api/core";
import { runs as runsApiMocked } from "../../../lib/api/runs";
import { runSignIn } from "../../../lib/api/run-sign-in";
import { OperatorProvider } from "../../wardyn/operator-context";
import { makeRun } from "../../../../test/factories";
import { SIGNIN_PROGRESS } from "./login-pane-copy";

const WAITING = { state: "waiting", user_code: "KHDW-PQRS", verification_url: "https://device.sso.us-east-1.amazonaws.com/?user_code=KHDW-PQRS" };
const INTERACTIVE_START = { id: "a1", time: new Date().toISOString(), actor_type: "system", actor: "wardynd", action: "run.interactive.start", outcome: "success" };

function startAws() {
  render(
    <OperatorProvider operator={false} principal="member@example.com">
      <HarnessLoginPane modelProvider="bedrock-prod" provider="aws" onDone={vi.fn()} onCancel={vi.fn()} />
    </OperatorProvider>,
  );
}

function mark(label: string) {
  const row = within(screen.getByTestId("signin-progress"))
    .getAllByRole("listitem")
    .find((li) => li.textContent?.includes(label));
  return row?.getAttribute("data-state");
}

describe("HarnessLoginPane — the starting step list follows the dialog's own witnesses (#3235)", () => {
  beforeEach(() => {
    startSignInMock.mockReset().mockResolvedValue({ runId: "run-123", state: "PENDING" });
    vi.mocked(runsApiMocked.killRun).mockReset().mockResolvedValue(undefined);
    // The run read stays stale for the whole wait: it never says RUNNING.
    vi.mocked(runsApiMocked.getRun).mockReset().mockResolvedValue(makeRun({ id: "run-123", state: "PENDING" }));
    listAuditMock.mockReset().mockResolvedValue([]);
    vi.mocked(runSignIn.get).mockReset().mockResolvedValue({ state: "not_waiting" });
  });

  it.each([
    ["has no run.interactive.start row", () => listAuditMock.mockResolvedValue([])],
    ["is refused", () => listAuditMock.mockRejectedValue(new HttpError(403, "Forbidden"))],
  ])("a device code on screen moves the list to the waiting step while the audit read %s", async (_name, auditAnswer) => {
    auditAnswer();
    vi.mocked(runSignIn.get).mockResolvedValue(WAITING);
    startAws();

    expect(await screen.findByTestId("signin-device-code")).toHaveTextContent("KHDW-PQRS");
    await waitFor(() => expect(mark(SIGNIN_PROGRESS.STEP_WAIT("AWS"))).toBe("active"));
    expect(mark(SIGNIN_PROGRESS.STEP_START)).toBe("done");
    expect(mark(SIGNIN_PROGRESS.STEP_DOWNLOAD)).toBe("done");
  });

  it("the audit row alone moves the list to the waiting step", async () => {
    listAuditMock.mockResolvedValue([INTERACTIVE_START]);
    startAws();

    expect(await screen.findByText(SIGNIN_PROGRESS.WAIT_HINT("AWS"))).toBeInTheDocument();
    expect(mark(SIGNIN_PROGRESS.STEP_WAIT("AWS"))).toBe("active");
    expect(screen.queryByTestId("signin-device-code")).not.toBeInTheDocument();
  });

  it("a pull reason alone holds the list on the download step", async () => {
    vi.mocked(runsApiMocked.getRun).mockResolvedValue(makeRun({ id: "run-123", state: "PENDING", status_reason: "Pulling" }));
    startAws();

    expect(await screen.findByText(SIGNIN_PROGRESS.DOWNLOAD_HINT)).toBeInTheDocument();
    expect(mark(SIGNIN_PROGRESS.STEP_START)).toBe("done");
    expect(mark(SIGNIN_PROGRESS.STEP_DOWNLOAD_ACTIVE)).toBe("active");
    expect(mark(SIGNIN_PROGRESS.STEP_WAIT("AWS"))).toBe("pending");
  });
});

// A retry kills the old sandbox without waiting, and the old run's id stays current until the new start answers.
// The old run can still answer "waiting" in that window: its code is never the new run's (#3407).
describe("HarnessLoginPane — a retry's progress follows the new run only (#3407)", () => {
  beforeEach(() => {
    startSignInMock.mockReset().mockResolvedValue({ runId: "run-old", state: "PENDING" });
    vi.mocked(runsApiMocked.killRun).mockReset().mockResolvedValue(undefined);
    listAuditMock.mockReset().mockResolvedValue([]);
    vi.mocked(runsApiMocked.getRun)
      .mockReset()
      .mockImplementation(async (id: string) =>
        id === "run-old"
          ? makeRun({ id, state: "STARTING", status_detail: "agent: ImagePullBackOff", status_reason: "ImagePullBackOff" })
          : makeRun({ id, state: "PENDING", status_reason: "Pulling" }),
      );
  });

  // Fails the old run on its image, retries with the new start held, lets the old run answer "waiting", then lets
  // the new run's id land.
  async function retryWhileOldRunWaits(newRunSignIn: object) {
    vi.mocked(runSignIn.get).mockReset().mockImplementation(async (id: string) => (id === "run-old" ? WAITING : newRunSignIn) as never);
    startAws();
    const retry = await screen.findByRole("button", { name: SIGNIN_PROGRESS.RETRY });
    let land!: (v: { runId: string; state: string }) => void;
    startSignInMock.mockReturnValueOnce(new Promise((r) => (land = r)));
    await userEvent.click(retry);
    expect(await screen.findByTestId("login-sandbox-starting")).toBeInTheDocument();
    // The launching window: the step list is not on the old run's code.
    await act(async () => {});
    expect(mark(SIGNIN_PROGRESS.STEP_WAIT("AWS"))).toBe("pending");
    await act(async () => land({ runId: "run-new", state: "PENDING" }));
  }

  it("the new run pulling holds the list on the download step, with no code from the old run", async () => {
    await retryWhileOldRunWaits({ state: "not_waiting" });
    expect(await screen.findByText(SIGNIN_PROGRESS.DOWNLOAD_HINT)).toBeInTheDocument();
    expect(mark(SIGNIN_PROGRESS.STEP_DOWNLOAD_ACTIVE)).toBe("active");
    expect(mark(SIGNIN_PROGRESS.STEP_WAIT("AWS"))).toBe("pending");
    expect(screen.queryByTestId("signin-device-code")).not.toBeInTheDocument();
  });

  it("the new run's own code still moves the list to the waiting step", async () => {
    await retryWhileOldRunWaits(WAITING);
    await waitFor(() => expect(mark(SIGNIN_PROGRESS.STEP_WAIT("AWS"))).toBe("active"));
    expect(screen.getByTestId("signin-device-code")).toHaveTextContent("KHDW-PQRS");
  });
});
