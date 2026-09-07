/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { SetupStatus } from "../../../lib/types";

// OnboardingScreen now fetches getSetupStatus for its live readiness chips, and
// its module transitively imports the setup funnel (which touches the api). Mock
// the whole client so the import graph is inert and readiness is deterministic.
const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

import { GettingStarted, OnboardingScreen, onboardingSeen, markOnboardingSeen } from "./onboarding-screen";
import { baseStatus } from "../../../lib/test-fixtures";
import { RoleProvider } from "../../wardyn/operator-context";

// This suite's own pins: ready, CC1-only runner, a logged-in Claude CLI, and a
// durable secret store.
function status(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return baseStatus({
    ready: true,
    runner: { driver: "docker", confinement_classes: ["CC1"] },
    providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "subscription" }],
    age_key: { durable: true },
    platform: { os: "linux", wsl: false },
    ...overrides,
  });
}

describe("OnboardingScreen (welcome hero)", () => {
  const user = userEvent.setup({ pointerEventsCheck: 0 });
  beforeEach(() => {
    localStorage.clear();
    getSetupStatusMock.mockReset().mockResolvedValue(status());
  });

  it("is ONE glanceable intro (hero + 5-node strip), not a 7-page tour", async () => {
    render(<OnboardingScreen onGetStarted={() => {}} />);
    expect(screen.getByText("Sandboxed. Governed. Self-hosted. Free.")).toBeInTheDocument();
    // the single how-it-works strip
    expect(screen.getByText("Behind a barrier")).toBeInTheDocument();
    expect(screen.getByText("Everything recorded")).toBeInTheDocument();
    // no paged tour
    expect(screen.queryByText(/of 7/)).not.toBeInTheDocument();
    // settle the async readiness fetch
    await screen.findByText(/Barrier:/);
  });

  // ui-setup-4: the hero's "about 2 minutes" estimate predates the funnel's
  // growth to 12 steps + a mandatory corp-network probe gate (steps.ts) — no
  // longer honest. The CTA drops the specific number entirely.
  it("the Get started CTA does not claim a stale specific time estimate", async () => {
    render(<OnboardingScreen onGetStarted={() => {}} />);
    // Word-bounded: the episode catalog below the CTA (EpisodeList) legitimately
    // says "about 72 minutes" — a bare /2 minutes/ would false-positive on it.
    expect(screen.queryByText(/\b2 minutes\b/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Get started/ })).toBeInTheDocument();
    await screen.findByText(/Barrier:/);
  });

  it("surfaces live readiness from getSetupStatus (barrier tier + connected model)", async () => {
    render(<OnboardingScreen onGetStarted={() => {}} />);
    expect(await screen.findByText(/Barrier: Fence ready/)).toBeInTheDocument();
    // llmLabel now names the resolved default integration row itself (see
    // intro.tsx's deriveReadiness) rather than an ad hoc "Claude connected"
    // string — this fixture's host-CLI subscription row is named accordingly.
    expect(screen.getByText(/Model: Claude subscription \(host CLI\)/)).toBeInTheDocument();
  });

  it("the not-ready barrier chip still carries its subject — never a bare 'Needs setup'", async () => {
    getSetupStatusMock.mockResolvedValue(status({ runner: { driver: "docker", confinement_classes: [] } }));
    render(<OnboardingScreen onGetStarted={() => {}} />);
    expect(await screen.findByText("Barrier: needs setup")).toBeInTheDocument();
    expect(screen.queryByText("Needs setup")).not.toBeInTheDocument();
  });

  it("an unreachable daemon (synthetic READY_FALLBACK) reads as unknown, not a real 'needs setup'", async () => {
    getSetupStatusMock.mockResolvedValue({ ...status(), unreachable: true, runner: { driver: "none", confinement_classes: [] } });
    render(<OnboardingScreen onGetStarted={() => {}} />);
    await screen.findByRole("button", { name: /Get started/ });
    expect(await screen.findAllByText(/Checking…/)).not.toHaveLength(0);
    expect(screen.queryByText("Barrier: needs setup")).not.toBeInTheDocument();
  });

  it("is a single forward CTA (onGetStarted) — no skip, no demo side-door", async () => {
    const onGetStarted = vi.fn();
    render(<OnboardingScreen onGetStarted={onGetStarted} />);
    await screen.findByText(/Barrier:/);

    await user.click(screen.getByRole("button", { name: /get started|finish setup/i }));
    expect(onGetStarted).toHaveBeenCalledTimes(1);

    // The escape hatches are gone — the mandatory setup gate keeps the operator here.
    expect(screen.queryByRole("button", { name: /skip for now/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /demo sandbox/i })).not.toBeInTheDocument();
  });

  it("onboardingSeen()/markOnboardingSeen() round-trip through localStorage", () => {
    expect(onboardingSeen()).toBe(false);
    markOnboardingSeen();
    expect(onboardingSeen()).toBe(true);
  });

  it("never renders a Composer chip (zero composer UI surfaces on the hero)", async () => {
    render(<OnboardingScreen onGetStarted={() => {}} />);
    await screen.findByText(/Barrier:/);
    expect(screen.queryByText(/Composer/)).not.toBeInTheDocument();
  });
});

