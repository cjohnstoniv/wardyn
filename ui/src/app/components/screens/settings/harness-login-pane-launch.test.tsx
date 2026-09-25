/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The launch call's own outcomes, and the blurb the pane puts on screen with
// them (U-8, U-11).
//
// Its own file because harness-login-pane.test.tsx is at the 1000-line gate
// (scripts/check-file-size.sh) — and because this is a different seam: every
// case here ends at the `starting` phase or at the `error` one, with no
// terminal, no capture and no corroboration. What happens AFTER the sandbox is
// up stays in the sibling file.
import * as React from "react";
import { describe, it, expect, vi, beforeEach, type MockInstance } from "vitest";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

// lastAttachOutput captures the onOutput callback the pane hands AttachTerminal
// on its most recent render — the only way to feed it a PTY chunk without a
// real xterm (the door's ready state needs this; most cases in this file
// never get far enough to mount the terminal at all).
let lastAttachOutput: ((chunk: string) => void) | undefined;
// The pane reaches AttachTerminal (and through it xterm's stylesheet); no case
// here gets far enough to mount it, but the import itself has to resolve.
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: React.forwardRef(function FakeTerminal(
    props: { onOutput?: (chunk: string) => void; autoRun?: string },
    ref: React.ForwardedRef<{ sendText: (t: string) => void }>,
  ) {
    lastAttachOutput = props.onOutput;
    React.useImperativeHandle(ref, () => ({ sendText: () => {} }), []);
    return <div data-testid="fake-terminal" />;
  }),
}));
const harnessLoginMock = vi.fn();
vi.mock("../../../lib/api/harness-auth", () => ({
  harnessAuth: {
    harnessLogin: (...a: unknown[]) => harnessLoginMock(...a),
    harnessCredentialPaste: vi.fn(),
  },
}));
vi.mock("../../../lib/api/runs", () => ({ runs: { killRun: vi.fn(), getRun: vi.fn() } }));
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: vi.fn() } }));

import { HarnessLoginPane, type HarnessLoginPaneHandle } from "./harness-login-pane";
import { AWS_BLURB_MANAGED_OPENING } from "./login-pane-copy";
import { HttpError } from "../../../lib/api/core";
import { runs as runsApiMocked } from "../../../lib/api/runs";
import { makeRun } from "../../../../test/factories";
import { LOGIN_SANDBOX_SLOW_START, LOGIN_SANDBOX_STUCK_LEAD_IN, RUN_POLL_SLOW_START_MS } from "./login-start-wait";
import { STARTING_CONTAINER_CREATING } from "../run-status-detail";
import { SIGNIN_PROGRESS } from "./login-pane-copy";

// U-11 (W6 blind lens) — the no-credential member preview offers "Sign in to
// AWS" (Getting Started renders the CTA off a not_configured state) and the
// launch is then refused with a deterministic 409 (harnesscred_launch.go). The
// pane answered with "Try again", which earns the identical refusal: the reader
// has to leave the preview, and nothing on that screen said so.
describe("a REFUSED launch offers no retry (U-11)", () => {
  beforeEach(() => {
    harnessLoginMock.mockReset();
    vi.mocked(runsApiMocked.killRun).mockReset().mockResolvedValue(undefined);
    vi.mocked(runsApiMocked.getRun).mockReset();
  });

  it("a 409 shows the refusal and NO Try again", async () => {
    harnessLoginMock.mockRejectedValue(new HttpError(409, "sign-in is refused in the member preview"));
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));

    expect(await screen.findByRole("alert")).toHaveTextContent("sign-in is refused in the member preview");
    expect(screen.queryByRole("button", { name: /try again/i })).not.toBeInTheDocument();
    // …and the way out is still there.
    expect(screen.getByRole("button", { name: /cancel/i })).toBeInTheDocument();
  });

  // The bound: every OTHER failure is a failure, and retrying one is the right
  // move — a 500, a dropped socket, a bad start URL.
  it("a 500 keeps Try again", async () => {
    harnessLoginMock.mockRejectedValue(new HttpError(500, "control plane unreachable"));
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));

    expect(await screen.findByRole("alert")).toHaveTextContent("control plane unreachable");
    expect(screen.getByRole("button", { name: /try again/i })).toBeInTheDocument();
  });

  // …and a retry that then WORKS clears the refusal rather than carrying it.
  it("Try again after a 500 relaunches", async () => {
    harnessLoginMock.mockRejectedValueOnce(new HttpError(500, "control plane unreachable"));
    harnessLoginMock.mockResolvedValue("run-123");
    vi.mocked(runsApiMocked.getRun).mockResolvedValue(makeRun({ id: "run-123", state: "PENDING" }));
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));
    await screen.findByRole("alert");
    await userEvent.click(screen.getByRole("button", { name: /try again/i }));
    expect(await screen.findByTestId("login-sandbox-starting")).toBeInTheDocument();
  });
});

