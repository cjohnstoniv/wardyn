/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { act } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import {
  isLikelyStartUrl,
  serverConfirmsCapture,
  HarnessLoginPane,
  loginFlow,
  SELFRUN_MARKER,
  LOGIN_SANDBOX_UNREADABLE,
} from "./harness-login-pane";
import { SIGNIN_PROGRESS } from "./login-pane-copy";
import { LOGIN_SANDBOX_READ_RETRYING, LOGIN_SANDBOX_SLOW_START } from "./login-start-wait";
import { runs as runsApiMocked } from "../../../lib/api/runs";
import type { SetupStatus } from "../../../lib/types";
import { makeRun } from "../../../../test/factories";

describe("isLikelyStartUrl", () => {
  it("accepts a real AWS access portal URL", () => {
    expect(isLikelyStartUrl("https://my-org.awsapps.com/start")).toBe(true);
    expect(isLikelyStartUrl("  https://identitycenter.amazonaws.com/ssoins-abc  ")).toBe(true);
  });

  it("rejects what the server would also reject", () => {
    expect(isLikelyStartUrl("")).toBe(false);
    expect(isLikelyStartUrl("my-org.awsapps.com/start")).toBe(false);
    expect(isLikelyStartUrl("http://my-org.awsapps.com/start")).toBe(false);
    // A newline would smuggle extra keys into the generated ~/.aws/config INI.
    expect(isLikelyStartUrl("https://\nsso_region = x")).toBe(false);
  });
});

// doneLabel is per-provider: a hardcoded "your Claude subscription is
// connected" regardless of provider would end an AWS SSO capture with the
// same Anthropic-only claim.
describe("loginFlow doneLabel", () => {
  it("names the provider actually connected, not always Claude", () => {
    expect(loginFlow("anthropic").doneLabel).toMatch(/claude subscription/i);
    expect(loginFlow("aws").doneLabel).toMatch(/aws sso/i);
    expect(loginFlow("aws").doneLabel).not.toMatch(/claude subscription/i);
  });
});

// The consent gate.
//
// Owner report, verbatim: "you see a dialog then all of a sudden terminal then
// all of a sudden a popup asking for auth. We should alert the user before
// this happens what to expect and what's required from them." Nothing
// launches the sandbox except Start login.

// lastAttachOutput captures the onOutput callback the pane hands AttachTerminal
// on its most recent render, so a test can feed it PTY chunks directly — the
// only way to exercise handleOutput's marker-watching without a real xterm
// (which does not render in jsdom).
let lastAttachOutput: ((chunk: string) => void) | undefined;
// The props of that same render — the login pane's whole job is to type ONE
// chained command into the PTY, so what it hands the terminal is a contract.
let lastAttachProps: { onOutput?: (chunk: string) => void; autoRun?: string } | undefined;
// The aws sandbox runs the chained command ITSELF now, so the pane's only
// remaining typing path is its grace-window fallback through this handle — the
// same `sendText` the pasted-code field uses. Spied, because "did the console
// type a SECOND login into a sandbox already running one" is the whole question.
const sendTextSpy = vi.fn();
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: React.forwardRef(function FakeTerminal(
    props: { onOutput?: (chunk: string) => void; autoRun?: string },
    ref: React.ForwardedRef<{ sendText: (t: string) => void }>,
  ) {
    lastAttachOutput = props.onOutput;
    lastAttachProps = props;
    React.useImperativeHandle(ref, () => ({ sendText: (t: string) => sendTextSpy(t) }), []);
    return <div data-testid="fake-terminal" />;
  }),
}));
const harnessLoginMock = vi.fn();
const harnessPasteMock = vi.fn();
vi.mock("../../../lib/api/harness-auth", () => ({
  harnessAuth: {
    harnessLogin: (...a: unknown[]) => harnessLoginMock(...a),
    harnessCredentialPaste: (...a: unknown[]) => harnessPasteMock(...a),
  },
}));
vi.mock("../../../lib/api/runs", () => ({ runs: { killRun: vi.fn(), getRun: vi.fn() } }));
const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({ setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) } }));
// review-1 S1: the watch polls the run's audit trail as a hint; defaulted to
// "no hint" so the 4 rewritten S-13 pins exercise its 30s STATUS fallback.
const listAuditMock = vi.fn();
vi.mock("../../../lib/api/audit", () => ({ audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) } }));

