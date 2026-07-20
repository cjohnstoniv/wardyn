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
//
// A fifth, harness-aware demo (needsModel:true) rounds out the set: same CC1 /
// no-grants / no-mounts / no-repos confinement and the same interactive idle
// sandbox, but egress is scoped to Anthropic's API and the operator runs a REAL
// Claude Code agent (`claude`) in the attached terminal — authenticating through
// the connected model, injected proxy-side. Wardyn governs any workload — a
// coding agent is the flagship case, not the only one — so this demo is gated on
// a connected model (demo-screen.tsx) and shown alongside, never instead of, the
// keyless four (which stay entirely LLM-free).
import type { RunPolicySpec } from "../../../lib/types";
import { lsGet, lsSet } from "../../../lib/storage";

// Durable set of demo ids the operator has launched at least once (per browser) —
// powers the per-demo completion checkmark in the Getting-Started funnel. It lives
// in this pure, xterm-free module so setup-screen can read it without pulling the
// terminal-heavy demo-screen graph into the setup chunk.
const LAUNCHED_KEY = "wardyn-demos-launched";
export function loadLaunchedDemos(): string[] {
  try {
    const parsed = JSON.parse(lsGet(LAUNCHED_KEY) ?? "[]");
    return Array.isArray(parsed) ? (parsed as string[]) : [];
  } catch {
    return [];
  }
}
export function markDemoLaunched(demoId: string): void {
  const set = new Set(loadLaunchedDemos());
  if (!set.has(demoId)) {
    set.add(demoId);
    lsSet(LAUNCHED_KEY, JSON.stringify([...set]));
  }
}

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
  /** A fuller "what to expect" — 2-3 sentences, shown in the detailed view. */
  overview: string;
  /** Honest danger note (demo 4 only — CC1 + open egress). */
  caution?: string;
  policy: RunPolicySpec;
  steps: DemoStep[];
  /** How you'd set up a sandbox like this yourself (New Run wizard steps). */
  setupUi: string[];
  /** True only for the harness demo — needs a connected model; demo-screen.tsx
   *  hides the card entirely (and gates Start) until llmReady. Like every demo it
   *  comes up idle for the operator to drive — here they run the agent CLI in the
   *  attached terminal (which is what makes "watch it live" honest). */
  needsModel?: boolean;
}