// U-8 (W6 blind lens) — the aws blurb asked the reader to give Wardyn their
// organization's access portal URL, under a managed row where there is no field,
// the server ignores a supplied one, and the intro one line above has just said
// there is nothing to enter.
describe("the aws blurb under a managed access portal (U-8)", () => {
  async function blurbAfterStart(startURLManaged: boolean) {
    harnessLoginMock.mockReset().mockResolvedValue("run-123");
    vi.mocked(runsApiMocked.getRun)
      .mockReset()
      .mockResolvedValue(makeRun({ id: "run-123", state: "PENDING" }));
    const { container } = render(
      <HarnessLoginPane
        provider="aws"
        startURLManaged={startURLManaged}
        onDone={vi.fn()}
        onCancel={vi.fn()}
      />,
    );
    if (!startURLManaged) {
      await userEvent.type(screen.getByLabelText("AWS access portal start URL"), "https://acme.awsapps.com/start");
    }
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));
    await screen.findByTestId("login-sandbox-starting");
    return container.textContent ?? "";
  }

  it("a managed row says the portal is already set, never asks for it", async () => {
    const text = await blurbAfterStart(true);
    expect(text).toContain(AWS_BLURB_MANAGED_OPENING);
    expect(text).not.toContain("Give Wardyn your organization");
    // The rest of the blurb is unchanged — same sandbox, same config, same command.
    expect(text).toContain("holding just that URL and the configured SSO region");
  });

  it("the ordinary Settings flow still asks for it", async () => {
    const text = await blurbAfterStart(false);
    expect(text).toContain("Give Wardyn your organization");
    expect(text).not.toContain(AWS_BLURB_MANAGED_OPENING);
  });
});