// S-13: serverConfirmsCapture is the pure predicate the component's
// confirmCapture wires to getSetupStatus — covered directly so every branch
// (harness row, model_access state, and the anthropic flow's narrower rule)
// is pinned without going through a rendered pane.
describe("serverConfirmsCapture", () => {
  function status(overrides: Partial<SetupStatus>): SetupStatus {
    return {
      ready: true,
      checks: [],
      auth: { mode: "local", local_loopback: true },
      runner: { driver: "docker", confinement_classes: [] },
      providers: [],
      secrets: { present: [], github_app: false },
      age_key: { durable: false },
      has_runs: false,
      platform: { os: "linux", wsl: false },
      ...overrides,
    };
  }

  // R-1: the ONE fact that says "this sign-in captured something" rather than
  // "a credential exists". source_run_id is stamped server-side from the login
  // run's own token claims (ssotoken.go), so the sandbox cannot write it.
  it("aws: confirmed by a harness row stamped with THIS run", () => {
    expect(
      serverConfirmsCapture(status({ harness: [{ provider: "aws", captured: true, source_run_id: "run-123" }] }), "aws", "run-123"),
    ).toBe(true);
  });

  // R-1, the reconnect: a PREVIOUS sign-in's credential satisfies every
  // presence predicate, and is not this run's capture.
  it("aws: a row from a PREVIOUS run is refused even when model_access is live", () => {
    expect(
      serverConfirmsCapture(
        status({ harness: [{ provider: "aws", captured: true, source_run_id: "run-earlier" }], model_access: { state: "live" } }),
        "aws",
        "run-123",
      ),
    ).toBe(false);
  });

  // The MEMBER-REDACTED shape — {provider, captured, expired,
  // source_run_id} and nothing else (setup.go's redactSetupStatusForUser).
  it("aws: the member-redacted row shape confirms on its own", () => {
    expect(
      serverConfirmsCapture(
        status({ harness: [{ provider: "aws", captured: true, expired: false, source_run_id: "run-123" }] }),
        "aws",
        "run-123",
      ),
    ).toBe(true);
  });

  // R-2: for aws the RULE is live/expiring on model_access. A harness row is a
  // presence bit that stays true for a dead credential, so on its own it only
  // widens what a forged marker can land on.
  it("aws: a presence-only harness row is NOT enough — model_access decides", () => {
    expect(serverConfirmsCapture(status({ harness: [{ provider: "aws", captured: true }] }), "aws", "run-123")).toBe(false);
    expect(
      serverConfirmsCapture(
        status({ harness: [{ provider: "aws", captured: true }], model_access: { state: "live" } }),
        "aws",
        "run-123",
      ),
    ).toBe(true);
  });

  // An EXPIRED row is exactly the shape the dropped leg used to admit.
  it("aws: an expired harness row confirms nothing", () => {
    expect(
      serverConfirmsCapture(
        status({ harness: [{ provider: "aws", captured: true, expired: true }], model_access: { state: "expired_signin" } }),
        "aws",
        "run-123",
      ),
    ).toBe(false);
  });

  it("aws: confirmed by model_access state live or expiring alone", () => {
    expect(serverConfirmsCapture(status({ model_access: { state: "live" } }), "aws", "run-123")).toBe(true);
    expect(serverConfirmsCapture(status({ model_access: { state: "expiring" } }), "aws", "run-123")).toBe(true);
  });

  it("aws: not confirmed when neither the harness row nor model_access agrees", () => {
    expect(
      serverConfirmsCapture(
        status({ harness: [{ provider: "aws", captured: false }], model_access: { state: "expired_signin" } }),
        "aws",
        "run-123",
      ),
    ).toBe(false);
    expect(serverConfirmsCapture(status({}), "aws", "run-123")).toBe(false);
  });

  it("anthropic: confirmed only by its own harness row — model_access never counts for it", () => {
    expect(serverConfirmsCapture(status({ harness: [{ provider: "anthropic", captured: true }] }), "anthropic", "run-123")).toBe(true);
    expect(serverConfirmsCapture(status({ model_access: { state: "live" } }), "anthropic", "run-123")).toBe(false);
  });

  it("a captured row for the OTHER provider does not confirm this one", () => {
    expect(serverConfirmsCapture(status({ harness: [{ provider: "anthropic", captured: true }] }), "aws", "run-123")).toBe(false);
  });
});

