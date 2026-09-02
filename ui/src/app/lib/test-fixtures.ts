/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Shared test fixtures for the two bodies nearly every screen suite has to
// stand up: GET /setup/status and GET /me. baseStatus lived under
// screens/setup/ until that funnel was deleted; 18 suites depend on it, most of
// them nothing to do with setup, so it belongs in lib/ — and the /me pair
// followed it here for the same reason, once seven suites had retyped the same
// member body and four had retyped the same drive.
import type { Me, MeUserDrive } from "./api/health";
import type { SetupStatus } from "./types";

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
    providers: [{ tool: "claude", installed: true, logged_in: false }],
    secrets: { present: [], github_app: false },
    age_key: { durable: false },
    has_runs: false,
    platform: { os: "linux", wsl: false, kvm: true },
    ...overrides,
  };
}

// GET /me for a plain MEMBER — the tier every drive/onboarding suite is
// written from, with NEITHER 0.7 drive bit set: no allocation and no door,
// which is what a deployment that has never registered a drive answers and
// what every case that isn't about drives must render as.
export function baseMe(overrides: Partial<Me> = {}): Me {
  return {
    principal: "alice@corp.example",
    method: "sso",
    operator: false,
    security_operator: false,
    role: "member",
    email: "alice@corp.example",
    user_drive: null,
    user_drive_denied_by_profile: "",
    ...overrides,
  };
}

// /me's `user_drive` — a managed claim with a size that IS shown and a
// writable mount, the case both member surfaces render most of. The share
// (no size, read-only, `external`) and the paused arms are overrides of it.
export function baseMeDrive(overrides: Partial<MeUserDrive> = {}): MeUserDrive {
  return {
    name: "Scratch",
    backend: "k8s_pvc",
    size_mib: 16384,
    writable: true,
    enforcement: "request",
    ...overrides,
  };
}