// 0.7.6 finding 6: the wait ends on the REASON, not on the clock. "The first
// time, it produced a 'Could not reach the control plane' error and a wrong
// diagnosis; the second time we only stayed calm because we had measured it
// before."
describe("the wait reads the substrate's reason (finding 6)", () => {
  beforeEach(() => {
    harnessLoginMock.mockReset().mockResolvedValue("run-123");
    vi.mocked(runsApiMocked.killRun).mockReset().mockResolvedValue(undefined);
    vi.mocked(runsApiMocked.getRun).mockReset();
  });

  // #628 state 7: an image that will not pull is the DOWNLOAD step failing.
  // The door shows the server's detail as is and offers Retry, which starts a
  // fresh sandbox — and stops the stuck one, which is still STARTING.
  it("a STARTING run on an unpullable image fails the download step, verbatim, with Retry", async () => {
    const detail = "agent: ImagePullBackOff: rpc error: code = Unknown desc = pull access denied";
    vi.mocked(runsApiMocked.getRun).mockResolvedValue(makeRun({
      id: "run-123",
      state: "STARTING",
      status_detail: detail,
      status_reason: "ImagePullBackOff",
    }));
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));

    const alertBox = await screen.findByRole("alert");
    expect(alertBox).toHaveTextContent(detail);
    expect(alertBox).not.toHaveTextContent(LOGIN_SANDBOX_STUCK_LEAD_IN);
    const steps = screen.getByTestId("signin-progress");
    expect(steps).toHaveTextContent(SIGNIN_PROGRESS.STEP_DOWNLOAD_FAILED);
    expect(steps).not.toHaveTextContent(SIGNIN_PROGRESS.STEP_WAIT("AWS"));
    expect(screen.getByRole("button", { name: SIGNIN_PROGRESS.CANCEL })).toBeInTheDocument();

    harnessLoginMock.mockResolvedValue("run-456");
    vi.mocked(runsApiMocked.getRun).mockResolvedValue(makeRun({ id: "run-456", state: "PENDING" }));
    await userEvent.click(screen.getByRole("button", { name: SIGNIN_PROGRESS.RETRY }));
    expect(runsApiMocked.killRun).toHaveBeenCalledWith("run-123");
    expect(await screen.findByTestId("login-sandbox-starting")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  // Docker names no terminal pull reason: its pull fails the run outright. The
  // step the run was on when it ended is what says the download failed.
  it("a run that fails while Pulling fails the download step with the run's own sentence", async () => {
    vi.mocked(runsApiMocked.getRun)
      .mockResolvedValueOnce(makeRun({
        id: "run-123",
        state: "STARTING",
        status_detail: "image: Pulling: ghcr.io/example/agent-aws-sso:0.8.0",
        status_reason: "Pulling",
      }))
      .mockResolvedValue(makeRun({
        id: "run-123",
        state: "FAILED",
        failure_hint: "docker: pull ghcr.io/example/agent-aws-sso:0.8.0: manifest unknown",
      }));
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));

    const starting = await screen.findByTestId("login-sandbox-starting");
    expect(starting).toHaveTextContent(SIGNIN_PROGRESS.STEP_DOWNLOAD_ACTIVE);
    expect(starting).toHaveTextContent(SIGNIN_PROGRESS.DOWNLOAD_HINT);

    const alertBox = await screen.findByRole("alert", {}, { timeout: 5000 });
    expect(alertBox).toHaveTextContent("docker: pull ghcr.io/example/agent-aws-sso:0.8.0: manifest unknown");
    expect(screen.getByTestId("signin-progress")).toHaveTextContent(SIGNIN_PROGRESS.STEP_DOWNLOAD_FAILED);
    expect(screen.getByRole("button", { name: SIGNIN_PROGRESS.RETRY })).toBeInTheDocument();
  });

  // The other terminal reasons are not the download: the image is here, and
  // the container will not start from it. Those keep the stuck lead-in and
  // offer no retry — it earns the identical answer until an admin acts.
  it("a container that will not start keeps the stuck lead-in and offers Cancel ONLY", async () => {
    vi.mocked(runsApiMocked.getRun).mockResolvedValue(makeRun({
      id: "run-123",
      state: "STARTING",
      status_detail: "agent: CrashLoopBackOff: back-off restarting failed container",
      status_reason: "CrashLoopBackOff",
    }));
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));

    const alertBox = await screen.findByRole("alert");
    expect(alertBox).toHaveTextContent(LOGIN_SANDBOX_STUCK_LEAD_IN);
    expect(screen.queryByRole("button", { name: /try again|retry/i })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /cancel/i })).toBeInTheDocument();
  });

  // Codex #11: dispatch marks the run FAILED the instant waitContainerRunning
  // errors, which can land between two of the pane's polls. The server keeps a
  // TERMINAL reason on a FAILED run precisely so this branch still says why.
  it("the same run caught already FAILED says the same thing", async () => {
    vi.mocked(runsApiMocked.getRun).mockResolvedValue(makeRun({
      id: "run-123",
      state: "FAILED",
      failure_hint: "the sandbox could not be created: agent container stuck waiting (ImagePullBackOff): denied",
      status_detail: "agent: ImagePullBackOff: rpc error: code = Unknown desc = pull access denied",
      status_reason: "ImagePullBackOff",
    }));
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));

    const alertBox = await screen.findByRole("alert");
    expect(alertBox).toHaveTextContent("agent: ImagePullBackOff: rpc error: code = Unknown desc = pull access denied");
    expect(screen.getByTestId("signin-progress")).toHaveTextContent(SIGNIN_PROGRESS.STEP_DOWNLOAD_FAILED);
    expect(screen.getByRole("button", { name: SIGNIN_PROGRESS.RETRY })).toBeInTheDocument();
  });

  // The other half of the field report's sentence: "ContainerCreating for two
  // minutes is normal". The clock still says 'slow' — the sentence is the
  // substrate's, not a hedged guess from the pane.
  it("ContainerCreating past the slow window reads as the ordinary first start", async () => {
    vi.mocked(runsApiMocked.getRun).mockResolvedValue(makeRun({
      id: "run-123",
      state: "STARTING",
      status_detail: "agent: ContainerCreating",
      status_reason: "ContainerCreating",
    }));
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
      await userEvent.click(screen.getByRole("button", { name: /start login/i }));
      await screen.findByTestId("login-sandbox-starting");
      await act(async () => {
        await vi.advanceTimersByTimeAsync(RUN_POLL_SLOW_START_MS + 5_000);
      });
      const block = screen.getByTestId("login-sandbox-starting");
      expect(block).toHaveTextContent(STARTING_CONTAINER_CREATING);
      expect(block).not.toHaveTextContent(LOGIN_SANDBOX_SLOW_START);
      // Still a wait, not an ending.
      expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  // The regression pin: a run with NO reason — a warm docker image, a pre-0.7.6
  // daemon — grades exactly as 0.7.5 did.
  it("with no reason at all, the 0.7.5 slow-start sentence still stands", async () => {
    vi.mocked(runsApiMocked.getRun).mockResolvedValue(makeRun({ id: "run-123", state: "STARTING" }));
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
      await userEvent.click(screen.getByRole("button", { name: /start login/i }));
      await screen.findByTestId("login-sandbox-starting");
      await act(async () => {
        await vi.advanceTimersByTimeAsync(RUN_POLL_SLOW_START_MS + 5_000);
      });
      expect(screen.getByTestId("login-sandbox-starting")).toHaveTextContent(LOGIN_SANDBOX_SLOW_START);
    } finally {
      vi.useRealTimers();
    }
  });
});

