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
// - secrets (three, all keyless): governance for a stored VALUE rather than a
//   destination — write-only-by-design (no route ever reads a secret back),
//   key-never-in-the-box (an api_key grant injects proxy-side, never resident),
//   and authorized-not-issued (an approval-gated, single-use mint). The two
//   granted demos carry `needsSecret` — a missing secret 422s at run-create.
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
  /** Honest danger note (allow-all-egress demos only — CC1 + open egress). */
  caution?: string;
  policy: RunPolicySpec;
  /** Which Getting-Started sub-section this demo belongs to: network egress
   *  governance (the original seven) or secrets governance (keeping a value
   *  out of the sandbox, whether or not it ever touches egress). */
  section: "egress" | "secrets";
  steps: DemoStep[];
  /** How you'd set up a sandbox like this yourself, on the New run page. */
  setupUi: string[];
  /** True only for the harness demo — needs a connected model; demo-screen.tsx
   *  hides the card entirely (and gates Start) until llmReady. Like every demo it
   *  comes up idle for the operator to drive — here they run the agent CLI in the
   *  attached terminal (which is what makes "watch it live" honest). */
  needsModel?: boolean;
  /** Set only on a demo whose policy carries an api_key grant referencing this
   *  secret NAME (validateInlineSecretRefs collects api_key secrets regardless
   *  of requires_approval) — a missing secret 422s at run-create. Gated the
   *  same shape as needsModel; the Getting-Started step filtering that hides
   *  an unmet step is phase-2 work (setup/steps.ts), not this catalog. */
  needsSecret?: string;
}