// policyToYaml renders a demo's inline_policy as a compact YAML block — the same
// shape you'd hand-write as a Wardyn policy file (the canonical on-disk form is
// JSON, e.g. examples/policies/*.json; this is the readable equivalent). Only the
// flat fields the demos actually set are emitted, in a stable, human order.
export function policyToYaml(p: RunPolicySpec): string {
  const fmt = (k: string, v: unknown): string => {
    if (Array.isArray(v)) {
      return v.length === 0 ? `${k}: []` : `${k}:\n` + v.map((x) => `  - ${String(x)}`).join("\n");
    }
    return `${k}: ${String(v)}`;
  };
  const order: (keyof RunPolicySpec)[] = [
    "min_confinement_class",
    "allowed_domains",
    "allow_all_egress",
    "first_use_approval",
    "auto_stop_after_sec",
  ];
  return order
    .filter((k) => p[k] !== undefined)
    .map((k) => fmt(k as string, p[k]))
    .join("\n");
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
    overview:
      "The strictest posture: the sandbox has no allowed destinations and never asks. Any host you didn't pre-approve is refused at the proxy the instant it's dialed — the agent can't reach out, can't leak, and can't stall waiting on a human. Reach for this when a task should touch nothing on the network.",
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
      { text: "Watch the denial land in the Audit panel below the terminal — on the record, in real time." },
    ],
    setupUi: [
      "New Run → pick the Fence (CC1) barrier.",
      "Under Access → Egress, leave the allowed-domains list empty.",
      "Set First-use approval to 'Always deny'.",
      "Launch interactive and attach the terminal.",
    ],
  },
  {
    id: "fail-then-approve",
    title: "Fail, then approve",
    teaches: "deny_with_review: the first hit is denied but raises an approval; approve it and a retry passes.",
    overview:
      "Same default-deny, but a blocked host isn't the end of the story: the first attempt is denied AND raises an approval you can grant. Approve it and the very next try to that host succeeds — the grant sticks for the rest of the run. Good for exploratory work where you want to vet each new destination once, on the fly.",
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
    setupUi: [
      "New Run → pick any barrier (Fence is fine for a demo).",
      "Under Access → Egress, leave the allow-list empty.",
      "Set First-use approval to 'Deny with review'.",
      "Launch interactive; denied requests surface in the Approvals panel below the terminal.",
    ],
  },
  {
    id: "held-at-the-door",
    title: "Held at the door",
    teaches: "wait_for_review: Wardyn HOLDS the connection open while it waits for your live decision.",
    overview:
      "The interactive variant: instead of failing fast, Wardyn holds the connection open while it waits for your live decision, so an approved request completes in the same command — no retry needed. Miss the ~30-second window and it falls back to a denial. Best when a human is watching and you want zero-retry approvals.",
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
    setupUi: [
      "New Run → pick any barrier.",
      "Under Access → Egress, leave the allow-list empty.",
      "Set First-use approval to 'Wait for review'.",
      "Launch interactive and keep the Approvals panel visible — you have ~30s to decide each held request.",
    ],
  },
  {
    id: "lines-that-cant-be-crossed",
    title: "Lines that can't be crossed",
    teaches: "allow_all_egress: the public internet is open, but cloud-metadata and private/LAN ranges are blocked no matter what.",
    overview:
      "The opposite extreme: egress is wide open to the public internet, yet two lines still hold no matter what — the cloud-metadata endpoint (169.254.169.254, where cloud credentials live) and every private/LAN range are refused unconditionally. No policy, and no 'allow all', can open them. Run this to see the guardrails that survive even the loosest egress.",
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
        cmd: "curl -sSI --max-time 5 http://169.254.169.254/latest/meta-data/",
        text: "Denied — you'll see a 403 (or a curl error), and a deny row in the Audit panel. That address is where cloud credentials live; no policy can open it.",
      },
      {
        cmd: "curl -sSI --max-time 5 http://192.168.1.1/",
        text: "Denied the same way. Private/LAN ranges are blocked regardless of policy.",
      },
    ],
    setupUi: [
      "New Run → pick a barrier (Fence here; nothing is mounted, so the blast radius is a bare sandbox).",
      "Under Access → Egress, toggle 'Allow all egress' ON.",
      "The cloud-metadata + private-range blocks are not user-configurable — they always apply.",
      "Launch interactive, reach a public host, then try 169.254.169.254 and a 192.168.x.x address.",
    ],
  },
  {
    id: "agent-in-the-box",
    title: "The agent in the box",
    teaches: "The flagship path: run a real coding agent in the terminal, bound by the exact same policy primitives as the demos above.",
    overview:
      "Every demo above proved the policy holds against a human at a terminal. This one hands you the same terminal to run a real Claude Code agent: it reaches Anthropic's API to think, and nothing else — the same default-deny-plus-allowlist confinement, now doing the job Wardyn actually exists for. It uses the model you connected in setup, injected proxy-side (never resident in the sandbox). A run doesn't have to be an agent at all — Wardyn governs any sandboxed workload — but this is the flagship case, so it gets its own demo.",
    policy: {
      allowed_domains: ["api.anthropic.com", "*.anthropic.com"],
      first_use_approval: "always_deny",
      ...SHARED,
    },
    needsModel: true,
    steps: [
      {
        cmd: "claude -p 'Write HELLO.md summarizing, in a few sentences, what a governed sandbox is and why restricting egress matters'",
        text: "Attach the terminal and run a one-shot agent task — watch Claude Code work live inside the sandbox, authenticating through your connected model (injected proxy-side).",
      },
      {
        cmd: "curl -sSI https://example.com",
        text: "The same policy still holds against the agent's box: any host off the allowlist is refused. The agent can reach api.anthropic.com to think and nothing else.",
      },
      { text: "Open the Audit panel below the terminal — every egress decision (allowed to Anthropic, denied elsewhere) is on the record, attributed to the run." },
    ],
    setupUi: [
      "First connect a model (Getting started → Model/Harness Provider) — this demo only appears once one is connected.",
      "New Run → pick the Fence (CC1) barrier.",
      "Under Access → Egress, allow only api.anthropic.com and *.anthropic.com.",
      "Launch interactive, attach the terminal, and run `claude` yourself.",
    ],
  },
];