// The terminal status write is the one a 500ms deadline may drop, so
// a FAILED run can reach the pane with the reason only in failure_hint. The
// server rebuilds status_detail from that hint — but the pane must never put its
// stuck lead-in in front of an EMPTY sentence whatever it is handed, and a
// pre-0.7.6 daemon hands it the hint alone.
describe("a terminal ending that arrived only as a failure_hint", () => {
  beforeEach(() => {
    harnessLoginMock.mockReset().mockResolvedValue("run-123");
    vi.mocked(runsApiMocked.killRun).mockReset().mockResolvedValue(undefined);
    vi.mocked(runsApiMocked.getRun).mockReset();
  });

  it("the server's rebuilt detail reads exactly like the run that kept it", async () => {
    vi.mocked(runsApiMocked.getRun).mockResolvedValue(makeRun({
      id: "run-123",
      state: "FAILED",
      // What projectStatusDetail rebuilds from the hint when the row held nothing.
      status_detail: "agent: ImagePullBackOff: rpc error: pull access denied",
      status_reason: "ImagePullBackOff",
      failure_hint:
        "the sandbox could not be created: agent container stuck waiting (ImagePullBackOff): rpc error: pull access denied",
    }));
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));

    const alertBox = await screen.findByRole("alert");
    expect(alertBox).toHaveTextContent("agent: ImagePullBackOff: rpc error: pull access denied");
    expect(screen.getByRole("button", { name: SIGNIN_PROGRESS.RETRY })).toBeInTheDocument();
  });

  // The old daemon: a reason and a hint, no detail. Never a lead-in with nothing
  // after it — the run's own sentence is better than a promise with no words.
  it("falls through to the run's own sentence rather than promising words it has not got", async () => {
    vi.mocked(runsApiMocked.getRun).mockResolvedValue(makeRun({
      id: "run-123",
      state: "FAILED",
      failure_hint:
        "the sandbox could not be created: agent container stuck waiting (ImagePullBackOff): rpc error: pull access denied",
    }));
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));

    const alertBox = await screen.findByRole("alert");
    expect(alertBox).toHaveTextContent("pull access denied");
    expect(alertBox.textContent ?? "").not.toMatch(new RegExp(`${LOGIN_SANDBOX_STUCK_LEAD_IN}\\s*$`));
  });
});


