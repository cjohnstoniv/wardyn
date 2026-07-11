/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Smoke test for the auth-mode residency framing (best-practice-safest-path
// campaign): the proxy-injected vs resident-at-run-time indicators added to
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
