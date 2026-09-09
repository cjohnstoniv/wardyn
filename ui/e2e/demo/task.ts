/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * Everything the demo recording says or types that is NOT a UI string, in one
 * file. When the video needs a different task, a different workspace path or a
 * different host, this is the only file to edit — walkthrough.spec.ts should
 * read as choreography, not content.
 *
 * The prose copy of this task lives in examples/workspaces/demo-node/TASK.md
 * with the reasoning behind each step; keep the two in sync.
 */

/** Where scripts/record-demo.sh materializes the workspace copy. */
export const WORKSPACE_PATH =
  process.env.WARDYN_DEMO_WORKSPACE ||
  `${process.env.WARDYN_DEMO_ROOT || `${process.env.HOME}/wardyn-demo`}/slugify`;

/** The name typed into the Add workspace dialog. */
export const WORKSPACE_NAME = "slugify";

/**
 * The run's TITLE — required on the New run screen, and the key runs are
 * grouped by on the Runs board. Kept short: it is the run's headline in the
 * board card, the table row and the run-detail command bar, all on camera.
 */
export const DEMO_TITLE = "Slugify + reach two hosts";

/**
 * Title for the SECOND, much smaller run Act 5 launches after the `always`
 * decision — proof that the permanent grant needs no fresh approval, not more
 * agent work. A bare shell, not the agent CLI, so it needs no task text.
 */
export const PROOF_RUN_TITLE = "Same host, no approval this time";

/**
 * The run's task. Numbered so the agent's path stays tight enough to film.
 *
 * ORDER IS CHOREOGRAPHY. The curl is step 1 because the held host is the beat
 * the video is built around: front-loaded, it surfaces ~a minute after launch
 * instead of after the whole coding stretch, so the decision happens early and
 * the long unwatchable part lands AFTER it — where the recorder's fast-forward
 * span (overlay.ts's ffwdStart/ffwdEnd) can compress it.
 *
 * Step 1's --max-time 90 is load-bearing: it has to outlast the 30s
 * wait_for_review hold (defaultHoldTimeout, internal/egress/proxy/approvals.go)
 * so the operator's on-camera approval lands while the request is still open
 * and the SAME curl completes.
 *
 * There is deliberately NO 169.254.169.254 metadata probe here any more. V01
 * teaches the line-that-cannot-be-crossed on the keyless funnel, in seconds;
 * paying a minute of a quota-bound agent run to re-teach it is the trade this
 * reorder refuses.
 */
export const DEMO_TASK = [
  // The retry is load-bearing, not padding. The operator ceiling clamps this
  // run's first_use_approval down to deny_with_review, so an off-policy host is
  // REFUSED outright (403 at the CONNECT, curl reports 000) and raises an
  // approval — and it is the RETRY after approval that succeeds. Without this
  // sentence the agent reads the 403, writes it down and moves on, and the
  // demo's whole payoff (approve → it goes through) never happens on camera.
  "1. Run: curl -sS --max-time 90 -o /dev/null -w '%{http_code}' https://example.com/ — if it is refused (status 000, or a 403 from the proxy), wait 20 seconds and run the exact same command once more, because an operator may be approving the host while you wait. Repeat that retry up to 3 times, then write the final status code into NOTES.md.",
  "2. Add a slugify(s) function to src/slug.js that lower-cases the input, replaces every run of non-alphanumeric characters with a single hyphen, and trims leading and trailing hyphens. Add a matching test to test/slug.test.js, then run `node --test` and make sure every test passes.",
  "3. Finish NOTES.md with one short paragraph: the final status code you got for example.com, and what you built in step 2.",
].join("\n");

/** The host step 1 reaches for — off the allowlist, so it gets held. */
export const HELD_HOST = "example.com";

/** Allow-listed outright: without it the subscription credential is refused. */
export const MODEL_HOST = "api.anthropic.com";

/**
 * The five keyless funnel demos, with the command each one's card tells the
 * operator to run and the caption that explains what the viewer is watching.
 * Ids and commands mirror ui/src/app/components/screens/demos/demo-catalog.ts.
 *
 * `scope` is the decision scope Act 3 picks when `approve` is true — "run" is
 * the split button's plain click (today's default, byte-identical to before
 * this field existed); once-or-for-good is the one demo that needs its
 * caret's "Once" instead. Explicit on every entry (not defaulted at the read
 * site) so the union stays homogeneous — see walkthrough.spec.ts's decide().
 */