// Shared across every demo: weakest barrier (runs anywhere), reaped 15 min after
// the operator walks away, and deliberately nothing else — no grants, no mounts,
// no repos. Spread into each policy FIRST so the barrier leads the rendered YAML
// (YamlBlock emits keys in insertion order).
const SHARED = {
  min_confinement_class: "CC1" as const,
  auto_stop_after_sec: 900,
};

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
        text: "Within ~30 seconds, click Approve below — the same hanging command completes. (Miss the window and it falls back to a 403 — approve and re-run.)",
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
    teaches: "allow_all_egress: the public internet is open, yet cloud-metadata and private/LAN addresses have no route out at all.",
    overview:
      "The opposite extreme: egress is wide open to the public internet, yet two addresses stay out of reach — the cloud-metadata endpoint (169.254.169.254, where cloud credentials live) and every private/LAN range. These are not policy: the sandbox has no route to them, so nothing reaches the proxy, there is no setting that opens them, and there is no approval to raise or deny. Run this to see the limits that were never yours to change.",
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
        text: "Fails instantly — curl can't connect. That address is where cloud credentials live, and the sandbox has no route to it: nothing reaches the proxy, so the Audit panel stays quiet. There was never a decision to make.",
      },
      {
        cmd: "curl -sSI --max-time 5 http://192.168.1.1/",
        text: "Fails the same way, and just as silently. Private/LAN ranges are off the map too — nothing to approve, nothing to log.",
      },
    ],
    setupUi: [
      "New Run → pick a barrier (Fence here; nothing is mounted, so the blast radius is a bare sandbox).",
      "In Policy, pick the 'Allow-all — observe first' template — \"allow_all_egress\": true.",
      "The cloud-metadata + private-range limits aren't settings — there is no route there to allow.",
      "Launch interactive, reach a public host, then try 169.254.169.254 and a 192.168.x.x address.",
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

  // ============================================================
  // Secrets demos — governance for a stored VALUE, tied to egress or not.
  // write-only-by-design goes FIRST: it's the precondition the other two
  // gate on (both reference the secret it walks the operator through adding).
  // ============================================================
  {
    id: "write-only-by-design",
    section: "secrets",
    title: "Write-only, even for you",
    teaches:
      "A stored secret's value can be replaced or removed, but never read back — not by the agent, not by the API, not by you.",
    overview:
      "Every secret you store in Wardyn goes into a one-way door: set it, rotate it, delete it — there is no route, anywhere, that hands the value back. This demo doesn't even need a policy grant to prove it; the negative holds before one exists. Add the secret on /secrets, then watch the terminal come up empty three different ways.",
    policy: {
      ...SHARED,
      allowed_domains: [],
      first_use_approval: "always_deny",
    },
    steps: [
      {
        cmd: "printenv | sort",
        text: "No key in the environment — this demo's policy carries no grant at all, so nothing was ever going to inject it.",
      },
      {
        cmd: "grep -rIl --exclude-dir={proc,sys,dev} wardyn-demo-key /etc /home /tmp /usr 2>/dev/null",
        text: "Nothing resident on disk either (scoped to skip /proc, /sys, /dev — a bare / grep can block on special files mid-demo).",
      },
      {
        cmd: 'curl -sS --noproxy \'*\' "$WARDYN_PROXY_URL/wardyn/v1/secrets/wardyn-demo-key"',
        text: "404 — there is no read-back route for a stored secret anywhere in Wardyn, not one gated to the operator either. Write-only isn't a permission you could escalate past; it's the only door that exists.",
      },
    ],
    setupUi: [
      "New Run → pick any barrier (Fence is fine for a demo).",
      "On /secrets, add a secret named \"wardyn-demo-key\" — the masked entry field and its write-only tooltip (\"can be replaced or removed, but never read back — not even by you\") are the same door this demo proves from the terminal.",
      "In Policy, start from the Minimal template and empty allowed_domains — set \"first_use_approval\": \"always_deny\". This demo doesn't need a grant to make its point.",
      "Launch interactive and attach the terminal.",
    ],
  },
  {
    id: "key-never-in-the-box",
    section: "secrets",
    title: "The key that never enters the box",
    needsSecret: "wardyn-demo-key",
    teaches:
      "A brokered api_key grant injects the header on the way OUT of the proxy — the sandbox itself never holds, sees, or can leak the value.",
    overview:
      "The sandbox is credentialed without ever being handed a credential: the header is stitched onto the outbound request only as it leaves the proxy, after the sandbox's own process already sent it. printenv and a scoped grep both come up empty, and the response carries no trace either — the proof lives in the Audit panel, stamped before you typed a single command.",
    policy: {
      ...SHARED,
      allowed_domains: ["example.com"],
      first_use_approval: "always_deny",
      eligible_grants: [
        {
          kind: "api_key",
          requires_approval: false,
          scope: { host: "example.com", header: "X-Wardyn-Demo", secret_name: "wardyn-demo-key", format: "%s" },
        },
      ],
    },
    steps: [
      { cmd: "printenv | sort", text: "No key in the environment." },
      {
        cmd: "grep -rIl --exclude-dir={proc,sys,dev} wardyn-demo-key /etc /home /tmp /usr 2>/dev/null",
        text: "Nothing resident on disk either — the value was never written into the box.",
      },
      {
        cmd: "curl -sSI http://example.com",
        text: "200 — but don't trust the response, trust the Audit panel below: it shows credential.mint and secret.read stamped at STARTUP, before you ran anything. That's when the box was credentialed — proxy-side, never inside.",
      },
      {
        cmd: "curl -sSI http://wikipedia.org",
        text: "Instant 403. Injecting a header never widens egress — the allowlist stays exact, and this host was never on it.",
      },
    ],
    setupUi: [
      "New Run → pick the Fence (CC1) barrier.",
      "In Policy, add an eligible grant: kind \"api_key\", host \"example.com\", header \"X-Wardyn-Demo\", secret_name pointing at a secret you've already stored.",
      "Set \"first_use_approval\": \"always_deny\" and allowed_domains to just that host.",
      "Launch interactive, attach the terminal, and watch the Audit panel — the mint happens before you type anything.",
    ],
  },
  {
    id: "authorized-not-issued",
    section: "secrets",
    title: "Authorized, not issued",
    needsSecret: "wardyn-demo-key",
    teaches:
      "requires_approval doesn't hand out a credential on request — it raises a human decision, and even an approved mint returns a RULE, never a value.",
    overview:
      "This grant needs a live approval before the broker will mint it, and it's single-use once it does. The sandbox asks for it itself, over the same broker route the proxy uses at startup, with no auth of its own — the proxy injects the run's own token. The first ask is refused pending review; approve it and the very next ask succeeds, returning an injection rule with no secret in it; ask a third time and it's refused again, because it already spent itself.",
    policy: {
      ...SHARED,
      allowed_domains: ["example.com"],
      first_use_approval: "always_deny",
      eligible_grants: [
        {
          kind: "api_key",
          requires_approval: true,
          ttl_seconds: 300,
          scope: { host: "example.com", header: "X-Wardyn-Demo", secret_name: "wardyn-demo-key", format: "%s" },
        },
      ],
    },
    steps: [
      {
        cmd: 'curl -sS --noproxy \'*\' -X POST -H "Content-Type: application/json" -d \'{"grant_id":"{grant_id}"}\' "$WARDYN_PROXY_URL/wardyn/v1/credentials/mint"',
        text: 'First mint — the sandbox\'s own request AUTO-CREATES the pending approval and comes back 409 {"code":"pending"}.',
      },
      {
        text: "Approve it below — the credential card appears right where you're already watching, not off on a separate screen.",
      },
      {
        cmd: 'curl -sS --noproxy \'*\' -X POST -H "Content-Type: application/json" -d \'{"grant_id":"{grant_id}"}\' "$WARDYN_PROXY_URL/wardyn/v1/credentials/mint"',
        text: "Same command again — 200, returning the injection RULE (host/header/format), never a value: the response's token field stays empty.",
      },
      {
        cmd: 'curl -sS --noproxy \'*\' -X POST -H "Content-Type: application/json" -d \'{"grant_id":"{grant_id}"}\' "$WARDYN_PROXY_URL/wardyn/v1/credentials/mint"',
        text: 'A third time — 409 {"code":"already_minted"}: single-use, already spent on the mint above. Check the Audit panel: deny → allow → deny, in that order — the two denials are the mechanism working, not a failure.',
      },
    ],
    setupUi: [
      "New Run → pick the Fence (CC1) barrier.",
      "In Policy, add an eligible grant: kind \"api_key\" with \"requires_approval\": true and a ttl_seconds — this is what raises the human decision instead of auto-minting.",
      "Set \"first_use_approval\": \"always_deny\".",
      "Launch interactive, attach the terminal, and mint from inside the sandbox — approve it in the panel below the terminal when it asks.",
    ],
  },
];
