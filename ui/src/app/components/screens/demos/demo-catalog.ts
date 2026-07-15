/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Demo sandbox catalog — four hands-on, workspace-free, LLM-free demos that a
// new user can run BEFORE onboarding any repo or key, to prove Wardyn's egress
// confinement first-hand. Each launches an interactive CC1 sandbox via the
// existing POST /api/v1/runs (interactive + inline_policy); the operator drives
// plain curl in the attached terminal and watches the policy hold. All four are
// CC1 / auto-stop 900s / no grants / no mounts / no repos by construction.
import type { RunPolicySpec } from "../../../lib/types";

// One numbered instruction in a demo. `cmd` (when present) renders as a copy
// pill the operator pastes into the attached terminal; `text` is the always-shown
// explanation of what they'll see.
export interface DemoStep {
  cmd?: string;
  text: string;
}

export interface Demo {
  id: string;
  title: string;
  /** One-line "what this proves" shown under the title. */
  teaches: string;
  /** Honest danger note (demo 4 only — CC1 + open egress). */
  caution?: string;
  policy: RunPolicySpec;
  steps: DemoStep[];
}

// Shared across every demo: weakest barrier (runs anywhere), reaped 15 min after
// the operator walks away, and deliberately nothing else — no grants, no mounts,
// no repos. Spread into each policy so the invariants live in one place.
const SHARED = {
  min_confinement_class: "CC1" as const,
  auto_stop_after_sec: 900,
};

export const DEMOS: Demo[] = [
  {
    id: "sealed-box",
    title: "The sealed box",
    teaches: "Default-deny egress: an unlisted host is refused outright — no prompt, no wait.",
    policy: {
      allowed_domains: [],
      first_use_approval: "always_deny",
      ...SHARED,
    },
    steps: [
      {
        cmd: "curl -sSI https://example.com",
        text: "Fails immediately: the proxy refuses the tunnel. No prompt, no wait — this policy never asks.",
      },
      { text: "Open Audit — the denial is on the record." },
    ],
  },
  {
    id: "fail-then-approve",
    title: "Fail, then approve",
    teaches: "deny_with_review: the first hit is denied but raises an approval; approve it and a retry passes.",
    policy: {
      allowed_domains: [],
      first_use_approval: "deny_with_review",
      ...SHARED,
    },
    steps: [
      {
        cmd: "curl -sSI https://example.com",
        text: "Fails, and an approval request appears below the terminal.",
      },
      { text: "Click Approve." },
      {
        text: "Run the same command again — HTTP/2 200. Approved hosts stay allowed for the rest of this run.",
      },
    ],
  },
  {
    id: "held-at-the-door",
    title: "Held at the door",
    teaches: "wait_for_review: Wardyn HOLDS the connection open while it waits for your live decision.",
    policy: {
      allowed_domains: [],
      first_use_approval: "wait_for_review",
      ...SHARED,
    },
    steps: [
      {
        cmd: "curl -sSI --max-time 60 https://example.com",
        text: "The command HANGS: Wardyn is holding the connection open, waiting for you.",
      },
      {
        text: "Within ~30 seconds, click Approve below — the same hanging command completes. (Miss the window and it falls back to a 403 — approve and re-run.)",
      },
      {
        cmd: "curl -sSI --max-time 60 https://wikipedia.org",
        text: "click Deny — instant refusal.",
      },
    ],
  },
  {
    id: "lines-that-cant-be-crossed",
    title: "Lines that can't be crossed",
    teaches: "allow_all_egress: the public internet is open, but cloud-metadata and private/LAN ranges are blocked no matter what.",
    caution:
      "Fence (CC1) shares your machine's kernel and this box allows the public internet — the widest window Wardyn opens. It's safe here only because nothing is mounted: no repo, no key, no workspace. The point of this demo is the two denials that still hold with egress wide open.",
    policy: {
      allowed_domains: [],
      allow_all_egress: true,
      first_use_approval: "always_deny",
      ...SHARED,
    },
    steps: [
      {
        cmd: "curl -sSI https://example.com",
        text: "Works: this sandbox allows the public internet.",
      },
      {
        cmd: "curl -s --max-time 5 http://169.254.169.254/latest/meta-data/",
        text: "Denied. That address is where cloud credentials live; no policy can open it.",
      },
      {
        cmd: "curl -s --max-time 5 http://192.168.1.1/",
        text: "Denied. Private/LAN ranges are blocked regardless of policy.",
      },
    ],
  },
];