export const FUNNEL_DEMOS = [
  {
    id: "sealed-box",
    label: "The sealed box",
    cmds: ["curl -sSI https://example.com"],
    caption: "Nothing is allow-listed, and this policy never asks. The refusal is instant.",
    approve: false,
    scope: "run",
  },
  {
    id: "fail-then-approve",
    label: "Fail, then approve",
    // Same command twice on purpose: the first is refused and raises the
    // approval, the retry after approval is what succeeds.
    cmds: ["curl -sSI https://example.com", "curl -sSI https://example.com"],
    caption: "Same default-deny — but the refusal also raises an approval you can grant.",
    approve: true,
    scope: "run",
  },
  {
    id: "held-at-the-door",
    label: "Held at the door",
    cmds: ["curl -sSI --max-time 60 https://example.com"],
    caption: "This time the connection is held open, waiting on a human. Nothing is refused behind your back.",
    approve: true,
    scope: "run",
  },
  {
    id: "lines-that-cant-be-crossed",
    label: "Lines that can't be crossed",
    cmds: [
      "curl -sSI https://example.com",
      "curl -sSI --max-time 5 http://169.254.169.254/latest/meta-data/",
      "curl -sSI --max-time 5 http://192.168.1.1/",
    ],
    caption: "Egress wide open to the public internet — and two lines that still hold.",
    approve: false,
    scope: "run",
  },
  {
    id: "once-or-for-good",
    label: "Once, or for good",
    // Same shape as fail-then-approve: the same command twice, a decide in
    // between. The difference is entirely in `scope` below — Once instead of
    // the split button's plain (This run) click.
    cmds: ["curl -sSI https://example.com", "curl -sSI https://example.com"],
    caption: "Same default-deny — but this time the grant is Once, not This run.",
    approve: true,
    scope: "once",
  },
] as const;

/**
 * Act 5 WITHOUT a model.
 *
 * Wardyn governs any workload — a coding agent is the flagship case, not the
 * only one — so the entire governance story (a held host, an operator decision
 * scoped to `always`, the workspace receipt, a later run that never has to ask)
 * needs no LLM at all. Only "an agent writes the code" does.
 *
 * Set WARDYN_DEMO_SHELL_ACT5=1 to record that variant: a Shell command run that
 * runs the workspace's real tests, reaches the same two hosts, and writes the
 * same NOTES.md. Used when the Claude subscription has no quota left, so a take
 * is still honest and complete rather than showing a failed agent.
 */
export const SHELL_ACT5 = process.env.WARDYN_DEMO_SHELL_ACT5 === "1";

/**
 * The shell equivalent of DEMO_TASK: same work, same two hosts, no model.
 *
 * THE RETRY LOOP IS LOAD-BEARING, for a reason that has nothing to do with
 * curl. `LiveApprovals` only renders while `run.state === "RUNNING"`
 * (run-detail.tsx), so a run that finishes in ten seconds takes the approval
 * strip down with it before an operator can decide anything — the row is
 * raised, then vanishes. An agent run is slow enough to hide this; a shell run
 * is not. Looping keeps the run alive while the decision happens, which is also
 * exactly what a real unattended job would do when a host it needs is refused.
 */
export const DEMO_COMMAND = [
  "node --test 2>&1 | tail -4",
  "echo",
  "code=000; i=1",
  "while [ $i -le 8 ]; do",
  `  code=$(curl -sS --max-time 25 -o /dev/null -w '%{http_code}' https://example.com/ 2>/dev/null || echo 000)`,
  '  [ "$code" = "200" ] && break',
  '  echo "attempt $i: example.com refused ($code) — waiting on an operator decision..."',
  "  sleep 12; i=$((i+1))",
  "done",
  `printf 'example.com -> %s\\n' "$code" | tee NOTES.md`,
  `printf 'metadata     -> ' | tee -a NOTES.md`,
  `(curl -sS --max-time 5 http://169.254.169.254/latest/meta-data/ 2>&1 || true) | head -1 | tee -a NOTES.md`,
  "echo; cat NOTES.md",
].join("\n");
