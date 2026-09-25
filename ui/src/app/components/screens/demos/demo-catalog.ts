/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Demo sandbox catalog — hands-on, workspace-free demos that a new user can run
// BEFORE onboarding any repo or key, to prove Wardyn's confinement first-hand.
// Each launches an interactive CC1 sandbox via the existing POST /api/v1/runs
// (interactive + inline_policy); the operator drives plain curl in the attached
// terminal and watches the policy hold. Every demo is CC1 / auto-stop 900s / no
// mounts / no repos by construction. Two sub-sections (`Demo.section`):
//
// - egress (seven): the original network-egress governance demos — the four
//   keyless first_use_approval/allow-all showcase demos, the agent-in-the-box
//   harness demo (needsModel:true — same CC1/no-grants confinement, but egress
//   is scoped to Anthropic's API and the operator runs a REAL Claude Code agent
//   in the terminal, authenticating through the connected model injected
//   proxy-side; gated on a connected model and shown alongside, never instead
//   of, the keyless demos), record-a-policy, and once-or-for-good. All keyless
//   except the harness one.
// - secrets (eight, all keyless): governance for a stored VALUE rather than a
//   destination. Three teach the api_key/broker mechanism itself —
//   write-only-by-design (no route ever reads a secret back),
//   key-never-in-the-box (an api_key grant injects proxy-side, never resident),
//   and authorized-not-issued (an approval-gated, single-use mint). Five more
//   split it by credential KIND, because how a credential is protected depends
//   entirely on what the consuming protocol can accept:
//     - rest-api-token  api_key, the realistic SaaS shape (Authorization: Bearer)
//     - pat-stdout-only git_pat — git-over-HTTPS is an opaque CONNECT tunnel with
//                       no header to inject, so the PAT is minted into a PIPE
//     - ssh-briefly-resident  ssh_key — the documented resident exception (no
//                       credential-helper seam exists in ssh at all)
//     - github-app-broker     github_token — TEACH+GATE (`needsGitHubApp`)
//     - sts-fail-closed       cloud_sts — the CREATE refusal IS the demo
//   Every demo referencing a stored secret carries `needsSecret` (api_key,
//   git_pat and ssh_key refs are all checked at run-create, which 422s on a
//   missing one).
//
// TEACH NOTE — the two things called "an OAuth token" are not the same lane, and
// neither gets a duplicate injection demo here:
//   - an OAuth ACCESS token (a managed Claude subscription) is injected exactly
//     like an api_key: resolved proxy-side at request time, with an inert
//     sentinel left in the sandbox. That mechanism is already on camera in the
//     agent-in-the-box demo and in key-never-in-the-box's audit trail — the
//     credential kind differs, the protection does not.
//   - an OAuth-APP INSTALLATION token (github_token) is a step further out: it
//     is minted from the live GitHub API and attached on the proxy's own
//     OUTBOUND leg after re-origination, so the sandbox never holds it AND
//     cannot even ask for it. That is github-app-broker, and it is why that
//     card teaches instead of running.
import type { RunPolicySpec } from "../../../lib/types";
import { lsGet, lsSet } from "../../../lib/storage";
import { SHARED, SECRETS_DEMOS } from "./demo-catalog-secrets";

// Durable set of demo ids the operator has launched at least once (per browser) —
// powers the per-demo completion checkmark in the Getting-Started funnel. It lives
// in this pure, xterm-free module so setup-screen can read it without pulling the
// terminal-heavy demo-runner graph into the setup chunk.
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

