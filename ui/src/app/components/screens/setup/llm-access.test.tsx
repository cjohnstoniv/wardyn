/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Smoke test for the auth-mode residency framing (best-practice safest-path
// guidance): the proxy-injected vs resident-at-run-time indicators added to
// ModelStep's rows. Mirrors step-bodies.test.tsx's ModelStep render shape.
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { ModelStep } from "./llm-access";
import { deriveReadiness } from "../onboarding/intro";
import { baseStatus } from "./test-fixtures";

describe("ModelStep — auth-mode residency framing", () => {
  it("shows the resident-at-run-time chip and the static-vs-SSO sentence on the Bedrock option", () => {
    const status = baseStatus({ bedrock: { creds_present: true, ready: false } });
    render(
      <ModelStep
        status={status}
        readiness={deriveReadiness(status)}
        onAddSecret={vi.fn()}
        onSetup={vi.fn()}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    expect(screen.getByText("resident (static/SSO)")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Static AWS keys become resident in sandboxes that use Bedrock; SSO via ~/.aws mount auto-rotates and is safer.",
      ),
    ).toBeInTheDocument();
  });

  it("shows the proxy-injected chip on a detected Claude subscription", () => {
    const status = baseStatus({
      providers: [{ tool: "claude", installed: true, logged_in: true }],
    });
    render(
      <ModelStep
        status={status}
        readiness={deriveReadiness(status)}
        onAddSecret={vi.fn()}
        onSetup={vi.fn()}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    expect(screen.getByText("proxy-injected (default)")).toBeInTheDocument();
  });
});

// The resident-CLI option ("Install / Log in to Claude CLI") reaches the SAME Claude
// subscription the container login captures. Offering it beside a CONNECTED managed
// credential reads as an unfinished step; offering it on a sealed control plane dangles
// a path that can never succeed (wardynd is blind to the host's ~/.claude by design).
describe("ModelStep — resident-CLI option is not dangled", () => {
  const sealed = { host_like: false as const };

  it("hides the Claude CLI option once a Wardyn-managed subscription is connected", () => {
    const status = baseStatus({
      deployment: sealed,
      harness: [{ provider: "anthropic", captured: true }],
    });
    render(
      <ModelStep
        status={status}
        readiness={deriveReadiness(status)}
        onAddSecret={vi.fn()}
        onSetup={vi.fn()}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    expect(screen.queryByText("Install Claude CLI")).not.toBeInTheDocument();
    expect(screen.queryByText("Log in to Claude CLI")).not.toBeInTheDocument();
  });

  it("hides the Claude CLI option on a sealed control plane even with nothing connected", () => {
    const status = baseStatus({ deployment: sealed });
    render(
      <ModelStep
        status={status}
        readiness={deriveReadiness(status)}
        onAddSecret={vi.fn()}
        onSetup={vi.fn()}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    expect(screen.queryByText("Install Claude CLI")).not.toBeInTheDocument();
    expect(screen.queryByText("Log in to Claude CLI")).not.toBeInTheDocument();
    // The path that CAN work here is still offered.
    expect(screen.getByText("Connect via container login (no local install)")).toBeInTheDocument();
  });

  it("hides the Codex CLI option on a sealed control plane (no container login exists for it)", () => {
    const status = baseStatus({ deployment: sealed });
    render(
      <ModelStep
        status={status}
        readiness={deriveReadiness(status)}
        onAddSecret={vi.fn()}
        onSetup={vi.fn()}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    expect(screen.queryByText("Install Codex CLI")).not.toBeInTheDocument();
    expect(screen.queryByText("Log in to Codex CLI")).not.toBeInTheDocument();
    // The only reachable Codex route on a sealed plane.
    expect(screen.getByText("Add OpenAI API key")).toBeInTheDocument();
  });

  it("still offers the Claude CLI option in host mode, where a resident login is detectable", () => {
    const status = baseStatus({ deployment: { host_like: true } });
    render(
      <ModelStep
        status={status}
        readiness={deriveReadiness(status)}
        onAddSecret={vi.fn()}
        onSetup={vi.fn()}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    expect(screen.getByText("Log in to Claude CLI")).toBeInTheDocument();
  });
});
