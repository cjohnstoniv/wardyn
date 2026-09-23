/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { SetupStatus } from "../../../lib/types";

// OnboardingScreen's live readiness chips and episode grouping are driven by
// the `status` PROP (the same App-resolved SetupStatus every other screen
// reads — see App.tsx's setupStatus), not a fetch of its own. GettingStarted's
// OTHER branches (SetupScreen, MemberGettingStarted) still call the api
// directly, and onboarding-screen.tsx's module transitively imports both, so
// the client stays mocked for those.
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

  it("is ONE glanceable intro (hero + 5-node strip), not a 7-page tour", () => {
    render(<OnboardingScreen onGetStarted={() => {}} status={status()} />);
    expect(screen.getByText("Sandboxed. Governed. Self-hosted. Free.")).toBeInTheDocument();
    // the single how-it-works strip
    expect(screen.getByText("Behind a barrier")).toBeInTheDocument();
    expect(screen.getByText("Everything recorded")).toBeInTheDocument();
    // no paged tour
    expect(screen.queryByText(/of 7/)).not.toBeInTheDocument();
    expect(screen.getByText(/Barrier:/)).toBeInTheDocument();
  });

  // ui-setup-4: the hero's "about 2 minutes" estimate predates the funnel's
  // growth to 12 steps + a mandatory corp-network probe gate (steps.ts) — no
  // longer honest. The CTA drops the specific number entirely.
  it("the Get started CTA does not claim a stale specific time estimate", () => {
    render(<OnboardingScreen onGetStarted={() => {}} status={status()} />);
    // Word-bounded: the episode catalog below the CTA (EpisodeList) legitimately
    // says "about 72 minutes" — a bare /2 minutes/ would false-positive on it.
    expect(screen.queryByText(/\b2 minutes\b/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Get started/ })).toBeInTheDocument();
  });

  it("surfaces live readiness from the status prop (barrier tier + connected model)", () => {
    render(<OnboardingScreen onGetStarted={() => {}} status={status()} />);
    expect(screen.getByText(/Barrier: Fence ready/)).toBeInTheDocument();
    // llmLabel now names the resolved default integration row itself (see
    // intro.tsx's deriveReadiness) rather than an ad hoc "Claude connected"
    // string — this fixture's host-CLI subscription row is named accordingly.
    expect(screen.getByText(/Model: Claude subscription \(host CLI\)/)).toBeInTheDocument();
  });

  it("the not-ready barrier chip still carries its subject — never a bare 'Needs setup'", () => {
    render(
      <OnboardingScreen
        onGetStarted={() => {}}
        status={status({ runner: { driver: "docker", confinement_classes: [] } })}
      />,
    );
    expect(screen.getByText("Barrier: needs setup")).toBeInTheDocument();
    expect(screen.queryByText("Needs setup")).not.toBeInTheDocument();
  });

  it("no status yet (still resolving) reads as unknown, not a real 'needs setup'", () => {
    render(<OnboardingScreen onGetStarted={() => {}} status={null} />);
    expect(screen.getByRole("button", { name: /Get started/ })).toBeInTheDocument();
    expect(screen.getAllByText(/Checking…/).length).toBeGreaterThan(0);
    expect(screen.queryByText("Barrier: needs setup")).not.toBeInTheDocument();
  });

  it("an unreachable daemon (synthetic READY_FALLBACK) reads as unknown, not a real 'needs setup'", () => {
    render(
      <OnboardingScreen
        onGetStarted={() => {}}
        status={{ ...status(), unreachable: true, runner: { driver: "none", confinement_classes: [] } }}
      />,
    );
    expect(screen.getByRole("button", { name: /Get started/ })).toBeInTheDocument();
    expect(screen.getAllByText(/Checking…/).length).toBeGreaterThan(0);
    expect(screen.queryByText("Barrier: needs setup")).not.toBeInTheDocument();
  });

  // The bug this pins: OnboardingScreen used to run its OWN getSetupStatus()
  // fetch once, with no retry — a single dropped request left the episode
  // catalog below permanently defaulted to the single-user grouping, even for
  // a real multi-user (SSO) install, for the rest of that page load. Now the
  // hero reads whatever status its caller (GettingStarted, fed by App.tsx's
  // own resolved-and-polled SetupStatus) hands it, so a status that starts
  // unresolved and later arrives as SSO must render as multi-user — not get
  // stuck on the single-user guess the null state rendered first.
  it("a status that resolves AFTER first render (e.g. a retried poll) switches the episode grouping — never wedges on the single-user guess", () => {
    const { rerender } = render(<OnboardingScreen onGetStarted={() => {}} status={null} />);
    expect(screen.getByText("Your deployment — single-user")).toBeInTheDocument();
    expect(screen.queryByText("Your deployment — multi-user")).not.toBeInTheDocument();

    rerender(
      <OnboardingScreen
        onGetStarted={() => {}}
        status={status({ auth: { mode: "sso", local_loopback: false } })}
      />,
    );
    expect(screen.getByText("Your deployment — multi-user")).toBeInTheDocument();
    expect(screen.queryByText("Your deployment — single-user")).not.toBeInTheDocument();
  });

  it("is a single forward CTA (onGetStarted) — no skip, no demo side-door", async () => {
    const onGetStarted = vi.fn();
    render(<OnboardingScreen onGetStarted={onGetStarted} status={status()} />);

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

  it("never renders a Composer chip (zero composer UI surfaces on the hero)", () => {
    render(<OnboardingScreen onGetStarted={() => {}} status={status()} />);
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
        <RoleProvider role="user">
          <GettingStarted onDone={() => {}} />
        </RoleProvider>
      </MemoryRouter>,
    );
    expect(await screen.findByText("Getting started")).toBeInTheDocument();
    expect(screen.getByText(/You're a member of this Wardyn/)).toBeInTheDocument();
    expect(screen.queryByText("Sandboxed. Governed. Self-hosted. Free.")).not.toBeInTheDocument();
  });

  // R4/F034: the guard was two-valued (`role === "user"`) after role became
  // three-valued, so a security admin fell THROUGH to the deployer funnel —
  // built from a SetupStatus the server redacts for them
  // (redactSetupStatusForUser zeroes Checks/Providers/Secrets,
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

  it("an admin (or the fail-open default) still sees the welcome hero", () => {
    render(<GettingStarted onDone={() => {}} status={status()} />);
    expect(screen.getByText("Sandboxed. Governed. Self-hosted. Free.")).toBeInTheDocument();
    expect(screen.getByText(/Barrier:/)).toBeInTheDocument();
  });

  // X3-F11: "Getting started" lives in the account menu (app-shell.tsx), not
  // the sidebar — NAV_ITEMS has nine entries, none of them this.
  it('X3-F11: the "revisit anytime" note names the account menu, not the sidebar', () => {
    render(<GettingStarted onDone={() => {}} status={status()} />);
    expect(screen.getByText(/Barrier:/)).toBeInTheDocument();
    expect(screen.getByText(/in the account menu/i)).toBeInTheDocument();
    expect(screen.queryByText(/in the sidebar/i)).not.toBeInTheDocument();
  });
});
