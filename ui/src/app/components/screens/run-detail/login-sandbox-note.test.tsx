/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeAll, beforeEach, describe, it, expect, vi } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { makeRun } from "../../../../test/factories";
import { aheadByHours } from "../../../lib/test-clock";
import { OperatorProvider } from "../../wardyn/operator-context";
import { RUN_SIGN_IN } from "../../wardyn/copy";
import {
  AWS_SSO_LOGIN_AGENT,
  HARNESS_LOGIN_TASK,
  LOGIN_SANDBOX_NOTE,
  LoginSandboxNote,
} from "./login-sandbox-note";

const getMock = vi.fn();
vi.mock("../../../lib/api/run-sign-in", () => ({ runSignIn: { get: (...a: unknown[]) => getMock(...a) } }));

const run = (task: string, agent = AWS_SSO_LOGIN_AGENT) => makeRun({ id: "run-1", task, agent, state: "RUNNING" });

describe("LoginSandboxNote", () => {
  it("names the box on an AWS harness login run", () => {
    render(<LoginSandboxNote run={run(HARNESS_LOGIN_TASK)} />);
    expect(screen.getByText(LOGIN_SANDBOX_NOTE)).toBeInTheDocument();
  });

  // The negative half is the whole reason this is keyed on the server's own
  // task discriminator: an ordinary interactive run must be untouched.
  it("renders nothing for any other run", () => {
    const { container } = render(<LoginSandboxNote run={run("review the failing tests")} />);
    expect(container).toBeEmptyDOMElement();
  });

  // `harness login` is PROVIDER-AGNOSTIC: the Anthropic lane is the login
  // route's own default and runs `claude setup-token` in the claude-code image.
  // An AWS sentence on that run is false on every clause it makes.
  it("renders nothing for the ANTHROPIC container login, which carries the same task", () => {
    const { container } = render(<LoginSandboxNote run={run(HARNESS_LOGIN_TASK, "claude-code")} />);
    expect(container).toBeEmptyDOMElement();
  });

  // U-2 (W6 blind lens): the note is about a box that is UP. On a KILLED or
  // COMPLETED login run every clause of it — the terminal, the device code, the
  // idle cap — describes a sandbox that is gone, and the Runs list is exactly
  // where a finished login run is reopened.
  it.each(["KILLED", "COMPLETED", "FAILED", "PENDING"])(
    "renders nothing for a %s harness-login run — the box is not up",
    (state) => {
      const { container } = render(
        <LoginSandboxNote run={{ ...run(HARNESS_LOGIN_TASK), state }} />,
      );
      expect(container).toBeEmptyDOMElement();
    },
  );

  it("keys on the server-side task AND agent literals", () => {
    // harnessLoginTask / awsSSOAgent (internal/api/harnesscred.go). Held to the
    // Go side by TestHarnessLoginTask_UIParity; these two assertions are the
    // local half, so a drift reds here as well as there.
    expect(HARNESS_LOGIN_TASK).toBe("harness login");
    expect(AWS_SSO_LOGIN_AGENT).toBe("aws-sso");
  });
});

// The server's own answers for the shared pane cases, read by path the way the
// extractor's test reads them (internal/api/testdata/sign_in_pane_fixtures.json).
const root = (() => {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8 && !existsSync(join(dir, "go.mod")); i++) dir = dirname(dir);
  return dir;
})();
const { cases } = JSON.parse(readFileSync(join(root, "internal/api/testdata/sign_in_pane_fixtures.json"), "utf8")) as {
  cases: { name: string; go: { state: string; verification_url?: string; user_code?: string } }[];
};
const answerOf = (name: string) => {
  const c = cases.find((x) => x.name.startsWith(name));
  if (!c) throw new Error(`no fixture case ${name}`);
  return c.go;
};