// The launch-after-dismiss race, and the handle that makes a dismissal from
// outside the pane reach the run it created (0.7.6, Codex #15).
//
// The model-access door mounts this pane in a Dialog, whose Escape / overlay
// click closes the PARENT — and `onCancel` is child-to-parent, so it cannot
// reach back in to kill the login run. Worse, harnessLogin's POST answers with
// the id AFTER dispatch has already created the run, so a cancellation landing
// mid-flight would see `runId === null`, kill nothing, and leave a "wardyn:
// sign-in running" run on the member's board for up to 30 minutes.
describe("a dismissal from outside the pane still ends the login run", () => {
  beforeEach(() => {
    harnessLoginMock.mockReset();
    vi.mocked(runsApiMocked.killRun).mockReset().mockResolvedValue(undefined);
    // Deliberately absent: these cases exercise the pane's `if (!run) return;`
    // guard, not a real run shape.
    vi.mocked(runsApiMocked.getRun).mockReset().mockResolvedValue(undefined as never);
  });

  it("ref.cancel() kills the run the pane is holding", async () => {
    harnessLoginMock.mockResolvedValue("run-abc");
    const onCancel = vi.fn();
    const ref = React.createRef<HarnessLoginPaneHandle>();
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={onCancel} paneRef={ref} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));
    await screen.findByTestId("login-sandbox-starting");

    await act(async () => ref.current?.cancel());
    expect(runsApiMocked.killRun).toHaveBeenCalledWith("run-abc");
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("a cancel DURING the launch POST kills the run that POST created", async () => {
    let answer: (id: string) => void = () => {};
    harnessLoginMock.mockReturnValue(new Promise<string>((resolve) => (answer = resolve)));
    const ref = React.createRef<HarnessLoginPaneHandle>();
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} paneRef={ref} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));

    // Dismissed while the POST is still in flight: nothing has an id yet.
    await act(async () => ref.current?.cancel());
    expect(runsApiMocked.killRun).not.toHaveBeenCalled();

    await act(async () => {
      answer("run-born-orphaned");
    });
    expect(runsApiMocked.killRun).toHaveBeenCalledWith("run-born-orphaned");
  });
});

