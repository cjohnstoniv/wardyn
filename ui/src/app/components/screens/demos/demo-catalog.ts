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
   *  governance (the original seven) or secrets governance (keeping a value
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
      "The sandbox is credentialed without ever being handed a credential: the header is stitched onto the outbound request only as it leaves the proxy, after the sandbox's own process already sent it. printenv and a scoped grep both come up empty, and the response carries no trace either — the proof lives in the Audit panel, stamped before you typed a single command. This is the mechanism stripped to its bones — a made-up header on a made-up host; \"A bearer token for a real API\" below is the same law wired the way you'd actually write it. Start this one as an OPERATOR — a member launch silently drops the inline grant (the policy clamp), so the injection beats never fire.",
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
        text: "The secret's name and config aren't resident on disk either. (The VALUE never entering the box is the audit panel's proof, next step.)",
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
      "New Run → the barrier picker is not the lever here: an api_key grant to a host outside the coding-agent baseline (the agent's own model/VCS endpoints) raises this run's floor to Vault (CC3) at create, whatever tier you pick — a run holding a third-party credential is itself worth stealing.",
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
      "This grant needs a live approval before the broker will mint it, and it's single-use once it does. The sandbox asks for it itself, over the same broker route the proxy uses at startup, with no auth of its own — the proxy injects the run's own token. The first ask is refused pending review; approve it and the very next ask succeeds, returning an injection rule with no secret in it; ask a third time and it's refused again, because it already spent itself. Start this one as an OPERATOR — a member launch silently drops the inline grant, so there's nothing to approve.",
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
      "New Run → the barrier picker is not the lever here: an api_key grant to a host outside the coding-agent baseline (the agent's own model/VCS endpoints) raises this run's floor to Vault (CC3) at create, whatever tier you pick — a run holding a third-party credential is itself worth stealing.",
      "In Policy, add an eligible grant: kind \"api_key\" with \"requires_approval\": true and a ttl_seconds — this is what raises the human decision instead of auto-minting.",
      "Set \"first_use_approval\": \"always_deny\".",
      "Launch interactive, attach the terminal, and mint from inside the sandbox — approve it in the panel below the terminal when it asks.",
    ],
  },

  // ============================================================
  // Per-KIND secrets demos. The three above teach the mechanism; these five
  // teach that the mechanism CHANGES with the credential kind, because it is
  // the consuming protocol — not a Wardyn preference — that decides how far
  // out of the sandbox a credential can be kept. Read them as a ladder:
  // header-injected (never enters) → piped (enters, never rests) → resident
  // (rests, briefly) → re-originated (cannot even be asked for) → refused.
  // ============================================================
  {
    id: "rest-api-token",
    section: "secrets",
    title: "A bearer token for a real API",
    needsSecret: "wardyn-demo-api-token",
    teaches:
      "The everyday case: a third-party API key wired as a standard Authorization: Bearer header, attached at the boundary and never inside the box.",
    overview:
      "The demo above proves the law with a made-up header on a made-up host. This is the same law wired the way you'd actually write it: a plain REST call to a third-party service — a Stripe, a Slack, your own internal API — carrying \"Authorization: Bearer <token>\", where the token is a Wardyn secret the sandbox never holds. The request leaves the sandbox with no credential on it and arrives with one. Nothing about that is visible from inside; the Audit panel is the proof. Start this one as an OPERATOR — a member launch silently drops the inline grant (the policy clamp), so the injection never fires.",
    policy: {
      ...SHARED,
      allowed_domains: ["example.org"],
      first_use_approval: "always_deny",
      eligible_grants: [
        {
          kind: "api_key",
          requires_approval: false,
          scope: {
            host: "example.org",
            header: "Authorization",
            format: "Bearer %s",
            secret_name: "wardyn-demo-api-token",
          },
        },
      ],
    },
    steps: [
      {
        cmd: "curl -sSI http://example.org/v1/whatever",
        text: "The ordinary third-party API call. It left this box with no Authorization header and reached example.org carrying \"Authorization: Bearer <token>\" — the proxy stitched it on at the boundary, after the sandbox's own process had already sent the request. (example.org has no /v1/whatever, so it answers 404. The status code is not the point; what reached it is.)",
      },
      {
        cmd: "printenv | sort",
        text: "No token in the environment. This is the difference from every SDK you have ever wired: there is no API_TOKEN variable to end up in a log line, a crash dump, or a subprocess.",
      },
      {
        cmd: "grep -rIl --exclude-dir={proc,sys,dev} wardyn-demo-api-token /etc /home /tmp /usr 2>/dev/null",
        text: "Nothing resident on disk either — not the value, not the config that names it.",
      },
      {
        text: "The Audit panel below is where the proof actually lives: secret.read and credential.mint, stamped at STARTUP, before you typed anything. Invisible from inside the box is the point, not a gap in the demo.",
      },
    ],
    setupUi: [
      "New Run → the barrier picker is not the lever here: an api_key grant to a host outside the coding-agent baseline (the agent's own model/VCS endpoints) raises this run's floor to Vault (CC3) at create, whatever tier you pick — a run holding a third-party credential is itself worth stealing.",
      "On /secrets, add a secret named \"wardyn-demo-api-token\". In real life this is the SaaS key itself, pasted once into the masked field.",
      "In Policy, add an eligible grant: kind \"api_key\", host \"example.org\", header \"Authorization\", format \"Bearer %s\", secret_name \"wardyn-demo-api-token\" — format is the whole difference between this and the demo above.",
      "Set allowed_domains to just that host and \"first_use_approval\": \"always_deny\" — injecting a credential never widens egress, so the allowlist still has to name the host.",
      "Launch interactive, attach the terminal, and watch the Audit panel — the mint happens before you type anything.",
    ],
  },
  {
    id: "pat-stdout-only",
    section: "secrets",
    title: "A PAT that only ever exists in a pipe",
    needsSecret: "wardyn-demo-pat",
    teaches:
      "git-over-HTTPS is an opaque tunnel with no header to inject, so a PAT is minted on demand straight to STDOUT — never env, never disk, never argv — behind a caller-auth gate.",
    overview:
      "Everything above kept the credential OUT of the sandbox because HTTP let the proxy attach it on the way past. git cannot be done that way: a clone is an end-to-end CONNECT tunnel the proxy cannot read into, so there is no request to stitch a header onto. Wardyn's answer is the next-narrowest thing — the PAT is minted only when git asks for it and written to STDOUT ONLY, into git's pipe, where nothing else can pick it up. And before it will emit anything the helper makes the caller prove it is the process the run provisioned. You will watch it refuse first. The stored secret here is fake; the mechanism is entirely real.",
    policy: {
      ...SHARED,
      allowed_domains: [],
      first_use_approval: "always_deny",
      eligible_grants: [
        {
          kind: "git_pat",
          requires_approval: false,
          scope: { host: "gitlab.example.com", secret_name: "wardyn-demo-pat" },
        },
      ],
    },
    steps: [
      {
        cmd: "printf 'protocol=https\\nhost=gitlab.example.com\\n\\n' | wardyn-git-helper --secret-file ~/.wardyn/git-helper.secret get",
        text: "REFUSED, on camera: \"caller did not present WARDYN_GIT_HELPER_SECRET; refusing to emit a brokered token\" — and nothing at all on stdout. Your attach shell is a fresh exec, not a descendant of the process that provisioned the run, so it never inherited the per-run secret. That is the gate visibly deciding, not an error — and seeing it is also proof the gate was provisioned at all: with no secret file the helper falls back to its legacy allow and emits straight away. (Both calls are PIPED because `get` reads git's key=value block from stdin — an unpiped call just sits on the TTY.)",
      },
      {
        cmd: "export WARDYN_GIT_HELPER_SECRET=$(cat ~/.wardyn/git-helper.secret)",
        text: "Present the secret. The 0400 file is readable by the agent uid, so a process running AS the agent can pass this gate — a residual the helper's own package doc states plainly rather than hiding. It raises the bar from \"any process in the sandbox\" to \"code running as the agent\"; it does not claim to be a wall.",
      },
      {
        cmd: "printf 'protocol=https\\nhost=gitlab.example.com\\n\\n' | wardyn-git-helper --secret-file ~/.wardyn/git-helper.secret get",
        text: "Same command, and now the PAT comes back — as git-credential key=value lines on STDOUT and nowhere else. In a real clone git reads them straight off this pipe and they are gone; here you are standing in git's place to see it happen.",
      },
      {
        cmd: "printenv | sort",
        text: "The PAT is not here. The WARDYN_GIT_HELPER_SECRET you just exported IS — read it for what it is: a gate token that lets you ASK, not a credential. WARDYN_GIT_PAT_GRANTS is here too, and it holds grant ids, never secrets.",
      },
      {
        cmd: "grep -rIl --exclude-dir={proc,sys,dev} wardyn-demo-pat /etc /home /tmp /usr 2>/dev/null",
        text: "Nothing on disk. The mint went to a pipe; no file was ever written for you to find.",
      },
      {
        text: "Audit panel: credential.mint, stamped when YOU asked — not at startup. That is the other half of the difference from an injected api_key, which is minted once before the box even opens.",
      },
    ],
    setupUi: [
      "New Run → pick the Fence (CC1) barrier. Unlike an api_key to a third-party host, a git_pat grant does NOT raise the confinement floor — its scope carries no read/write flag, and flooring every SCM clone at Vault would block them on any host without a Vault-class runner.",
      "On /secrets, add a secret named \"wardyn-demo-pat\" — any value; the mint returns whatever is stored, so a fake proves the mechanism.",
      "In Policy, add an eligible grant: kind \"git_pat\", host \"gitlab.example.com\", secret_name \"wardyn-demo-pat\" (username is optional and defaults to the host's convention).",
      "Leave allowed_domains empty and set \"first_use_approval\": \"always_deny\" — the mint route is local to the proxy, so this demo needs no egress at all.",
      "Launch interactive and attach the terminal. In a real run you would never type the helper yourself: git invokes it, reads the pipe, and moves on.",
    ],
  },
  {
    id: "ssh-briefly-resident",
    section: "secrets",
    title: "The one that touches disk — briefly",
    needsSecret: "wardyn-demo-ssh-key",
    teaches:
      "git's SSH transport has NO credential-helper seam, so an SSH key cannot be brokered without becoming resident — Wardyn narrows the window instead of pretending it isn't there.",
    overview:
      "This is the documented exception, and the reason it is a demo rather than a footnote: the ssh client reads a private key from a FILE, so unlike every kind above there is no way to keep this one out of the sandbox. Wardyn's answer is a window instead of a wall — the key is written 0400 just before the clone and shredded right after. THIS IS A RE-ENACTMENT, and narrated as one: on a real run that whole window opens and closes during startup, before attach is even allowed, so nobody can ever watch it live. You will find an empty ~/.ssh and the startup mint already on the record, then re-play the window by hand — the same local mint route, the same node one-liner, the same 0400 file — and shred it yourself. Fake keypair; nothing authenticates.",
    policy: {
      ...SHARED,
      allowed_domains: [],
      first_use_approval: "always_deny",
      eligible_grants: [
        {
          kind: "ssh_key",
          requires_approval: false,
          scope: { host: "github.com", key_secret_ref: "wardyn-demo-ssh-key" },
        },
      ],
    },
    steps: [
      {
        cmd: "ls -la ~/.ssh",
        text: "Empty — and that is the first half of the lesson. The key WAS here: startup minted it, wrote it 0400, and shredded it before the attach gate opened. You are looking at the after.",
      },
      {
        text: "Now look at the Audit panel below: credential.mint, stamped at STARTUP. That row is the window you could never have watched. Everything from here on is an honest re-enactment of it, run by hand at the same mint route.",
      },
      {
        cmd: "curl -fsS --noproxy '*' -X POST -H 'Content-Type: application/json' -d '{\"grant_id\":\"{grant_id}\"}' \"$WARDYN_PROXY_URL/wardyn/v1/credentials/mint\" | (mkdir -p ~/.ssh; node -e 'let d=\"\";process.stdin.on(\"data\",c=>d+=c).on(\"end\",()=>{const o=JSON.parse(d);require(\"fs\").writeFileSync(process.env.HOME+\"/.ssh/id_wardyn_demo\",o.token.endsWith(\"\\n\")?o.token:o.token+\"\\n\",{mode:0o400})})') && chmod 0400 ~/.ssh/id_wardyn_demo",
        text: "The re-play. A value-bearing grant is re-mintable BY DESIGN (single-use only binds an APPROVED mint), so the same route hands the key back. node does the extraction because this image ships no jq and no python — that is exactly the one-liner startup itself uses.",
      },
      {
        cmd: "ls -l ~/.ssh/id_wardyn_demo",
        text: "-r-------- , agent-owned. THIS is the window: for the length of a clone, and only then, a private key is a real file in a real sandbox. Wardyn does not claim otherwise.",
      },
      {
        cmd: "P=\"${WARDYN_PROXY_URL#*//}\"; ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o ConnectTimeout=10 -o \"ProxyCommand corkscrew ${P%%:*} ${P##*:} %h %p\" -i ~/.ssh/id_wardyn_demo -p 443 git@ssh.github.com",
        text: "Refused — the stored key is fake, so GitHub rejects it. The CONNECT itself was allowed: creating a run with an ssh_key grant unions ssh.github.com:443 into allowed_domains, because a key you cannot reach the host with governs nothing. Note what is NOT open: github.com:22, and every other host.",
      },
      {
        cmd: "shred -u ~/.ssh/id_wardyn_demo 2>/dev/null || rm -f ~/.ssh/id_wardyn_demo; ls -la ~/.ssh",
        text: "Gone — and this is agent-run's own wipe verbatim, `|| rm -f` included. shred needs to WRITE the file it overwrites, and a 0400 key is not writable even by its owner, so shred declines and the rm behind it does the work: the file is unlinked, not scrubbed. Startup's wipe has always behaved exactly this way; the demo shows it rather than tidying it up. The lesson isn't that this kind is safe — it's that the exception is bounded, documented, and the only one.",
      },
    ],
    setupUi: [
      "New Run → pick the Fence (CC1) barrier. An ssh_key grant does not raise the confinement floor (same reasoning as git_pat), but it IS the one kind that puts a credential on disk — pick your barrier with that in mind on a real run.",
      "On /secrets, add a secret named \"wardyn-demo-ssh-key\" — the private key PEM. A fake one is enough to prove the window; only the clone needs a real one.",
      "In Policy, add an eligible grant: kind \"ssh_key\", host \"github.com\", key_secret_ref \"wardyn-demo-ssh-key\". The host must be a supported SSH-over-443 provider (github.com / dev.azure.com) — anything else is refused at policy write, and would skip the startup mint entirely.",
      "Leave allowed_domains empty and set \"first_use_approval\": \"always_deny\" — run-create adds ssh.github.com:443 for you, and only that.",
      "Launch interactive and attach. On a real run you would never type any of this: startup mints, clones and shreds before the attach gate opens.",
    ],
  },
  {
    id: "github-app-broker",
    section: "secrets",
    title: "A token the sandbox never even sees",
    needsGitHubApp: true,
    teaches:
      "The strongest lane: a GitHub App installation token is minted proxy-side and attached after re-origination — the sandbox holds no token and is REFUSED if it asks for one.",
    overview:
      "One step past every demo above. A git_pat at least passes through the sandbox on its way to git; this one never arrives at all. git is pointed at a broker route on the proxy, which mints a short-lived, repo-scoped installation token from the live GitHub API and re-originates the request with it — so the token exists only on the proxy's outbound leg. Ask the mint route for it from inside and you are refused by name. That live GitHub API is also why this card cannot be faked: without a configured GitHub App there is nothing to mint from, so the card teaches and Start stays closed.",
    policy: {
      ...SHARED,
      allowed_domains: [],
      first_use_approval: "always_deny",
      eligible_grants: [
        {
          kind: "github_token",
          requires_approval: false,
          scope: { repos: ["octocat/Hello-World"], permissions: { contents: "read" } },
        },
      ],
    },
    steps: [
      {
        cmd: "printenv | grep '^WARDYN_G'",
        text: "WARDYN_GITHUB_GRANT_ID and WARDYN_GIT_BROKER_REPOS: a grant id and a repo allowlist. Both are eligibility records, not credentials. Search this box as hard as you like — there is no token to find, because none was ever sent.",
      },
      {
        cmd: "curl -sS --noproxy '*' -X POST -H \"Content-Type: application/json\" -d '{\"grant_id\":\"{grant_id}\"}' \"$WARDYN_PROXY_URL/wardyn/v1/credentials/mint\"",
        text: "403, and read the reason: \"this grant is brokered on /wardyn/gh/; the GitHub App installation token is minted proxy-side and never enters the sandbox\". The route that hands a git_pat back happily refuses THIS grant on purpose — handing the token over would defeat the per-repo allowlist and the push branch-namespace parser it exists to enforce.",
      },
      {
        cmd: "curl -sSI --max-time 10 https://github.com",
        text: "Denied — twice over, and that is the lesson. This demo's allowlist is empty, so default-deny alone refuses it; but a brokered run ALSO denies the managed GitHub hosts by name even with a wide allowlist. The forge is reachable by ONE route, /wardyn/gh/<org>/<repo>, where every request is parsed against the grant's repo scope. A run that could dial github.com directly would have a second, unparsed path — so it doesn't get one.",
      },
      {
        text: "In a real brokered run git never notices any of this: an insteadOf rewrite sends the declared repos through the broker, the proxy mints and attaches, and pushes land confined to refs/heads/wardyn/<run-id>/. This demo has no repos to clone, so what it shows you is the negative — the token you cannot reach.",
      },
    ],
    setupUi: [
      "First configure a GitHub App under Settings — App id and private key. Without both, this demo's Start stays disabled: the token is minted from the live GitHub API, so there is nothing to fake locally.",
      "New Run → pick the Fence (CC1) barrier. A github_token grant floors the run at Vault (CC3) only when its permissions are WRITE-capable; \"contents\": \"read\" here does not.",
      "In Policy, add an eligible grant: kind \"github_token\" with scope.repos naming \"<org>/<repo>\" and scope.permissions — the repo, not the host, is the unit of trust.",
      "Leave allowed_domains empty and set \"first_use_approval\": \"always_deny\" — the broker route is local to the proxy, and run-create denies the brokered GitHub hosts for you.",
      "Launch interactive with a repo declared, and git clone it: the URL rewrite is already in the run's git config.",
    ],
  },
  {
    id: "sts-fail-closed",
    section: "secrets",
    title: "No identity, no credential",
    teaches:
      "A cloud_sts grant needs an attested workload identity — and without one Wardyn refuses the RUN, not the mint: no sandbox ever starts.",
    overview:
      "The last rung is the one where nothing is handed out at all. A cloud_sts grant exchanges the workload's own attested identity for short-lived cloud credentials, which means it is only meaningful when something is actually attesting — a SPIRE identity provider. On the embedded provider there is nothing to attest with, so the refusal does not wait for a mint: it fires at run-create, and no sandbox is ever built. There is no terminal in this demo and that IS the demo. Press Start and watch Wardyn refuse; the refusal lands on this card and counts as completed, because seeing it is the whole lesson.",
    policy: {
      ...SHARED,
      allowed_domains: [],
      first_use_approval: "always_deny",
      eligible_grants: [{ kind: "cloud_sts", requires_approval: false, scope: {} }],
    },
    steps: [
      {
        text: "Press Start demo. Nothing will attach — the run is refused before a sandbox exists, and the refusal is rendered right here on the card instead of a toast you'd miss.",
      },
      {
        text: "Read WHICH gate refused — the message names it, and which one you get depends on this host. With a Vault-class runner it is the identity gate: \"policy requires the spire identity provider\". Without one the RUNNER gate fires first — \"cannot enforce confinement_class CC3\" — because a cloud_sts grant is write-capable, so it floors the run at Vault before identity is ever consulted. Two gates, both fail-closed, either one enough.",
      },
      {
        text: "Nothing started, so there is nothing to attach to, kill, or clean up. That is what fail-closed buys: the credential was never reachable — not reached and then refused.",
      },
      {
        text: "With SPIRE wired, this same policy is what a run uses to reach AWS/GCP/Azure with credentials minted per run against an attested identity: no long-lived cloud key is stored anywhere, so there is none to leak. This card teaches that shape; it does not simulate it.",
      },
    ],
    setupUi: [
      "New Run → the barrier is decided for you: a cloud_sts grant is write-capable, so run-create floors it at Vault (CC3) whatever you pick, and refuses outright on a host that cannot enforce it.",
      "In Policy, add an eligible grant: kind \"cloud_sts\" — its scope is an empty object; the identity provider, not the policy, decides what it can exchange for.",
      "Leave allowed_domains empty and set \"first_use_approval\": \"always_deny\". You will not get far enough to need egress.",
      "Launch: on the embedded identity provider this 422s at create, by design. Wiring a SPIRE identity provider is what turns this policy from a lesson into a working run.",
    ],
  },
];