// Every demo id, in catalog order, as a CONST-ASSERTED tuple. This is what
// keeps setup/steps.ts's `SetupStepId` a literal union: Getting Started lists
// the whole catalog as sub-steps, and `DEMOS.map((d) => d.id)` alone would
// widen to `string[]` and take the union with it. `Demo.id: DemoId` below ties
// the two together, so an entry can never carry an id this tuple doesn't name;
// setup/steps.test.ts pins the reverse (same ids, same order as DEMOS).
export const DEMO_IDS = [
  "sealed-box",
  "fail-then-approve",
  "held-at-the-door",
  "lines-that-cant-be-crossed",
  "denied-however-spelled",
  "agent-in-the-box",
  "record-a-policy",
  "once-or-for-good",
  "write-only-by-design",
  "key-never-in-the-box",
  "authorized-not-issued",
  "rest-api-token",
  "pat-stdout-only",
  "ssh-briefly-resident",
  "github-app-broker",
  "sts-fail-closed",
] as const;
export type DemoId = (typeof DEMO_IDS)[number];

export interface Demo {
  id: DemoId;
  title: string;
  /** One-line "what this proves" shown under the title. */
  teaches: string;
  /** A fuller "what to expect" — 2-3 sentences, shown in the detailed view. */
  overview: string;
  /** Honest danger note (allow-all-egress demos only — CC1 + open egress). */
  caution?: string;
  policy: RunPolicySpec;
  /** Which Getting-Started sub-section this demo belongs to: network egress
   *  governance or secrets governance (keeping a value
   *  out of the sandbox, whether or not it ever touches egress). */
  section: "egress" | "secrets";
  steps: DemoStep[];
  /** How you'd set up a sandbox like this yourself, on the New run page. */
  setupUi: string[];
  /** True only for the harness demo — needs a connected model. Its funnel
   *  sub-step is filtered out of the walk until llmReady (setup/steps.ts's
   *  stepOrder), which IS the gate. Like every demo it comes up idle for the
   *  operator to drive — here they run the agent CLI in the attached terminal
   *  (which is what makes "watch it live" honest). */
  needsModel?: boolean;
  /** Set only on a demo whose policy carries a grant referencing this secret
   *  NAME. validateInlineSecretRefs collects api_key, git_pat AND ssh_key
   *  secret refs (regardless of requires_approval), so a missing secret 422s at
   *  run-create for every kind that names one. Gated the same shape as
   *  needsModel: stepOrder drops the sub-step until the secret is in
   *  `SetupStatus.secrets.present`. */
  needsSecret?: string;
  /** Set only on a TEACH+GATE demo whose credential cannot be faked locally —
   *  today just github-app-broker, whose installation token is minted from the
   *  LIVE GitHub API. Unlike needsSecret/needsModel this does NOT drop the
   *  sub-step from the walk (setup/steps.ts's stepOrder): a deleted card can
   *  neither teach nor be filmed, and this card's whole job is teaching. It
   *  stays in the rail with a DISABLED Start plus gate copy — the same shape
   *  `!barrierReady` already uses. Gated on SetupStatus.secrets.github_app,
   *  which is true iff BOTH App secrets are stored; member redaction zeroes
   *  Secrets, so the gate reads closed for a member either way and its copy
   *  says so instead of sending them to a page they cannot write. */
  needsGitHubApp?: boolean;
  /** The demo's LESSON is a create-time refusal (sts-fail-closed): an expected
   *  422 at Start renders on the card AND counts the demo complete. Without this
   *  flag a 422 only renders the error — a CC3-floored card 422'ing on a
   *  Fence-only host must NOT falsely earn its checkmark. */
  refusalCompletes?: boolean;
}

