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
import { render, screen, waitFor, within } from "@testing-library/react";

vi.mock("../../attach-terminal", () => ({
  AttachTerminal: React.forwardRef(function FakeTerminal(_props: unknown, ref: React.ForwardedRef<{ sendText: (t: string) => void }>) {
    React.useImperativeHandle(ref, () => ({ sendText: () => {} }), []);
    return <div data-testid="fake-terminal" />;
  }),
}));
vi.mock("../../../lib/api/model-provider-signin", () => ({
  modelProviderSignIn: { startSignIn: () => Promise.resolve({ runId: "run-123", state: "PENDING" }), captureSignIn: vi.fn() },
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
