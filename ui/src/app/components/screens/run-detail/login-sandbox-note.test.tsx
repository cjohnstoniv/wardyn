/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import type { AgentRun } from "../../../lib/types";
import { HARNESS_LOGIN_TASK, LOGIN_SANDBOX_NOTE, LoginSandboxNote } from "./login-sandbox-note";

const run = (task: string) => ({ id: "run-1", task, state: "RUNNING" }) as AgentRun;

describe("LoginSandboxNote", () => {
  it("names the box on a harness login run", () => {
    render(<LoginSandboxNote run={run(HARNESS_LOGIN_TASK)} />);
    expect(screen.getByText(LOGIN_SANDBOX_NOTE)).toBeInTheDocument();
  });

  // The negative half is the whole reason this is keyed on the server's own
  // task discriminator: an ordinary interactive run must be untouched.
  it("renders nothing for any other run", () => {
    const { container } = render(<LoginSandboxNote run={run("review the failing tests")} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("keys on the server-side task literal, not on the agent or the image", () => {
    // harnessLoginTask (internal/api/harnesscred.go). A drift here is a note
    // that silently stops rendering on the only run it exists for.
    expect(HARNESS_LOGIN_TASK).toBe("harness login");
  });
});
