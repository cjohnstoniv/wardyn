/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Shared SetupStatus test fixture (V1) — six suites (setup-screen, step-bodies,
// steps, environment-step, ../onboarding/intro, ../onboarding/onboarding-screen)
// used to hand-roll near-identical baseStatus()/status() fixtures. One shared
// shape here; each suite composes its own value pins as overrides.
import type { SetupStatus } from "../../../lib/types";

export function baseStatus(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return {
    ready: false,
    checks: [],
    auth: { mode: "local", local_loopback: true },
    runner: {
      driver: "docker",
      confinement_classes: ["CC1", "CC2"],
      confinement_substrates: { CC1: "oci/runc", CC2: "oci/runsc" },
    },
    composer: { enabled: false, backends: [] },
    providers: [{ tool: "claude", installed: true, logged_in: false }],
    secrets: { present: [], github_app: false },
    age_key: { durable: false },
    has_runs: false,
    platform: { os: "linux", wsl: false, kvm: true },
    ...overrides,
  };
}