export const DEMOS: Demo[] = [
  {
    id: "sealed-box",
    section: "egress",
    title: "The sealed box",
    teaches: "Default-deny egress: an unlisted host is refused outright — no prompt, no wait.",
    overview:
      "The strictest posture: the sandbox has no allowed destinations and never asks. Any host you didn't pre-approve is refused at the proxy the instant it's dialed — the agent can't reach out, can't leak, and can't stall waiting on a human. Reach for this when a task should touch nothing on the network.",
    policy: {
      ...SHARED,
      allowed_domains: [],
      first_use_approval: "always_deny",
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
      "In Policy, start from the Minimal template and empty allowed_domains — no hosts at all.",
      "Set \"first_use_approval\": \"always_deny\" — no prompt, no wait.",
      "Launch interactive and attach the terminal.",
    ],
  },
  {
    id: "fail-then-approve",
    section: "egress",
    title: "Fail, then approve",
    teaches: "deny_with_review: the first hit is denied but raises an approval; approve it and a retry passes.",
    overview:
      "Same default-deny, but a blocked host isn't the end of the story: the first attempt is denied AND raises an approval you can grant. Approve it and the very next try to that host succeeds — the grant sticks for the rest of the run. Good for exploratory work where you want to vet each new destination as it comes up.",
    policy: {
      ...SHARED,
      allowed_domains: [],
      first_use_approval: "deny_with_review",
    },
    steps: [
      {
        cmd: "curl -sSI https://example.com",
        text: "Fails, and an approval request appears below the terminal.",
      },
      { text: "Click Approve." },
      {
        text: "Run the same command again — HTTP/2 200. A plain Approve keeps it allowed for the rest of this run (the split button's caret offers other options).",
      },
    ],
    setupUi: [
      "New Run → pick any barrier (Fence is fine for a demo).",
      "In Policy, start from the Minimal template and empty allowed_domains — no hosts at all.",
      "Set \"first_use_approval\": \"deny_with_review\" — refused now, raised for review.",
      "Launch interactive; denied requests surface in the Approvals panel below the terminal.",
    ],
  },
  {
    id: "held-at-the-door",
    section: "egress",
    title: "Held at the door",
    teaches: "wait_for_review: Wardyn HOLDS the connection open while it waits for your live decision.",
    overview:
      "The interactive variant: instead of failing fast, Wardyn holds the connection open while it waits for your live decision, so an approved request completes in the same command — no retry needed. Miss the ~30-second window and it falls back to a denial. Best when a human is watching and you want zero-retry approvals.",
    policy: {
      ...SHARED,
      allowed_domains: [],
      first_use_approval: "wait_for_review",
    },
    steps: [
      {
        cmd: "curl -sSI --max-time 60 https://example.com",
        text: "The command HANGS: Wardyn is holding the connection open, waiting for you.",
      },
      {
        text: "Within ~30 seconds, click Approve below — the same hanging command completes. (Miss the window and it falls back to a 403 stamped X-Wardyn-Egress: approval-pending — 'wait, then retry', not a hard no. Approve and re-run.)",
      },
      {
        cmd: "curl -sSI --max-time 60 https://wikipedia.org",
        text: "click Deny — instant refusal.",
      },
    ],
    setupUi: [
      "New Run → pick any barrier.",
      "In Policy, start from the Minimal template and empty allowed_domains — no hosts at all.",
      "Set \"first_use_approval\": \"wait_for_review\" — the connection is HELD while you decide.",
      "Launch interactive and keep the Approvals panel visible — you have ~30s to decide each held request.",
    ],
  },
  {
    id: "lines-that-cant-be-crossed",
    section: "egress",
    title: "Lines that can't be crossed",
    teaches: "allow_all_egress: the public internet is open, yet cloud-metadata and private/LAN addresses are still refused — the floor beneath every policy.",
    overview:
      "The opposite extreme: egress is wide open to the public internet, yet two addresses stay out of reach — the cloud-metadata endpoint (169.254.169.254, where cloud credentials live) and every private/LAN range. These are not policy you wrote: the proxy's floor refuses them beneath every policy — even this one's allow-all — and each attempt lands in the Audit panel as a deny (builtin:private-ip). No toggle opens them and there is no approval to raise; the one exception is an operator writing a literal address into the allowlist itself. Run this to see the limits that were never yours to change.",
    caution:
      "Fence (CC1) shares your machine's kernel and this box allows the public internet — the widest window Wardyn opens. It's safe here only because nothing is mounted: no repo, no key, no workspace. The point of this demo is the two addresses that stay unreachable with egress wide open.",
    policy: {
      ...SHARED,
      allowed_domains: [],
      allow_all_egress: true,
      first_use_approval: "always_deny",
    },
    steps: [
      {
        cmd: "curl -sSI https://example.com",
        text: "Works: this sandbox allows the public internet.",
      },
      {
        cmd: "curl -sSI --max-time 5 http://169.254.169.254/latest/meta-data/",
        text: "Refused — a 403 stamped by the proxy. That address is where cloud credentials live, and the floor denies it beneath every policy: the deny lands in the Audit panel as builtin:private-ip. There was never a decision to make — no approval was raised, and none can be.",
      },
      {
        cmd: "curl -sSI --max-time 5 http://192.168.1.1/",
        text: "Refused the same way — 403, and its own deny row. Private/LAN ranges sit under the same floor: nothing to approve, but everything on the record.",
      },
    ],
    setupUi: [
      "New Run → pick a barrier (Fence here; nothing is mounted, so the blast radius is a bare sandbox).",
      "In Policy, pick the 'Allow-all — observe first' template — \"allow_all_egress\": true.",
      "The cloud-metadata + private-range limits aren't settings — the proxy's floor refuses them, on the record, beneath any policy.",
      "Launch interactive, reach a public host, then try 169.254.169.254 and a 192.168.x.x address.",
    ],
  },
  {
    id: "denied-however-spelled",
    section: "egress",
    title: "Denied, however you spell it",
    teaches:
      "denied_domains beats allow-all — every FQDN spelling of a blocked host meets the same deny, and the 403 names its reason in response headers.",
    overview:
      "Egress is wide open except for one host you explicitly denied — denied_domains wins over allow_all_egress. Then try to dodge the list: \"example.com.\" with a trailing dot is a legal FQDN spelling of the very same host, and the classic way past a naive deny-list. Wardyn normalizes every spelling to one canonical host before any matching, so the dodge meets the identical 403 — and the refusal explains itself in response headers: X-Wardyn-Egress: denied, X-Wardyn-Egress-Reason: policy:denied, and X-Wardyn-Host naming the canonical host it matched, not the spelling you sent. A script inside the sandbox can tell a hard no from a not-yet-approved wait without guessing.",
    caution:
      "Fence (CC1) shares your machine's kernel and this box allows the public internet — safe here only because nothing is mounted: no repo, no key, no workspace. The point is the one host that stays out of reach no matter how it's spelled.",
    policy: {
      ...SHARED,
      allowed_domains: [],
      allow_all_egress: true,
      denied_domains: ["example.com"],
      first_use_approval: "always_deny",
    },
    steps: [
      {
        cmd: "curl -sSI https://example.org",
        text: "Works: egress is wide open — this policy allows the public internet.",
      },
      {
        cmd: "curl -sSI --max-time 5 https://example.com",
        text: "Refused — 403, even under allow-all: denied_domains always wins. The response headers say why (X-Wardyn-Egress: denied, X-Wardyn-Egress-Reason: policy:denied) and the deny lands in the Audit panel as policy:denied.",
      },
      {
        cmd: "curl -sSI --max-time 5 https://example.com.",
        text: "The dodge: a trailing dot is a legal spelling of the SAME host, and it used to slip past naive deny-lists. Refused identically — and X-Wardyn-Host: example.com shows the proxy matched the canonical host, not your spelling.",
      },
    ],
    setupUi: [
      "New Run → pick a barrier (Fence here; nothing is mounted, so the blast radius is a bare sandbox).",
      "In Policy, pick the 'Allow-all — observe first' template — \"allow_all_egress\": true.",
      "Add \"denied_domains\": [\"example.com\"] — the one line that beats allow-all.",
      "Launch interactive, reach a public host, then try the denied host — dotted and undotted.",
    ],
  },
  {
    id: "agent-in-the-box",
    section: "egress",
    title: "The agent in the box",
    teaches: "The flagship path: run a real coding agent in the terminal, bound by the exact same policy primitives as the demos above.",
    overview:
      "Every demo above proved the policy holds against a human at a terminal. This one hands you the same terminal to run a real Claude Code agent: it reaches Anthropic's API to think, and nothing else — the same default-deny-plus-allowlist confinement, now doing the job Wardyn actually exists for. It uses the model you connected in setup, injected proxy-side (never resident in the sandbox). A run doesn't have to be an agent at all — Wardyn governs any sandboxed workload — but this is the flagship case, so it gets its own demo.",
    policy: {
      ...SHARED,
      allowed_domains: ["api.anthropic.com", "*.anthropic.com"],
      first_use_approval: "always_deny",
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
      "In Policy, pick the 'Model provider only' template and trim allowed_domains to api.anthropic.com and *.anthropic.com.",
      "Launch interactive, attach the terminal, and run `claude` yourself.",
    ],
  },
  {
    id: "record-a-policy",
    section: "egress",
    title: "Record a policy",
    teaches:
      "Run a task open once, then synthesize the least-privilege policy from what it actually did, and re-run it confined.",
    overview:
      "Recording flips the usual order: instead of guessing an allowlist upfront, you let a task run with egress wide open, then Wardyn reads back exactly what it reached — every host, every method — and proposes the least-privilege policy that would have let it through. Approve it and the next run is confined to only that. Same idea Record Mode uses on a real workspace, here with nothing but a terminal.",
    caution:
      "Fence (CC1) plus allow-all egress is the weakest combination Wardyn offers — open on purpose, so there's something real to record. It's safe here only because nothing is mounted: no repo, no key, no workspace.",
    policy: {
      ...SHARED,
      allowed_domains: [],
      allow_all_egress: true,
      first_use_approval: "always_deny",
    },
    steps: [
      {
        cmd: "curl -sSI https://pypi.org",
        text: "Reaches out to a package registry — recorded, not blocked. This is the point: nothing is denied while recording.",
      },
      {
        cmd: "curl -sSI https://registry.npmjs.org",
        text: "A second registry. Every host you touch becomes a candidate line in the synthesized policy.",
      },
      {
        cmd: "curl -sSI https://example.com",
        text: "A third, unrelated host — recorded the same way, so you can see the synthesis include (or you could trim) it.",
      },
      {
        text: "Click End demo below, then “Turn this into a policy” — Wardyn proposes an allowlist of exactly the hosts above, ready to save and re-run confined.",
      },
    ],
    setupUi: [
      "New Run → pick the Fence (CC1) barrier.",
      "In Policy, pick the 'Allow-all — observe first' template — \"allow_all_egress\": true is what makes the recording honest.",
      "Launch interactive, attach the terminal, and run whatever the task actually needs.",
      "On the run's own page, Audit → 'Make a policy from this run' synthesizes one from what it did — same action this demo's “Turn this into a policy” takes.",
    ],
  },
  {
    id: "once-or-for-good",
    section: "egress",
    title: "Once, or for good",
    teaches: "The Once scope grants exactly one connection, not the run — approve it and the very next attempt has to ask again.",
    overview:
      "Every approval above stuck around for the rest of the run once granted. Once is narrower: it spends itself on the single connection it was raised for, so the next attempt to that same host is refused all over again and raises a brand-new approval — nothing lingers by accident. Reach for it to unblock one call without opening the host for good.",
    policy: {
      ...SHARED,
      allowed_domains: [],
      first_use_approval: "deny_with_review",
    },
    steps: [
      {
        cmd: "curl -sSI https://example.com",
        text: "Fails, and an approval request appears below the terminal.",
      },
      {
        text: "Click the split button's caret next to Approve and choose Once — one connection, not the rest of the run.",
      },
      {
        cmd: "curl -sSI https://example.com",
        text: "Same command — HTTP/2 200. Run it a third time and it's refused all over again: the Once grant already spent itself, so it has to ask again.",
      },
    ],
    setupUi: [
      "New Run → pick any barrier (Fence is fine for a demo).",
      "In Policy, start from the Minimal template and empty allowed_domains — no hosts at all.",
      "Set \"first_use_approval\": \"deny_with_review\" — refused now, raised for review.",
      "Launch interactive; when a request appears, use the split button's caret to grant Once instead of a plain Approve.",
    ],
  },

  ...SECRETS_DEMOS,
];