// P5: the pane waits for the sandbox instead of assuming it.
//
// POST /setup/harness-login answers with the run id BEFORE dispatch
// (internal/api/harnesscred_launch.go), so "resolved" does not mean
// "attachable": handleAttachTicket 409s a non-RUNNING run and a mint failure is
// terminal in AttachTerminal. The pane holds a `starting` phase — with the run
// id, so Cancel kills a sandbox that is still coming up — and polls the run
// until it is RUNNING (or ends).
describe("HarnessLoginPane — the starting phase", () => {
  // ticket: P5
  beforeEach(() => {
    harnessLoginMock.mockReset().mockResolvedValue("run-123");
    harnessPasteMock.mockReset().mockResolvedValue(undefined);
    lastAttachOutput = undefined;
    lastAttachProps = undefined;
    vi.mocked(runsApiMocked.killRun).mockReset().mockResolvedValue(undefined);
    vi.mocked(runsApiMocked.getRun).mockReset();
    getSetupStatusMock.mockReset();
  });

  async function startAws() {
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: /start login/i }));
  }

  it("keeps the run id so Cancel kills a sandbox that is still coming up", async () => {
    vi.mocked(runsApiMocked.getRun).mockResolvedValue(makeRun({ id: "run-123", state: "PENDING" }));
    await startAws();

    // The waiting copy, not a terminal: attaching to a PENDING run is a 409 the
    // component treats as terminal.
    expect(await screen.findByTestId("login-sandbox-starting")).toBeInTheDocument();
    expect(screen.queryByTestId("fake-terminal")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /cancel/i }));
    expect(runsApiMocked.killRun).toHaveBeenCalledWith("run-123");
  });

  it("polls the run to RUNNING, then mounts the terminal without auto-typing into a self-running sandbox", async () => {
    vi.mocked(runsApiMocked.getRun)
      .mockResolvedValueOnce(makeRun({ id: "run-123", state: "PENDING" }))
      .mockResolvedValue(makeRun({ id: "run-123", state: "RUNNING" }));
    await startAws();

    // The second read is a poll tick away (RUN_POLL_MS), not a microtask.
    expect(await screen.findByTestId("fake-terminal", {}, { timeout: 5000 })).toBeInTheDocument();
    // The image starts the chained command itself (deploy/images/aws-sso/signin-pane.sh)
    // and this terminal joins that session, so AttachTerminal's unconditional
    // 900ms type would land on the running login's stdin and run a SECOND
    // wardyn-aws-sso in the same run — already_captured, i.e. a fail marker on a
    // sign-in that worked. The pane's own grace-window fallback replaces it.
    expect(lastAttachProps?.autoRun).toBeUndefined();
    // …but the flow still CARRIES the command, because the fallback types it and
    // cmd/wardyn-aws-sso's TestLoginCommand_UIParity reads this literal.
    expect(loginFlow("aws").cmd).toContain("&& wardyn-aws-sso");
  });

  // Swallowing `getRun` failures unconditionally ("a blip is not an
  // outcome") is right for ONE and wrong for all of them — a daemon restart
  // mid-pull, a pruned run or a 403 after a roster edit would leave the
  // starting copy on screen forever with nothing but Cancel to end it.
  it("a persistently unreadable run ends the wait, and Cancel still works", async () => {
    vi.mocked(runsApiMocked.getRun).mockRejectedValue(new Error("control plane unreachable"));
    // Drive the poll's own clock rather than waiting out 15 real ticks — 30s of
    // wall time in a unit suite is a cost every CI run pays to learn nothing
    // extra. Installed BEFORE the render: usePoll's setInterval has to be the
    // faked one, and userEvent gets the same clock so its own waits resolve.
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    try {
      render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
      await user.click(screen.getByRole("button", { name: /start login/i }));

      // The first failures are blips: the pane keeps waiting.
      expect(screen.getByTestId("login-sandbox-starting")).toBeInTheDocument();
      expect(screen.queryByRole("alert")).toBeNull();

      // advanceTimersByTimeAsync flushes the microtasks each rejected read
      // queues, which is what usePoll's in-flight guard waits on.
      //
      // Finding 6: fifteen ticks is no longer an ending. It was called "≈30s"
      // and the reporting estate's cold pull took 131 — so thirty seconds of a
      // daemon being unreachable now says "still trying", and the wait ends on
      // the CLOCK (RUN_POLL_UNREADABLE_AFTER_MS, 150 ticks at this cadence).
      for (let i = 0; i < 15; i++) {
        await act(async () => {
          await vi.advanceTimersByTimeAsync(2000);
        });
      }
      expect(screen.queryByRole("alert")).toBeNull();
      expect(screen.getByTestId("login-sandbox-starting")).toHaveTextContent(LOGIN_SANDBOX_READ_RETRYING);

      for (let i = 15; i < 150; i++) {
        await act(async () => {
          await vi.advanceTimersByTimeAsync(2000);
        });
      }
    } finally {
      vi.useRealTimers();
    }

    const alertBox = screen.getByRole("alert");
    expect(alertBox).toHaveTextContent("stopped being able to read the sign-in sandbox");
    expect(screen.queryByTestId("fake-terminal")).not.toBeInTheDocument();

    // The run id survives the ending, so the sandbox is still the operator's to
    // kill — the wait ended because Wardyn could not READ the run, which is no
    // evidence at all that the sandbox stopped.
    await userEvent.click(screen.getByRole("button", { name: /cancel/i }));
    expect(runsApiMocked.killRun).toHaveBeenCalledWith("run-123");
  });

  // Finding 6, the case the old budget could not express: the reads are FINE,
  // the sandbox just isn't up — a 131-second first pull of the aws-sso image on
  // the reporting estate. Nothing here ever fails, so the failure counter this
  // replaced would have sat at zero forever while the copy claimed the start was
  // ordinary.
  it("a healthy STARTING read past 60s shows the slow-start sentence and never an alert", async () => {
    vi.mocked(runsApiMocked.getRun).mockResolvedValue(makeRun({ id: "run-123", state: "PENDING" }));
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    try {
      render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
      await user.click(screen.getByRole("button", { name: /start login/i }));
      expect(screen.getByTestId("login-sandbox-starting")).toHaveTextContent(SIGNIN_PROGRESS.STEP_START);
      expect(screen.getByTestId("login-sandbox-starting")).not.toHaveTextContent(LOGIN_SANDBOX_SLOW_START);

      for (let i = 0; i < 35; i++) {
        await act(async () => {
          await vi.advanceTimersByTimeAsync(2000);
        });
      }
      expect(screen.getByTestId("login-sandbox-starting")).toHaveTextContent(LOGIN_SANDBOX_SLOW_START);
      expect(screen.queryByRole("alert")).toBeNull();

      // Well past the old 30s budget AND past the new 300s one — which does not
      // apply, because nothing is failing.
      for (let i = 35; i < 200; i++) {
        await act(async () => {
          await vi.advanceTimersByTimeAsync(2000);
        });
      }
      expect(screen.queryByRole("alert")).toBeNull();
      expect(screen.getByTestId("login-sandbox-starting")).toHaveTextContent(LOGIN_SANDBOX_SLOW_START);
    } finally {
      vi.useRealTimers();
    }
  });

  // The unreadable clock measures the CURRENT run of failures, not the wait: a
  // sign-in that loses the daemon for four minutes, gets one answer, and loses
  // it again for another four has never been unreadable for five — and ending it
  // on the sum would be the tick counter's mistake with a clock's face on it.
  it("one successful read resets the unreadable clock", async () => {
    let up = false;
    vi.mocked(runsApiMocked.getRun).mockImplementation(async () => {
      if (!up) throw new Error("control plane unreachable");
      up = false; // exactly ONE answer, then dark again
      return makeRun({ id: "run-123", state: "PENDING" });
    });
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    try {
      render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
      await user.click(screen.getByRole("button", { name: /start login/i }));

      const advance = async (ticks: number) => {
        for (let i = 0; i < ticks; i++) {
          await act(async () => {
            await vi.advanceTimersByTimeAsync(2000);
          });
        }
      };

      await advance(120); // 240s dark — retrying, not over
      expect(screen.queryByRole("alert")).toBeNull();
      up = true;
      await advance(1); // one answer
      await advance(120); // another 240s dark: 480s total, 240s consecutive
      expect(screen.queryByRole("alert")).toBeNull();

      await advance(35); // now 310s consecutive, and the wait ends
      expect(screen.getByRole("alert")).toHaveTextContent(LOGIN_SANDBOX_UNREADABLE);
    } finally {
      vi.useRealTimers();
    }
  });

  // The self-run grace window.
  //
  // The aws-sso image starts the pair in its own tmux session before its prep,
  // and every attach path joins that session. The pane therefore types nothing
  // — UNLESS the sandbox never announced itself, which is what an operator
  // WARDYN_AGENT_IMAGES pin on an older image looks like from here.
  //
  // Fake timers before render, always: the pane arms its grace timer the
  // moment it attaches, so timers installed afterwards can never fire it and
  // every "did not type" assertion below would be true for the wrong reason.
  async function attachedOnFakeTimers(): Promise<void> {
    vi.mocked(runsApiMocked.getRun).mockResolvedValue(makeRun({ id: "run-123", state: "RUNNING" }));
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<HarnessLoginPane provider="aws" startURLManaged onDone={vi.fn()} onCancel={vi.fn()} />);
    await user.click(screen.getByRole("button", { name: /start login/i }));
    await screen.findByTestId("fake-terminal");
  }

  // Each case emits ONE suppressing signal and advances well past the window.
  // The suppression, not the arming, is what is under test — "types after the
  // grace window when an old image is pinned" below is the control that proves
  // the timer fires at all on this same shape.
  for (const [name, chunk] of [
    // The image's own banner, through the exported constant the shell side is
    // pinned against (TestSelfRunBanner_UIParity, cmd/wardyn-aws-sso).
    ["the image announces the self-run", `${SELFRUN_MARKER} — AWS sign-in sandbox.\r\n`],
    // A device code is already on screen: the login is live whatever else the
    // buffer does or does not say.
    ["a device verification URL has already been seen", "https://d-1234567890.awsapps.com/start/#/device?user_code=ABCD-EFGH\r\n"],
    // The refusal line is still printing (no trailing newline yet), so the fail
    // arm has not fired and the phase has not changed — the timer's own
    // failMarker check is the only thing standing between this sandbox and a
    // second sign-in typed over its refusal.
    ["the sandbox is part-way through printing a refusal", "wardyn: aws sso credential rejected: the pinned account"],
  ] as const) {
    it(`does not type when ${name}`, async () => {
      sendTextSpy.mockClear();
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        await attachedOnFakeTimers();
        // NEITHER typing path fires: AttachTerminal's own unconditional
        // autoRun would fire 900ms after connect with no check at all.
        expect(lastAttachProps?.autoRun).toBeUndefined();
        act(() => lastAttachOutput?.(chunk));
        await act(async () => {
          await vi.advanceTimersByTimeAsync(30_000);
        });
      } finally {
        vi.useRealTimers();
      }
      expect(sendTextSpy).not.toHaveBeenCalled();
    });
  }

  it("types after the grace window when an old image is pinned", async () => {
    sendTextSpy.mockClear();
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      await attachedOnFakeTimers();
      // An image that predates the self-run says nothing at all.
      expect(sendTextSpy).not.toHaveBeenCalled();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000);
      });
    } finally {
      vi.useRealTimers();
    }
    expect(sendTextSpy).toHaveBeenCalledTimes(1);
    expect(sendTextSpy.mock.calls[0][0]).toBe(loginFlow("aws").cmd + "\r");
  });

  it("a FAILED run shows the run's own failure_hint instead of waiting forever", async () => {
    vi.mocked(runsApiMocked.getRun).mockResolvedValue(makeRun({
      id: "run-123",
      state: "FAILED",
      failure_hint: "the sandbox image could not be pulled",
    }));
    await startAws();

    const alertBox = await screen.findByRole("alert");
    expect(alertBox).toHaveTextContent("the sandbox image could not be pulled");
    expect(screen.queryByTestId("fake-terminal")).not.toBeInTheDocument();
  });
});