// #628 / Finding 7a: the click that starts the sign-in opens NO tab. The tab
// opens only from the door's Open button, once the provider's page exists —
// a fresh gesture, so no popup blocker has a reason to refuse it.
describe("the provider tab opens only from the Open button (#628)", () => {
  let openSpy: MockInstance<typeof window.open>;
  let fakeWindow: { opener: unknown; location: { href: string } };
  const DEVICE_URL = "https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH";

  beforeEach(() => {
    harnessLoginMock.mockReset().mockResolvedValue("run-123");
    lastAttachOutput = undefined;
    vi.mocked(runsApiMocked.killRun).mockReset().mockResolvedValue(undefined);
    vi.mocked(runsApiMocked.getRun).mockReset().mockResolvedValue(makeRun({ id: "run-123", state: "RUNNING" }));
    fakeWindow = { opener: {}, location: { href: "" } };
    // vitest 4: re-spying an already-spied global returns the SAME mock
    // instance, so clear its history per test.
    openSpy = vi.spyOn(window, "open").mockClear().mockReturnValue(fakeWindow as unknown as Window);
  });

  async function attachAws() {
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));
    await screen.findByTestId("fake-terminal");
  }

  it("Start login opens nothing, and the door waits on AWS until the link arrives", async () => {
    await attachAws();
    expect(openSpy).not.toHaveBeenCalled();
    expect(screen.getByTestId("signin-progress")).toHaveTextContent(SIGNIN_PROGRESS.STEP_WAIT("AWS"));
    expect(screen.getByText(SIGNIN_PROGRESS.WAIT_HINT("AWS"))).toBeInTheDocument();
  });

  it("the link arriving opens nothing either — it arms the Open button, with the device code beside it", async () => {
    await attachAws();
    await act(async () => lastAttachOutput?.(`${DEVICE_URL}\n`));
    expect(openSpy).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: SIGNIN_PROGRESS.OPEN("AWS") })).toBeInTheDocument();
    expect(screen.getByTestId("signin-device-code")).toHaveTextContent("ABCD-EFGH");
    expect(screen.getByTestId("signin-ready")).toHaveTextContent(DEVICE_URL);
  });

  it("the Open click opens the tab, severs opener, navigates it, and moves to the opened state", async () => {
    await attachAws();
    await act(async () => lastAttachOutput?.(`${DEVICE_URL}\n`));
    await userEvent.click(screen.getByRole("button", { name: SIGNIN_PROGRESS.OPEN("AWS") }));
    expect(openSpy).toHaveBeenCalledWith("", "_blank");
    expect(fakeWindow.opener).toBeNull();
    expect(fakeWindow.location.href).toBe(DEVICE_URL);
    expect(screen.getByTestId("signin-tab-open")).toHaveTextContent(SIGNIN_PROGRESS.TAB_OPEN("AWS"));
    expect(screen.getByTestId("signin-device-code")).toHaveTextContent("ABCD-EFGH");

    await userEvent.click(screen.getByRole("button", { name: SIGNIN_PROGRESS.REOPEN }));
    expect(openSpy).toHaveBeenCalledTimes(2);
  });

  it("a blocked Open stays on the ready state, whose copy-link fallback is already there", async () => {
    openSpy.mockReturnValue(null);
    await attachAws();
    await act(async () => lastAttachOutput?.(`${DEVICE_URL}\n`));
    await userEvent.click(screen.getByRole("button", { name: SIGNIN_PROGRESS.OPEN("AWS") }));
    expect(screen.queryByTestId("signin-tab-open")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: SIGNIN_PROGRESS.COPY_LINK })).toBeInTheDocument();
  });

  it("the Claude door has the same ready state, with no device code and no printed link", async () => {
    const oauth = "https://claude.ai/oauth/authorize?code=true&client_id=abc&response_type=code&state=xyz";
    render(<HarnessLoginPane provider="anthropic" onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));
    await screen.findByTestId("fake-terminal");
    expect(screen.getByTestId("signin-progress")).toHaveTextContent(SIGNIN_PROGRESS.STEP_WAIT("Claude"));
    await act(async () => lastAttachOutput?.(`${oauth}\n`));
    expect(openSpy).not.toHaveBeenCalled();
    expect(screen.queryByTestId("signin-device-code")).not.toBeInTheDocument();
    expect(screen.getByTestId("signin-ready")).not.toHaveTextContent(oauth);
    await userEvent.click(screen.getByRole("button", { name: SIGNIN_PROGRESS.OPEN("Claude") }));
    expect(fakeWindow.location.href).toBe(oauth);
    expect(screen.getByTestId("signin-tab-open")).toHaveTextContent(SIGNIN_PROGRESS.TAB_OPEN("Claude"));
  });
});
