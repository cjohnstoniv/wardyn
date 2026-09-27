/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { makeRun } from "../../../../test/factories";
import {
  AWS_SSO_LOGIN_AGENT,
  HARNESS_LOGIN_TASK,
  LOGIN_SANDBOX_NOTE,
  LoginSandboxNote,
} from "./login-sandbox-note";

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