describe("LoginSandboxNote — the waiting sign-in strip", () => {
  const mine = (o: Partial<ReturnType<typeof makeRun>> = {}) =>
    makeRun({ id: "run-1", task: HARNESS_LOGIN_TASK, agent: AWS_SSO_LOGIN_AGENT, state: "RUNNING", created_by: "me", ...o });
  const view = (r: ReturnType<typeof makeRun>, principal = "me", operator = false) =>
    render(
      <OperatorProvider operator={operator} principal={principal}>
        <LoginSandboxNote run={r} />
      </OperatorProvider>,
    );
  const tick = (ms: number) => act(async () => void (await vi.advanceTimersByTimeAsync(ms)));

  // The strip is a lazy chunk: resolve it up front so a tick is enough to draw it.
  beforeAll(async () => {
    await import("./run-sign-in-strip");
  });
  beforeEach(() => {
    vi.useFakeTimers();
    getMock.mockReset();
  });
  afterEach(() => vi.useRealTimers());

  it("shows the latest attempt's code and link above today's note", async () => {
    getMock.mockResolvedValue(answerOf("two consecutive attempts"));
    view(mine());
    await tick(0);
    const strip = screen.getByRole("status");
    expect(strip).toHaveTextContent(RUN_SIGN_IN.TITLE);
    expect(strip).toHaveTextContent(RUN_SIGN_IN.BODY);
    expect(screen.getByLabelText(RUN_SIGN_IN.CODE_LABEL)).toHaveTextContent("WXYZ-1234");
    const open = screen.getByRole("link", { name: RUN_SIGN_IN.OPEN });
    expect(open).toHaveAttribute("href", "https://device.sso.us-east-1.amazonaws.com/?user_code=WXYZ-1234");
    expect(open).toHaveAttribute("target", "_blank");
    expect(open).toHaveAttribute("rel", "noopener noreferrer");
    expect(screen.getByRole("button", { name: RUN_SIGN_IN.COPY })).toBeInTheDocument();
    expect(strip).toHaveTextContent("Opens device.sso.us-east-1.amazonaws.com");
    // the note is unchanged, below the strip
    const note = screen.getByTestId("login-sandbox-note");
    expect(note).toHaveTextContent(LOGIN_SANDBOX_NOTE);
    expect(strip.compareDocumentPosition(note) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("a first not_waiting answer adds nothing, and an old run is not read again", async () => {
    getMock.mockResolvedValue(answerOf("a completed attempt"));
    view(mine({ created_at: aheadByHours(-1) }));
    await tick(0);
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(screen.queryByText(RUN_SIGN_IN.NO_LONGER)).not.toBeInTheDocument();
    await tick(20_000);
    expect(getMock).toHaveBeenCalledTimes(1);
  });

  it("D7: a run in its first two minutes keeps reading until the code is drawn", async () => {
    getMock.mockResolvedValueOnce(answerOf("no attempt yet")).mockResolvedValue(answerOf("a waiting attempt"));
    view(mine({ created_at: new Date(Date.now() - 10_000).toISOString() }));
    await tick(0);
    expect(screen.queryByLabelText(RUN_SIGN_IN.CODE_LABEL)).not.toBeInTheDocument();
    await tick(5000);
    expect(screen.getByLabelText(RUN_SIGN_IN.CODE_LABEL)).toHaveTextContent("ABCD-EFGH");
    expect(getMock).toHaveBeenCalledTimes(2);
  });

  it("not_waiting after waiting replaces the strip with the closing line and stops reading", async () => {
    getMock.mockResolvedValueOnce(answerOf("a waiting attempt")).mockResolvedValue(answerOf("a completed attempt"));
    view(mine());
    await tick(0);
    expect(screen.getByLabelText(RUN_SIGN_IN.CODE_LABEL)).toBeInTheDocument();
    await tick(5000);
    expect(screen.queryByLabelText(RUN_SIGN_IN.CODE_LABEL)).not.toBeInTheDocument();
    expect(screen.getByText(RUN_SIGN_IN.NO_LONGER)).toBeInTheDocument();
    await tick(30_000);
    expect(getMock).toHaveBeenCalledTimes(2);
  });

  it("a failed read keeps the strip, says so, stops, and Retry reads again", async () => {
    getMock.mockResolvedValueOnce(answerOf("a waiting attempt")).mockRejectedValueOnce(new Error("503"));
    view(mine());
    await tick(0);
    await tick(5000);
    expect(screen.getByLabelText(RUN_SIGN_IN.CODE_LABEL)).toHaveTextContent("ABCD-EFGH");
    expect(screen.getByText(RUN_SIGN_IN.READ_FAILED)).toBeInTheDocument();
    await tick(30_000);
    expect(getMock).toHaveBeenCalledTimes(2);
    getMock.mockResolvedValue(answerOf("a waiting attempt"));
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await tick(0);
    expect(getMock).toHaveBeenCalledTimes(3);
    expect(screen.queryByText(RUN_SIGN_IN.READ_FAILED)).not.toBeInTheDocument();
  });

  it("shows the checking line only after a second of the first read", async () => {
    getMock.mockReturnValue(new Promise(() => {}));
    view(mine());
    await tick(900);
    expect(screen.queryByText(RUN_SIGN_IN.CHECKING)).not.toBeInTheDocument();
    await tick(200);
    expect(screen.getByText(RUN_SIGN_IN.CHECKING)).toBeInTheDocument();
  });

  it.each([
    ["an admin on someone else's run", mine({ created_by: "someone-else" }), "me", true],
    ["an unresolved identity", mine(), "", true],
    ["a run that is not running", mine({ state: "KILLED" }), "me", false],
  ])("%s: no read, no strip", async (_n, r, principal, operator) => {
    view(r, principal, operator);
    await tick(10_000);
    expect(getMock).not.toHaveBeenCalled();
    expect(screen.queryByTestId("run-sign-in-strip")).not.toBeInTheDocument();
  });
});
