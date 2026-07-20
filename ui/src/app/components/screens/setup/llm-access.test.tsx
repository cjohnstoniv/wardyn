/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Smoke test for the auth-mode residency framing (best-practice safest-path
// guidance): the proxy-injected vs resident-at-run-time indicators added to
// ModelStep's rows. Mirrors step-bodies.test.tsx's ModelStep render shape.
import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { ModelStep } from "./llm-access";
import { deriveReadiness } from "../onboarding/intro";
import { baseStatus } from "./test-fixtures";

describe("ModelStep — auth-mode residency framing", () => {
  it("shows Bedrock's four modes with correct residency chips (bearer green, SSO/keys resident)", () => {
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
    // Bedrock renders under the default Claude harness. Bearer is proxy-injected +
    // recommended; the SSO and static-key modes are each "resident".
    expect(screen.getByText("Bearer token")).toBeInTheDocument();
    expect(screen.getByText("AWS SSO (containerized login)")).toBeInTheDocument();
    expect(screen.getByText("Host ~/.aws profile")).toBeInTheDocument();
    expect(screen.getByText("Access keys")).toBeInTheDocument();
    expect(screen.getByText("Recommended")).toBeInTheDocument();
    // Two resident chips (SSO + static keys); the bearer row is proxy-injected.
    expect(screen.getAllByText("resident").length).toBeGreaterThanOrEqual(2);
    expect(
      screen.getByText(/signed in-process, so they live in the sandbox/i),
    ).toBeInTheDocument();
  });

  it("keeps AWS Bedrock reachable even when every other Claude path is already satisfied", () => {
    // Host mode with a logged-in Claude CLI AND an Anthropic key: the resident-CLI
    // and add-key options are both gone, but Bedrock (unconfigured) must still be
    // offered — it's an independent connect path, not an "add another Claude key".
    const status = baseStatus({
      deployment: { host_like: true },
      providers: [{ tool: "claude", installed: true, logged_in: true }],
      secrets: { present: ["anthropic-api-key"], github_app: false },
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
    expect(screen.getByRole("button", { name: /set up aws bedrock/i })).toBeInTheDocument();
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
    expect(screen.getByRole("button", { name: /set up claude subscription/i })).toBeInTheDocument();
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
    // Harness-first: pick Codex to reveal its connection methods.
    fireEvent.click(screen.getByText("Codex"));
    expect(screen.queryByText("Install Codex CLI")).not.toBeInTheDocument();
    expect(screen.queryByText("Log in to Codex CLI")).not.toBeInTheDocument();
    // The only reachable Codex route on a sealed plane.
    expect(screen.getByText("Set up OpenAI API key")).toBeInTheDocument();
    // And the honesty line explaining why there's no container login.
    expect(screen.getByText(/there's no container login/i)).toBeInTheDocument();
  });

  it("still offers the host Claude CLI route in host mode, where a resident login is detectable", () => {
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
    // The primary action is the credential-named "Set up Claude subscription";
    // the host's own CLI login is offered as a secondary alternative beneath it.
    expect(screen.getByRole("button", { name: /set up claude subscription/i })).toBeInTheDocument();
    expect(screen.getByText(/already logged in with the claude cli on this host/i)).toBeInTheDocument();
    // The fixture has the CLI installed but not logged in, so the secondary route
    // offers that existing install rather than an install guide.
    expect(screen.getByRole("button", { name: /use that login instead/i })).toBeInTheDocument();
  });
});

describe("ModelStep — Bedrock reveals its mode chooser, never a single presumed mode", () => {
  it("expands the Bedrock mode chooser inline instead of opening one mode's secret dialog", () => {
    const onAddSecret = vi.fn();
    const status = baseStatus(); // no bedrock configured
    render(
      <ModelStep
        status={status}
        readiness={deriveReadiness(status)}
        onAddSecret={onAddSecret}
        onSetup={vi.fn()}
        onRecheck={vi.fn()}
        rechecking={false}
      />,
    );
    // Collapsed: the modes aren't shown yet.
    expect(screen.queryByText("Bearer token")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /set up aws bedrock/i }));

    // Expanding must NOT jump straight into a secret dialog for one mode…
    expect(onAddSecret).not.toHaveBeenCalled();
    // …it reveals all three so the operator picks (incl. the host ~/.aws SSO path).
    expect(screen.getByText("Bearer token")).toBeInTheDocument();
    expect(screen.getByText("AWS SSO (containerized login)")).toBeInTheDocument();
    expect(screen.getByText("Host ~/.aws profile")).toBeInTheDocument();
    expect(screen.getByText("Access keys")).toBeInTheDocument();
  });
});