// B4 HIGH-4: a member has no Getting Started nav entry, but a direct /setup
// navigation must still land honestly — never the operator funnel (built from
// a redacted SetupStatus a member can't act on), never a silent bounce. Since
// Phase 5, it lands on the member's OWN Getting Started
// (member-getting-started.tsx) rather than the old one-line notice —
// member-getting-started.test.tsx covers that screen's own sections in full;
// this suite only proves the routing swap.
describe("GettingStarted (member direct navigation — B4 HIGH-4)", () => {
  beforeEach(() => {
    localStorage.clear();
    getSetupStatusMock.mockReset().mockResolvedValue(status());
  });

  it("a member sees their own Getting Started, not the admin welcome hero", async () => {
    render(
      <MemoryRouter>
        <RoleProvider role="member">
          <GettingStarted onDone={() => {}} />
        </RoleProvider>
      </MemoryRouter>,
    );
    expect(await screen.findByText("Getting started")).toBeInTheDocument();
    expect(screen.getByText(/You're a member of this Wardyn/)).toBeInTheDocument();
    expect(screen.queryByText("Sandboxed. Governed. Self-hosted. Free.")).not.toBeInTheDocument();
  });

  // R4/F034: the guard was two-valued (`role === "member"`) after role became
  // three-valued, so a security admin fell THROUGH to the deployer funnel —
  // built from a SetupStatus the server redacts for them
  // (redactSetupStatusForMember zeroes Checks/Providers/Secrets,
  // internal/api/setup.go), driving mutations that are super-admin-only.
  // setupGateActive already reads `!== "admin"` for exactly this reason.
  it("a security admin sees the member Getting Started, not the deployer funnel", async () => {
    // The literal redacted payload the server hands a non-operator.
    getSetupStatusMock.mockResolvedValue(status({ checks: [], providers: [], secrets: { present: [], github_app: false } }));
    render(
      <MemoryRouter>
        <RoleProvider role="security_admin">
          <GettingStarted onDone={() => {}} />
        </RoleProvider>
      </MemoryRouter>,
    );
    expect(await screen.findByText("Getting started")).toBeInTheDocument();
    expect(screen.queryByText("Sandboxed. Governed. Self-hosted. Free.")).not.toBeInTheDocument();
  });

  it("an admin (or the fail-open default) still sees the welcome hero", async () => {
    render(<GettingStarted onDone={() => {}} />);
    expect(screen.getByText("Sandboxed. Governed. Self-hosted. Free.")).toBeInTheDocument();
    await screen.findByText(/Barrier:/);
  });
});
