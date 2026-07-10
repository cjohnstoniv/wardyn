/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { SetupStatus } from "../../../lib/types";

// OnboardingScreen now fetches getSetupStatus for its live readiness chips, and
// its module transitively imports the setup funnel (which touches the api). Mock
// the whole client so the import graph is inert and readiness is deterministic.
const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api", async () => {
  const actual = await vi.importActual<typeof import("../../../lib/api")>("../../../lib/api");
  return {
    HttpError: actual.HttpError,
    api: {
      getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a),
      listSecrets: vi.fn().mockResolvedValue([]),
      listComposerBackends: vi.fn().mockResolvedValue([]),
      health: vi.fn().mockResolvedValue({}),
    },
  };
});

import { OnboardingScreen, onboardingSeen, markOnboardingSeen } from "./onboarding-screen";

function status(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return {
    ready: true,
    checks: [],
    auth: { mode: "local", local_loopback: true },
    runner: { driver: "docker", confinement_classes: ["CC1"] },
    composer: { enabled: false, backends: [] },
    providers: [{ tool: "claude", installed: true, logged_in: true }],
    secrets: { present: [], github_app: false },
    age_key: { durable: true },
    has_runs: false,
    platform: { os: "linux", wsl: false },
    ...overrides,
  };
}

describe("OnboardingScreen (welcome hero)", () => {
  const user = userEvent.setup({ pointerEventsCheck: 0 });
  beforeEach(() => {
    localStorage.clear();
    getSetupStatusMock.mockReset().mockResolvedValue(status());
  });

  it("is ONE glanceable intro (hero + 5-node strip), not a 7-page tour", async () => {
    render(<OnboardingScreen onGetStarted={() => {}} onSkip={() => {}} />);
    expect(screen.getByText("Let agents work. Keep your keys.")).toBeInTheDocument();
    // the single how-it-works strip
    expect(screen.getByText("Behind a barrier")).toBeInTheDocument();
    expect(screen.getByText("Everything recorded")).toBeInTheDocument();
    // no paged tour
    expect(screen.queryByText(/of 7/)).not.toBeInTheDocument();
    // settle the async readiness fetch
    await screen.findByText(/Barrier:/);
  });

  it("surfaces live readiness from getSetupStatus (barrier tier + connected model)", async () => {
    render(<OnboardingScreen onGetStarted={() => {}} onSkip={() => {}} />);
    expect(await screen.findByText(/Barrier: Fence ready/)).toBeInTheDocument();
    expect(screen.getByText(/Model: Claude connected/)).toBeInTheDocument();
  });

  it("Get set up advances (onGetStarted); Skip to the console exits (onSkip)", async () => {
    const onGetStarted = vi.fn();
    const onSkip = vi.fn();
    render(<OnboardingScreen onGetStarted={onGetStarted} onSkip={onSkip} />);
    await screen.findByText(/Barrier:/);

    await user.click(screen.getByRole("button", { name: /get set up|finish setup/i }));
    expect(onGetStarted).toHaveBeenCalledTimes(1);

    await user.click(screen.getByRole("button", { name: /skip to the console/i }));
    expect(onSkip).toHaveBeenCalledTimes(1);
  });

  it("onboardingSeen()/markOnboardingSeen() round-trip through localStorage", () => {
    expect(onboardingSeen()).toBe(false);
    markOnboardingSeen();
    expect(onboardingSeen()).toBe(true);
  });

  it("never renders a Composer chip (zero composer UI surfaces on the hero)", async () => {
    render(<OnboardingScreen onGetStarted={() => {}} onSkip={() => {}} />);
    await screen.findByText(/Barrier:/);
    expect(screen.queryByText(/Composer/)).not.toBeInTheDocument();
  });
});
