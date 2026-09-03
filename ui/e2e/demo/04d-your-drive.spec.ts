/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * V04d — "Your drive". The multi-user path's storage episode, and the cloud
 * headline: the one place Wardyn asks the CLUSTER to hold state on a person's
 * behalf, and the one thing a sandbox by definition destroys.
 *
 *     WARDYN_DEMO_SKIP_MODEL=1 WARDYN_DEMO_BASE_URL=http://localhost:8280 \
 *       scripts/record-demo.sh --video 04d
 *
 * THE SCRIPT IS ADJUDICATED — local/review-0.7/dialog/04d-script.md, after
 * four persona lanes and the ruling in its §8 (ledger:
 * local/review-0.7/dialog/04d-REWRITE-SET.md). Its §2 beat table and §3
 * transcript are in LOCKSTEP: every caption below is one line of §3, in order,
 * unedited, and the C-numbers in the comments are that transcript's. Two
 * sentences are FROZEN CANON quoted verbatim on the soundtrack — C19-C22 are
 * DRIVES.HONESTY's four sentences and C60 quotes DRIVES.RECLAIM_HINT — and are
 * "verbatim or not at all" (the grader greps for them). Wording changes go
 * through the script, never through this file.
 *
 * THE BLOCKER THE ROUND FIXED, and why the shape is what it is. The first draft
 * said "the first one's disk went with it" over a run that was still RUNNING —
 * which would have filmed two live pods sharing one claim (DESIGN.md §6.1
 * residual 36) under the title "survives the sandbox". The remedy is on camera
 * and is not trimmable: a CONTROL FILE written outside the drive (3.15), run
 * A's own `run.drive.mount` audit row naming the volume the preview printed
 * (3.16), run A KILLED on camera (3.17), the control file shown ABSENT in run B
 * (4.5b), and only then the note read back (4.6). Cutting any one of them turns
 * a proof back into a claim.
 *
 * STATE CONTRACT — batch D, immediately after 04c, on the kind cluster
 * `wardyn-quickstart` at :8280 with the SSO overlay. Staged off camera and
 * narrated nowhere:
 *   1. The cluster 02c built: healthz.runner=k8s, healthz.sso=true, onboarded,
 *      admin@/member@wardyn.local live in Dex.
 *   2. THE CHART FLIP (12b's shape). Without it the runner Role lacks
 *      persistentvolumeclaims: [get, create] and beat 3.11 dies as a §7.7
 *      REFUSED_BACKEND naming the flag:
 *        helm --kube-context kind-wardyn-quickstart upgrade wardyn \
 *          deploy/helm/wardyn -n wardyn --reuse-values \
 *          --set userDrives.enabled=true \
 *        && kubectl --context kind-wardyn-quickstart -n wardyn \
 *          rollout status deploy/wardyn
 *      C8 names the flag's EFFECT ("this cluster's chart lets Wardyn ask it for
 *      disks"), never the flag.
 *      THE KEY IS TOP-LEVEL. 04d-script.md §5 #2 spells it
 *      `k8s.userDrives.enabled`, and this file carried that spelling too; the
 *      chart reads `.Values.userDrives` (values.yaml:411, rbac.yaml:60, and
 *      the Makefile's own render gates at :541,567-568). A `--set
 *      k8s.userDrives.enabled=true` upgrade SUCCEEDS and sets a key nothing
 *      reads, so the flip silently does not happen and 3.11 dies six minutes
 *      into the take. Owner ledger: the script's §5 #2 needs the same fix.
 *   3. NO user_drives / user_drive_grants rows, so beat 1.4 films the real
 *      empty state.
 *   4. SINGLE-NODE kind. Its default `standard` class is rancher.io/local-path
 *      with WaitForFirstConsumer, so the claim binds to whichever node schedules
 *      run A's pod; on a multi-node kind run B can land elsewhere and
 *      ReadWriteOnce will not reattach — and the read-back beat IS the episode.
 *      `kubectl get nodes` must show one node before the take. local-path also
 *      IGNORES requests.storage, which is exactly what C18b's "here, it does
 *      not" and the frozen C20 rely on.
 *   5. WARDYN_DEMO_SKIP_MODEL=1 — nothing here calls a model.
 *
 * 2 AND 4 ARE THE TWO THAT COST A WHOLE TAKE, and were documented here and
 * enforced nowhere. Both are invisible until minutes in — the flip is only
 * needed at DISPATCH, so acts 1-2 film clean and 3.11 dies; a multi-node kind
 * survives all the way to run B's mount. The beforeAll preflight below turns
 * both into a refusal in the first seconds, before a frame is shot.
 *
 * NO SWEEP, and that is deliberate: 04c does not sweep either (the cluster is
 * one take old), and sweepStaleState() would deny the member's undecided
 * example.com approval and kill 04c's still-live run — state 04d consumes
 * knowingly and never films. What this episode LEAVES: the drive `notebook`,
 * one user allocation, PVC wardyn-drive-notebook-member Bound and holding
 * notes.txt, run A `keep a note` KILLED on camera, run B `read it back`
 * probably still RUNNING, and ZERO new approval rows.
 *
 * NOTHING DIALS OUT, on purpose. 04c left Egress hosts ENFORCED and this member
 * holds no grant, so any network beat would hijack the episode with an approval
 * it cannot resolve. The grader asserts that NEGATIVE — no approval.* row on
 * either run — rather than a decision.
 *
 * Canon: every drive string is imported from ui/src's frozen copy modules, the
 * way ui/e2e/drives.spec.ts does. A copy change must break this take rather
 * than let the film drift away from docs/design/user-drives-prompt.md §7.
 */

import { execFileSync } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";

import { test, expect } from "@playwright/test";
import { GOVERNANCE as GOV } from "../../src/app/lib/governance-copy";
import { DRIVES, DRIVE_MEMBER, PEOPLE, PERM } from "../../src/app/lib/user-drives-copy";
import { driveModeWord, driveSizeLabel } from "../../src/app/lib/user-drives-display";
import { MEMBER_GETTING_STARTED as GS } from "../../src/app/components/wardyn/copy";
import {
  act,
  beat,
  caption,
  centerInFrame,
  chapter,
  PACE,
  spotlight,
  typeInTerminal,
} from "./overlay";
import { BEAT_SHORT, pollScreen, silentCard } from "./demos";
import { bootRun, RUN_OVER } from "./runs";
import { dexSignIn, dexSignOut } from "./sso";
// stage.ts is the rig, and importing it is also why 04d gets a FRESH,
// SIGNED-OUT context: 04c's session does not carry over, so this episode signs
// itself in on camera.
import { stage } from "./stage";

test.skip(!process.env.WARDYN_DEMO, "demo recording — run via `make record-demo` (exports WARDYN_DEMO=1)");

test.describe.configure({ mode: "serial" });

// ---------------------------------------------------------------------------
// This episode's nouns (04d's own — no collision with 04c or 12b)
// ---------------------------------------------------------------------------

const ADMIN = "admin@wardyn.local";
/** 04c's own member literal. */
const MEMBER = "member@wardyn.local";

/** The cluster this episode is shot on. Same names and same defaults the
 *  grader uses (scripts/lib/verify-demo-take-04d.sh), so an operator who
 *  overrode one for the take does not have to remember a second spelling. */
const CLUSTER_CONTEXT = process.env.WARDYN_V04D_CONTEXT || "kind-wardyn-quickstart";
const CLUSTER_NAMESPACE = process.env.WARDYN_V04D_NAMESPACE || "wardyn";

/** The chart flip, verbatim from this file's header — printed by the preflight
 *  so a refusal hands back the command that fixes it rather than a diagnosis. */
const CHART_FLIP =
  `helm --kube-context ${CLUSTER_CONTEXT} upgrade wardyn deploy/helm/wardyn ` +
  `-n ${CLUSTER_NAMESPACE} --reuse-values --set userDrives.enabled=true ` +
  `&& kubectl --context ${CLUSTER_CONTEXT} -n ${CLUSTER_NAMESPACE} rollout status deploy/wardyn`;

/** A single lowercase word, so the PVC slug is unambiguous and TTS reads it
 *  cleanly. Collides with nothing in 00 / 04c / 12b. */
const DRIVE_NAME = "notebook";

/** 1024 MiB, which driveSizeLabel spells "1 GiB" — the GIB arm. k8s_pvc
 *  requires > 0 (a claim cannot request zero). */
const SIZE_MIB = "1024";
const SIZE = driveSizeLabel(Number(SIZE_MIB))!;

/**
 * The object the preview prints, run A's audit row names, and the close's
 * reclaim command reads.
 *
 * types.DriveObjectName: k8s_pvc -> `wardyn-drive-<drive-slug>-<home>`, and
 * with HOME_EMAIL_LOCAL the home is the part of the member's address before the
 * "@". That determinism is why this episode uses email_local and not the
 * derived id: `hash` computes the home from the winning subject, and
 * capabilitySubjects ranks `sub` above `email` — so an admin who pastes only an
 * email into the preview would be shown a name the member's real run does not
 * create (the script's Q1, raised as a P1 finding against resolveUserDrive).
 */
const HOME = MEMBER.split("@")[0];
const OBJECT_NAME = `wardyn-drive-${DRIVE_NAME}-${HOME}`;

/** The two runs. Distinct from 04c/12b's `reach for the outside world`. */
const RUN_A_TITLE = "keep a note";
const RUN_B_TITLE = "read it back";

/** The file that must outlive run A's sandbox. Never spoken — on screen only. */
const DRIVE_FILE = "/home/agent/drive/notes.txt";
const SENTINEL = "run one was here";

/**
 * The CONTROL file: written in run A OUTSIDE the drive, on the run's own
 * ephemeral scratch (runs_create.go's composerWorkspaceTarget, surfaced as
 * WARDYN_EPHEMERAL_DIRS). Run B must NOT find it — that is what makes
 * "brand-new sandbox" something the camera proved rather than said.
 * REHEARSAL: if /home/agent/work is not writable in Terminal mode, fall both
 * commands and the handoff's `control` back to /tmp/scratch.txt.
 */
const CONTROL_FILE = "/home/agent/work/scratch.txt";

/** The audit action whose row 3.16 rings. A constant, not a quoted literal:
 *  it is a Go action id and lives nowhere in ui/src, so a literal here would
 *  fail scripts/demo-rerecord-impact.py's label gate for the right string in
 *  the wrong place. */
const DRIVE_MOUNT_ACTION = "run.drive.mount";

// The commands. `printenv WARDYN_USER_DRIVE` is the cheapest possible receipt
// that WARDYN — not the shell — decided the mode: applyUserDriveEnv writes
// "<target>:<rw|ro>" and nothing else, and only when a drive is mounted.
// REHEARSAL: if the k8s lane does not set it, fall back to
// `mount | grep /home/agent/drive` and read the ro/rw option (noisier on
// camera, same fact).
const ENVCHECK = "printenv WARDYN_USER_DRIVE";
const WRITE = `echo '${SENTINEL}' > ${DRIVE_FILE} && ls -l /home/agent/drive`;
const CONTROL = `echo 'scratch' > ${CONTROL_FILE} && ls /home/agent/work`;
const CONTROLCHECK = `cat ${CONTROL_FILE}`;
const READBACK = `cat ${DRIVE_FILE}`;
const REFUSE_WRITE = `echo 'run two was here' >> ${DRIVE_FILE}`;

// The member's floor. 04c:127-137 verbatim: Minimal ships a CC2 floor and this
// demo cluster's one barrier is the Fence, so the one-line edit is itself the
// lesson — the floor is the member's to RAISE, never to sneak under the
// admin's ceiling.
const MEMBER_SPEC = JSON.stringify(
  {
    allowed_domains: ["api.anthropic.com"],
    first_use_approval: "deny_with_review",
    min_confinement_class: "CC1",
    auto_stop_after_sec: 3600,
    eligible_grants: [],
  },
  null,
  2,
);

// Captured on camera, handed to the grader.
let runA = "";
let runB = "";

/**
 * The grader's handoff (V13's v13-run-id.txt precedent).
 *
 * scripts/lib/verify-demo-take-04d.sh has no other way to tell this episode's
 * two runs apart from 04c's and 12b's on the same cluster, and "the newest two"
 * is wrong the moment a rehearsal runs the same evening. NEVER fails a take:
 * bookkeeping for the grader is not a beat.
 */
function writeHandoff(): void {
  const dir = process.env.WARDYN_DEMO_WORK_DIR;
  if (!dir) {
    console.warn("V04d: WARDYN_DEMO_WORK_DIR unset — the grader gets no run map");
    return;
  }
  try {
    mkdirSync(dir, { recursive: true });
    writeFileSync(
      path.join(dir, "04d-runs.json"),
      `${JSON.stringify(
        { write: runA, read: runB, sentinel: SENTINEL, control: CONTROL_FILE, object: OBJECT_NAME },
        null,
        2,
      )}\n`,
    );
  } catch (e) {
    console.warn(`V04d: could not write the run handoff: ${String(e)}`);
  }
}

/** The run id out of the cockpit URL the boot landed on. */
function runIdFrom(url: string): string {
  return url.match(/\/runs\/([0-9a-f-]{8,})/i)?.[1] ?? "";
}

// ---------------------------------------------------------------------------
// The preflight — the two staged preconditions, asserted instead of documented
// ---------------------------------------------------------------------------

/**
 * One `kubectl` read against the take's cluster, trimmed — or "" if kubectl is
 * not on PATH, the context is unknown, or the read is refused.
 *
 * NEVER throws. Whether a blank answer is fatal is the preflight's decision,
 * arm by arm, and two of the three arms below treat it differently.
 *
 * The cluster is read the same way the grader reads it
 * (scripts/lib/verify-demo-take-04d.sh's own kubectl/--context calls): nothing
 * the browser can reach exposes a node list or a Role, so the page context is
 * not an option for either.
 */
function kubectlOut(...args: string[]): string {
  try {
    return execFileSync("kubectl", ["--context", CLUSTER_CONTEXT, ...args], {
      encoding: "utf8",
      timeout: 30_000,
      stdio: ["ignore", "pipe", "ignore"],
    }).trim();
  } catch {
    return "";
  }
}

/** The cluster's admin bearer, out of the install's own Secret — the same
 *  source and the same fallbacks the grader uses. "" when none is reachable. */
function clusterAdminToken(): string {
  const env = process.env.WARDYN_ADMIN_TOKEN || process.env.WARDYN_DEMO_TOKEN;
  if (env) return env;
  const b64 = kubectlOut("-n", CLUSTER_NAMESPACE, "get", "secret", "wardyn-auth", "-o", "jsonpath={.data.admin-token}");
  return b64 ? Buffer.from(b64, "base64").toString("utf8").trim() : "";
}

/**
 * Refuse the take NOW if either of the two expensive preconditions is missing.
 *
 * Both were in the header and in nothing else, and both fail LATE: the chart
 * flip is only consulted at dispatch, so acts 1-2 film clean and beat 3.11
 * dies six minutes in as a REFUSED_BACKEND; a multi-node kind survives further
 * still, all the way to run B's mount, where ReadWriteOnce declines to
 * reattach and the read-back — which IS this episode — has nothing to show.
 *
 * Registered AFTER stage.ts's own beforeAll (import order), so stage() is
 * assigned by the time this runs, and re-guarded on WARDYN_DEMO for the same
 * reason 00's hook is: a file-level test.skip must not be the only thing
 * standing between a stray `pnpm e2e` and someone's cluster.
 */
test.beforeAll(async () => {
  if (!process.env.WARDYN_DEMO) return;
  const token = clusterAdminToken();

  // 1 · THE DRIVES SURFACE ANSWERS. The cheapest proof that this is a build
  // with the drives API, reachable at the base URL the take is pointed at,
  // and that the caller can read it — which is act 1's very first screen. It
  // does NOT prove the chart flip (that is arm 3): GET /drives reads the
  // database, and the flip only widens the runner's Role in the cluster.
  const res = await stage().request.get("/api/v1/drives", {
    headers: token ? { Authorization: `Bearer ${token}` } : undefined,
  });
  expect(
    res.ok(),
    `GET /api/v1/drives answered ${res.status()} on ${process.env.WARDYN_DEMO_BASE_URL || "http://localhost:8080"}` +
      ` — act 1 opens on that screen. A 401/403 means no admin bearer reached it (export WARDYN_ADMIN_TOKEN, or let this` +
      ` spec read Secret wardyn-auth from ${CLUSTER_CONTEXT}/${CLUSTER_NAMESPACE}); a 404 means this is not a build with` +
      ` user drives; a connection error means the take is pointed at the wrong stack.`,
  ).toBe(true);

  // 2 · SINGLE-NODE (header §4). kind's default `standard` class is
  // rancher.io/local-path with WaitForFirstConsumer: the claim binds to
  // whichever node schedules run A's pod, and on a second node run B's
  // ReadWriteOnce mount never reattaches.
  //
  // Read from kubectl, because no page-reachable route lists nodes. When
  // kubectl cannot answer, the operator may attest with WARDYN_V04D_NODES —
  // and if neither is available this arm FAILS rather than passing on
  // silence, which is the whole bug it exists for.
  const nodes = kubectlOut("get", "nodes", "-o", "jsonpath={.items[*].metadata.name}");
  const nodeCount = nodes ? nodes.split(/\s+/).filter(Boolean).length : Number(process.env.WARDYN_V04D_NODES ?? NaN);
  expect(
    Number.isFinite(nodeCount) && nodeCount > 0,
    `could not count the nodes of ${CLUSTER_CONTEXT}: kubectl answered nothing and WARDYN_V04D_NODES is unset.` +
      ` Run \`kubectl --context ${CLUSTER_CONTEXT} get nodes\` and set WARDYN_V04D_NODES to the count — a multi-node` +
      ` cluster loses run B's read-back, and that beat is the episode.`,
  ).toBe(true);
  expect(
    nodeCount,
    `${CLUSTER_CONTEXT} has ${nodeCount} nodes; 04d needs ONE. local-path binds the claim to the node that scheduled` +
      ` run A, so on a second node run B's ReadWriteOnce mount will not reattach and the read-back at 4.6 — the whole` +
      ` point of the episode — cannot film. Rebuild the cluster single-node before the take.`,
  ).toBe(1);

  // 3 · THE CHART FLIP (header §2). The flag is only consulted at DISPATCH, so
  // nothing before beat 3.11 notices it is off.
  //
  // Asserted on POSITIVE EVIDENCE OF ABSENCE only: if the namespace's Roles
  // read back at all and none of them carries persistentvolumeclaims, the flip
  // is off. A blank read (no kubectl, no permission, or RBAC provisioned
  // out-of-band with k8s.rbac.create=false) skips this arm rather than failing
  // a correct cluster on a read the spec was never entitled to.
  const roles = kubectlOut("-n", CLUSTER_NAMESPACE, "get", "roles", "-o", "jsonpath={.items[*].rules[*].resources}");
  if (roles) {
    expect(
      roles.includes("persistentvolumeclaims"),
      `no Role in ${CLUSTER_CONTEXT}/${CLUSTER_NAMESPACE} grants persistentvolumeclaims, so the runner cannot ask for` +
        ` a disk and beat 3.11 will die as a REFUSED_BACKEND six minutes into the take. Throw the chart flip first:\n` +
        `  ${CHART_FLIP}`,
    ).toBe(true);
  } else {
    console.warn(
      `V04d: could not read Roles in ${CLUSTER_NAMESPACE} — the chart flip is unverified. If 3.11 refuses, run:\n  ${CHART_FLIP}`,
    );
  }
});

/**
 * The member's terminal-run recipe, up to (but not including) the drive
 * checkbox: Title, Terminal, Minimal, the spec, Fence.
 *
 * `narrate` false is act 4's silent repeat — the choices were explained once,
 * at 3.4-3.5, and a second walk-through would be three captions the viewer has
 * already heard.
 */
async function memberRunForm(title: string, narrate: boolean): Promise<void> {
  const page = stage();
  await page.goto("/runs/new");
  await expect(page.getByRole("heading", { name: "New run", level: 1 })).toBeVisible({ timeout: 30_000 });
  await page.getByLabel("Title").fill(title);
  await act(
    page,
    page.getByRole("radio", { name: /^Terminal/ }),
    narrate ? "A plain command window. No AI agent, and no key to any AI service." : undefined, // C38
  );
  await act(
    page,
    page.getByRole("button", { name: "Minimal" }),
    narrate
      ? "Minimal policy — the spec box holds the rules this run starts with. You'll write your own in 'Your first policy'." // C39a
      : undefined,
  );
  await page.getByRole("textbox", { name: "Spec (JSON)" }).fill(MEMBER_SPEC);
  await act(
    page,
    page.getByRole("radio", { name: /^Fence/ }),
    narrate ? "And lowered to Fence — the one barrier this cluster has." : undefined, // C39b
  );
}

// ---------------------------------------------------------------------------
// Cold open + Act 1 — the admin registers a drive (C1-C22)
// ---------------------------------------------------------------------------

test("V04d act 1 — the admin registers a drive", async () => {
  test.setTimeout(600_000);
  const page = stage();
  await page.goto("/");
  await page.bringToFront();

  // Fail here rather than minutes into a silent, caption-less take.
  await expect
    .poll(() => page.evaluate(() => typeof (window as unknown as Record<string, unknown>).__demo), { timeout: 15_000 })
    .toBe("object");

  await chapter(page, "Your drive", "One directory that survives the sandbox");

  await dexSignIn(page, ADMIN);
  await page.waitForURL(/\/(runs|setup)/, { timeout: 60_000 });
  await caption(page, "You've seen who may do what. Now: what's theirs to keep."); // C1
  await beat(page, PACE.read);
  // The two-halves shape is ANNOUNCED before an admin fills forms under a card
  // that says "Your drive" — the adjudication's E04d-1.
  await caption(page, "First the admin sets it up; then its owner uses it."); // C1b
  await beat(page, PACE.read);

  await act(page, page.getByRole("link", { name: "Workspaces" }), "A workspace is what a run works on."); // C2
  await expect(page.getByRole("heading", { name: "Workspaces", level: 1 })).toBeVisible({ timeout: 30_000 });

  // /drives has NO nav item: the Workspaces header's outline button is the one
  // door from a nav-bearing screen (user-drives-prompt.md §6), so the episode
  // reaches it the way the product does.
  await act(page, page.getByRole("button", { name: DRIVES.TITLE, exact: true }), "This is what it keeps."); // C3
  await expect(page.getByRole("heading", { name: DRIVES.TITLE, level: 1 })).toBeVisible({ timeout: 30_000 });

  // C4 DELETED (§8, runtime): DRIVES.LEAD stays on screen under the title.
  const empty = page.getByText(DRIVES.EMPTY_TITLE);
  await expect(
    empty,
    "this cluster already has drives — beat 1.4 films the real empty state, so clear user_drives before the take",
  ).toBeVisible({ timeout: 30_000 });
  await spotlight(page, empty.locator(".."));
  await caption(
    page,
    "A run keeps nothing of its own between runs. A workspace you attach does; so does a drive. The screen says so.", // C5
  );
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, page.getByRole("button", { name: DRIVES.NEW_CTA, exact: true }), "Register one."); // C6
  const editor = page.getByTestId("drives-drive-editor");
  await expect(editor).toBeVisible({ timeout: 30_000 });

  const nameField = page.locator("#drive-name");
  await nameField.fill(DRIVE_NAME);
  await spotlight(page, editor.getByText(DRIVES.NAME_HINT));
  await caption(
    page,
    "A name — what the person sees when they choose to mount it. Inside the sandbox, the path never changes.", // C7
  );
  await beat(page, PACE.read);
  await spotlight(page, null);

  // The editor opens on this deployment's FIRST offered backend, which on a k8s
  // runner is already k8s_pvc (backendsFor) — the click is the beat, not a
  // state change. C8 names the chart flip's EFFECT; the flag itself stays a
  // staged precondition (see the header).
  await act(
    page,
    editor.getByRole("button", { name: DRIVES.BACKEND_K8S_PVC }),
    "Backend — where the storage comes from. This cluster's chart lets Wardyn ask it for disks: Wardyn-managed volume.", // C8
  );
  // C9 DELETED (§8, trim list): BACKEND_HINT stays on screen.

  // LEFT EMPTY, and that is the beat: empty means the cluster's default class,
  // which on kind is `standard` (rancher.io/local-path) — a provisioner that
  // does NOT bind the request, which is exactly what C18b and the frozen C20
  // say out loud.
  const storageClass = page.locator("#drive-storage-class");
  await expect(storageClass, "the storage-class field is missing — this drive is not on the k8s_pvc backend").toHaveValue(
    "",
  );
  await spotlight(page, editor.getByText(DRIVES.STORAGE_CLASS_HINT));
  await caption(page, "Storage class — which kind of disk the cluster makes. Empty means its default."); // C10
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, editor.getByRole("button", { name: DRIVES.HOME_EMAIL_LOCAL }));
  await spotlight(page, editor.getByText(DRIVES.HOME_EMAIL_LOCAL_HINT));
  await caption(page, "Every person gets their own directory in it. Name it from their email."); // C11
  await beat(page, PACE.read);
  await spotlight(page, null);
  // C12 DELETED (§8, trim list): HOME_RULE renders under the field for every
  // option, so the fail-closed promise stays on screen, unspoken and unfilmed.

  await page.locator("#drive-size").fill(SIZE_MIB);
  // SIZE_HINT_REQUIRED replaces SIZE_HINT on k8s_pvc — a claim cannot request
  // zero, and there is no required-marker glyph, so the hint carries the word.
  await spotlight(page, editor.getByText(DRIVES.SIZE_HINT_REQUIRED));
  await caption(page, "This cluster wants a number — how much space to ask for."); // C13
  await beat(page, PACE.read);
  await spotlight(page, null);

  // BOTH halves of the writability lesson, split by the adjudication (E04d-10)
  // so the default is stated over the still-unchecked box and the click is
  // narrated AS a choice — the old single line landed "writes are off by
  // default" on the click that turned them on.
  const writableSwitch = editor.getByRole("switch", { name: DRIVES.FIELD_WRITABLE });
  await expect(writableSwitch, "Writable must start OFF — WRITABLE_HINT opens on that promise").toHaveAttribute(
    "aria-checked",
    "false",
  );
  await spotlight(page, writableSwitch);
  await caption(page, "Writes are off by default."); // C14a
  await beat(page, BEAT_SHORT);
  await act(page, writableSwitch, "This one we allow — the proof ahead needs a write."); // C14b
  await expect(writableSwitch).toHaveAttribute("aria-checked", "true");

  await spotlight(page, editor.getByText(DRIVES.WRITABLE_HINT));
  await caption(page, "A writable drive is where a run's changes persist — and what a compromised run could alter."); // C15a
  await beat(page, PACE.read);
  // The residual, spoken as a limit and not dialled: the drive is loot whatever
  // its mode, and the egress door is the wall (DESIGN.md §6.1 residual 32).
  // 04d promises no control over it and films none.
  await caption(
    page,
    "Read-only or not, a run can carry the drive out through any host it may reach. The egress door keeps it in — not this switch.", // C15b
  );
  await beat(page, PACE.read);
  await spotlight(page, null);

  // "Keep their directory" is already the editor's default; the click is the
  // BEAT — an admin stating an intent — not a state change.
  await act(
    page,
    editor.getByRole("radio", { name: DRIVES.RECLAIM_RETAIN }),
    "And what you intend when someone leaves is recorded here.", // C16
  );
  await spotlight(page, editor.getByText(DRIVES.RECLAIM_HINT));
  await beat(page, BEAT_SHORT);
  await spotlight(page, null);

  await act(page, page.getByRole("button", { name: DRIVES.SAVE_CTA, exact: true }), "Save it."); // C17
  await expect(editor).toBeHidden({ timeout: 30_000 });

  // The registry's first row. "Allocated to nobody" is the state the next act
  // fills — the captions' own voice says "row"/"who gets it" and reserves the
  // word "allocation" for the frozen C22 (E04d-12).
  const drivesTable = page.getByRole("table").first();
  const driveRow = drivesTable.getByRole("row", { name: new RegExp(DRIVE_NAME) });
  await expect(driveRow).toBeVisible({ timeout: 30_000 });
  await expect(driveRow.getByText(DRIVES.KIND_MANAGED)).toBeVisible();
  await expect(driveRow.getByText(SIZE)).toBeVisible();
  await expect(driveRow.getByText(DRIVES.ENFORCEMENT_REQUEST)).toBeVisible();
  await spotlight(page, driveRow.getByText(DRIVES.ALLOCATED_NONE));
  await caption(page, "One drive; nobody has it yet."); // C18
  await beat(page, PACE.read);
  await spotlight(page, null);

  // THE HONESTY BLOCK. C19-C22 are DRIVES.HONESTY's four sentences, spoken
  // VERBATIM — the round refused every rewrite of them and the grader greps for
  // the first one. C18b is the plain-words lead-in the round added so the quote
  // lands on an audience that has been told what "request" means.
  //
  // If P5 ever renders the note as four sentence spans the spotlight can step
  // through them; today it is one element, and the four beats read the same.
  const honesty = page.getByText(DRIVES.HONESTY);
  await expect(honesty, "the honesty note is not on screen — the size claim cannot be made without it").toBeVisible({
    timeout: 30_000,
  });
  await centerInFrame(honesty);
  await spotlight(page, honesty);
  await caption(
    page,
    "That number, in plain words: it is a request. Whether the cluster holds the drive to it depends on the storage behind it — here, it does not. The note says exactly that.", // C18b
  );
  await beat(page, PACE.read);
  await caption(page, "Wardyn never enforces a drive's size itself."); // C19 — FROZEN
  await beat(page, PACE.read);
  await caption(
    page,
    "On Kubernetes the size is the volume request and the storage class decides whether it binds — block disks do, network-share provisioners do not.", // C20 — FROZEN
  );
  await beat(page, PACE.read);
  await caption(page, "On Docker a managed drive has no byte cap, the same gap disk_mib has."); // C21 — FROZEN
  await beat(page, PACE.read);
  await caption(page, "A share is bounded by its own quota. The size you see is the allocation, not a guarantee."); // C22 — FROZEN
  await beat(page, PACE.read);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Act 2 — the allocation, and the preview offboarding needs (C23-C34)
// ---------------------------------------------------------------------------

test("V04d act 2 — who gets it, and what they would get", async () => {
  test.setTimeout(300_000);
  const page = stage();

  const allocLead = page.getByText(DRIVES.ALLOC_LEAD);
  await centerInFrame(allocLead);
  await spotlight(page, allocLead);
  // The precedence rule stays ON SCREEN in ALLOC_LEAD and is not spoken: one
  // row is all this take builds, so a rule about three tiers would be a claim
  // the camera never checks (E04d-15).
  await caption(page, "Now: who gets it. One person — one row."); // C23
  await beat(page, PACE.read);
  await spotlight(page, null);

  // A person's own row — the only tier that may name a directory, and the tier
  // whose home the preview and the run both derive from the same claim.
  await act(page, page.getByRole("button", { name: PERM.SUBJECT_USER, exact: true }));
  await page.getByRole("textbox", { name: PERM.FIELD_WHO, exact: true }).fill(MEMBER);
  await spotlight(page, page.getByText(PERM.HINT_USER));
  // "subject" is never spoken; its meaning is (E04d-16).
  await caption(
    page,
    "One person — matched by the email their sign-in sends, or the sign-in's own ID for them. Either works.", // C24
  );
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, page.locator("#drive-allocation-drive"), "The drive."); // C25
  await act(page, page.getByRole("option", { name: DRIVE_NAME, exact: true }));

  // Size override, writable override and Enabled are all LEFT ALONE. The
  // hint's own "never widen" clause is said ONCE, at C57, where the proof is.
  await spotlight(page, page.getByText(DRIVES.WRITABLE_OVERRIDE_HINT));
  await caption(page, "Everything else inherits the drive's settings."); // C26
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, page.getByRole("button", { name: DRIVES.ADD_CTA, exact: true }), "Allocate."); // C27
  // C28 DELETED (§8, trim list): the row is on screen.
  const allocationsTable = page.getByRole("table").nth(1);
  await expect(allocationsTable.getByRole("row", { name: new RegExp(MEMBER) })).toBeVisible({ timeout: 30_000 });
  await expect(page.getByText(DRIVES.ALLOCATED_COUNT(1), { exact: true })).toBeVisible({ timeout: 30_000 });

  // EFFECT_NOTE and SIGNIN_NOTE render together; only the first is spoken —
  // no group row exists in this take, so the group clause stays on screen
  // (E04d-19).
  await spotlight(page, page.getByText(DRIVES.EFFECT_NOTE));
  await caption(page, "It takes effect on their next run."); // C29
  await beat(page, PACE.read);
  await spotlight(page, null);

  // THE PREVIEW asks the SERVER — the identical resolveUserDriveFor the launch
  // path takes — so the tier and the object name in its answer could only have
  // come from the code the member's own run will run.
  await page.locator("#drive-preview-claims").fill(MEMBER);
  await act(page, page.getByRole("button", { name: DRIVES.PREVIEW_CTA, exact: true }), "Before they launch: ask what this person would get."); // C30

  const result = page.getByTestId("drives-preview-result");
  await expect(result).toBeVisible({ timeout: 30_000 });
  const tierLine = page.getByText(DRIVES.PREVIEW_RESULT(DRIVE_NAME, DRIVES.PREVIEW_TIER_USER));
  await expect(tierLine).toBeVisible();
  await spotlight(page, tierLine);
  await caption(page, "Their own row — worked out exactly the way their run will work it out."); // C31
  await beat(page, PACE.read);

  // THE STRING BEATS 3.16 AND 5.4 COME BACK FOR. This is the only surface in
  // the product that prints the object name, and 3.16 is where the run's own
  // record is shown to name the same one.
  await expect(result).toContainText(HOME);
  await expect(
    result,
    `the preview did not print ${OBJECT_NAME} — the receipt at 3.16 and the reclaim command at 5.6 both quote it`,
  ).toContainText(OBJECT_NAME);
  await centerInFrame(result);
  await spotlight(page, result);
  await caption(
    page,
    "Their directory — and the volume this makes on the cluster, by name. Their first run's record will show the same.", // C32
  );
  await beat(page, PACE.read);
  // C33 DELETED (§8, runtime): ENFORCEMENT_REQUEST is still in the preview's dl.
  await spotlight(page, null);

  await spotlight(page, page.getByText(GOV.PREVIEW_NOT_SAVED));
  await caption(page, "Nothing here is saved. It only answers."); // C34
  await beat(page, PACE.read);
  await spotlight(page, null);
});

// ---------------------------------------------------------------------------
// Act 3 — the member's first run: write, receipt, kill (C35-C47e)
// ---------------------------------------------------------------------------

test("V04d act 3 — the member writes a note, and ends the run", async () => {
  test.setTimeout(900_000);
  const page = stage();

  // Sign out ON CAMERA — the role flip is part of the film (04c's shape), not
  // silent plumbing (12b's).
  await dexSignOut(page, "Now the person it belongs to."); // C35
  await page.goto("/");
  await dexSignIn(page, MEMBER);
  await page.waitForURL(/\/(runs|setup)/, { timeout: 60_000 });

  await expect(page.getByText(GS.SUBTITLE)).toBeVisible({ timeout: 30_000 });
  // The chip beside the barrier and the sign-in: which drive is allocated to
  // you is a fact about this deployment, in BARRIER_CHIP's own `Label · value`
  // shape.
  const driveChip = page.getByText(DRIVE_MEMBER.GS_DRIVE_CHIP(DRIVE_NAME, SIZE, driveModeWord(true)));
  await expect(
    driveChip,
    "the member's Getting Started shows no drive chip — /me.user_drive resolved to null for this session",
  ).toBeVisible({ timeout: 30_000 });
  await spotlight(page, driveChip);
  await caption(page, "Their Getting Started names it, beside the barrier and the sign-in."); // C36
  await beat(page, PACE.read);
  await spotlight(page, null);

  // The one place a drive and a workspace get conflated — so the sentence that
  // separates them renders inside the workspace card itself.
  const driveBody = page.getByText(DRIVE_MEMBER.GS_DRIVE_BODY);
  await centerInFrame(driveBody);
  await spotlight(page, driveBody);
  await caption(page, "And it says what people get wrong: a drive is not a workspace."); // C37
  await beat(page, PACE.read);
  await spotlight(page, null);

  await memberRunForm(RUN_A_TITLE, true); // C38, C39a, C39b
  // C40 DELETED (§8, trim list): the workspace Select stays visibly on
  // "Ephemeral scratch — no repo" throughout — orthogonality is on screen, not
  // narrated.

  const driveBlock = page.getByTestId("nr-drive");
  await expect(
    driveBlock,
    "the New Run card offers no drive checkbox — this member's allocation did not resolve",
  ).toBeVisible({ timeout: 30_000 });
  await centerInFrame(driveBlock);
  await spotlight(page, driveBlock.getByText(DRIVE_MEMBER.NR_CHECKBOX));
  await caption(page, "Mount my drive."); // C41 — verbatim NR_CHECKBOX
  await beat(page, BEAT_SHORT);

  const mount = page.locator("#nr-drive-mount");
  await expect(mount, "the drive checkbox must start unchecked — mounting is the person's choice").not.toBeChecked();
  await act(page, mount);
  await expect(mount).toBeChecked();
  // The path IS in the hint, under the checkbox: NR_HINT is
  // `"{name}" at /home/agent/drive — {size}, {mode}.`
  //
  // REHEARSAL PIN — ONE RING, NOT TWO. NR_HINT and NR_RW_NOTE are two
  // sentences inside ONE <p> (workspace-card.tsx:119-126), so `hint` and
  // `rwNote` below resolve to the SAME element and C42 and C43 spotlight an
  // identical rectangle. The beat table reads as two rings; the film has one,
  // held across both lines. That is correct and expected — do not chase it as
  // a missed ring in the first take's review, and do not "fix" it by ringing
  // the whole nr-drive block for one of them (a wider ring on the second beat
  // reads as the spotlight losing its place). Splitting the copy into two
  // elements is a ui/src change and is deliberately NOT made here: the mock
  // round owns it, as a 0.7.1 item.
  const hint = driveBlock.getByText(DRIVE_MEMBER.NR_HINT(DRIVE_NAME, SIZE, DRIVES.MODE_RW_INLINE));
  await expect(hint).toBeVisible();
  await spotlight(page, hint);
  await caption(page, "The name, the path it lands on, the size, and the mode."); // C42
  await beat(page, PACE.read);

  const rwNote = driveBlock.getByText(DRIVE_MEMBER.NR_RW_NOTE);
  await expect(rwNote).toBeVisible();
  await spotlight(page, rwNote);
  await caption(page, "What a run writes there persists to your next run."); // C43 — verbatim NR_RW_NOTE
  await beat(page, PACE.read);
  await spotlight(page, null);
  // C44 DELETED (§8, trim list): NR_READONLY_TOGGLE is on screen under the
  // checkbox, untouched — 4.2 is where it is explained and used.

  // SILENT launch and a silent boot: nothing may be spoken inside a
  // fast-forward span, and a k8s pod pull is long enough to need one.
  await act(page, page.getByRole("button", { name: "Launch run" }));
  runA = runIdFrom(await bootRun(page, "run A never came up as an attached terminal"));
  writeHandoff();

  const screen = page.locator(".xterm-screen").first();
  await typeInTerminal(page, ENVCHECK);
  await pollScreen(screen, /\/home\/agent\/drive:rw/, "the run was not told it holds a writable drive");
  await caption(page, "Ask the run where the drive landed: the path, and read-write."); // C45
  await beat(page, PACE.read);

  await caption(page, "Write a file into it."); // C46
  await beat(page, BEAT_SHORT);
  await typeInTerminal(page, WRITE);
  await pollScreen(screen, /notes\.txt/, "the write into the drive never landed");
  await spotlight(page, screen);
  // The PRIMARY rewrite of the round (§8): "agent" is disambiguated in the line
  // itself, and isolation is stated as the MECHANISM — a volume per person —
  // rather than as a feeling read off an `ls` where every sandbox is uid 1000.
  await caption(
    page,
    "One file, owned by 'agent' — the sandbox's account, not an AI. Nothing else: another person's run gets its own volume, not a folder in this one.", // C47
  );
  await beat(page, PACE.read);
  await spotlight(page, null);

  // THE CONTROL FILE — outside the drive, on the run's own ephemeral scratch.
  // Run B not finding it is what makes "brand-new sandbox" a thing the camera
  // proved (4.5b).
  await typeInTerminal(page, CONTROL);
  await pollScreen(screen, /scratch\.txt/, `${CONTROL_FILE} is not writable — fall back to /tmp/scratch.txt`);
  await caption(page, "And one more file, outside the drive, on the sandbox's own scratch disk."); // C47b
  await beat(page, PACE.read);

  // THE RECEIPT. The run page's Audit tab renders a row as time · actor ·
  // action · target and NOTHING from data (AuditTab), so the target field is
  // the only way the object name reaches a screen from inside a run — which is
  // why auditDriveMount spends it on the storage rather than on the run id.
  // A member reads their OWN run's rows (auditScope lets the creator through
  // GET /audit?run_id=), so this films from the member's seat.
  await act(page, page.getByRole("tab", { name: "Audit" }));
  const mountRow = page.getByText(DRIVE_MOUNT_ACTION, { exact: true }).first().locator("..");
  await expect(
    mountRow,
    "no run.drive.mount row on run A's Audit tab — dispatch attached no drive",
  ).toBeVisible({ timeout: 60_000 });
  await expect(
    mountRow,
    `the run.drive.mount row does not name ${OBJECT_NAME} — C47c would narrate a name the screen never showed`,
  ).toContainText(OBJECT_NAME);
  await centerInFrame(mountRow);
  await spotlight(page, mountRow);
  await caption(page, "The run's own record names the volume it was handed — the name the preview printed."); // C47c
  await beat(page, PACE.read);
  await spotlight(page, null);

  // KILL RUN A ON CAMERA — 00's beat 3.12 shape and 00's C46 words, so the
  // series ends a run one way. The dialog's own body says what the kill costs,
  // so the softer verb is not a euphemism the viewer cannot check. A member may
  // kill their own run (OPERATIONS.md).
  await act(page, page.getByRole("button", { name: "Kill", exact: true }), "End the run."); // C47d
  const kill = page.getByRole("alertdialog");
  await expect(kill).toBeVisible({ timeout: 15_000 });
  await expect(kill).toContainText("tears down the sandbox");
  await act(page, kill.getByRole("button", { name: "Kill run" }), "The sandbox goes with it — and its own disk."); // C47e
  await expect(
    page.getByText(RUN_OVER).first(),
    "run A never went terminal — run B would then share one claim with a live pod, which is not what C48 says",
  ).toBeVisible({ timeout: 120_000 });
  // The PVC carries no run label, so teardown's DeleteCollection by run id
  // never sees it — the claim survives this kill, which is the whole episode.
  // Asserted where it is checkable: scripts/lib/verify-demo-take-04d.sh arm 7.
});

// ---------------------------------------------------------------------------
// Act 4 — the second run: read it back, narrowed (C48-C57)
// ---------------------------------------------------------------------------

test("V04d act 4 — a brand-new sandbox, and the same directory", async () => {
  test.setTimeout(900_000);
  const page = stage();

  await memberRunForm(RUN_B_TITLE, false);
  await caption(page, "A second run, in a brand-new sandbox."); // C48
  await beat(page, PACE.read);

  const driveBlock = page.getByTestId("nr-drive");
  await centerInFrame(driveBlock);
  await act(page, page.locator("#nr-drive-mount"));
  // NR_READONLY_TOGGLE renders only for a WRITABLE allocation and only
  // narrows — there is no control here that widens anything, which is exactly
  // what C57 says and all it says.
  await act(
    page,
    page.locator("#nr-drive-readonly"),
    "Mount it again — read-only this time, so a run you don't fully trust can read the notes, never touch them.", // C49
  );

  // The hint's {mode} and the sentence under it flip TOGETHER with the toggle,
  // so neither promises persistence a read-only mount cannot give — literally
  // together: they are one <p> (workspace-card.tsx:119-126).
  //
  // REHEARSAL PIN — ONE RING, NOT TWO, the C42/C43 note again. C49's ring
  // sits on the read-only toggle (act()'s own), and C50 rings the paragraph;
  // but a reviewer reading the beat table for act 4 should expect the hint and
  // the read-only sentence to light up as a single rectangle, exactly as they
  // did at C42/C43.
  const roHint = driveBlock.getByText(DRIVE_MEMBER.NR_HINT(DRIVE_NAME, SIZE, DRIVES.MODE_RO_INLINE));
  await expect(roHint).toBeVisible();
  const roNote = driveBlock.getByText(DRIVE_MEMBER.NR_RO_NOTE);
  await expect(roNote).toBeVisible();
  await expect(driveBlock.getByText(DRIVE_MEMBER.NR_RW_NOTE)).toHaveCount(0);
  await spotlight(page, roNote);
  await caption(page, "A run can read it and never change it."); // C50 — verbatim NR_RO_NOTE
  await beat(page, PACE.read);
  await spotlight(page, null);

  await act(page, page.getByRole("button", { name: "Launch run" }));
  runB = runIdFrom(await bootRun(page, "run B never came up as an attached terminal"));
  writeHandoff();

  const screen = page.locator(".xterm-screen").first();
  await typeInTerminal(page, ENVCHECK);
  await pollScreen(screen, /\/home\/agent\/drive:ro/, "run B's drive was not narrowed to read-only at the mount");
  await caption(page, "Same drive. This time, read-only."); // C51
  await beat(page, PACE.read);

  // NEW BOX, PROVED: the control file died with run A's pod. This lands BEFORE
  // the read-back, so "survives the sandbox" is measured against a sandbox the
  // viewer watched end.
  await typeInTerminal(page, CONTROLCHECK);
  await pollScreen(
    screen,
    /No such file/,
    "the control file survived into run B — this is not a new sandbox, and C48 is false",
  );
  await caption(page, "The scratch file? Gone with its sandbox."); // C51b
  await beat(page, PACE.read);

  // PERSISTENCE PROVED — past a teardown, not beside a still-running sandbox.
  await typeInTerminal(page, READBACK);
  await pollScreen(screen, new RegExp(SENTINEL), "the note run A wrote did not survive into run B");
  await spotlight(page, screen);
  await caption(page, "And the file the last run left."); // C52
  await beat(page, PACE.read);
  await caption(page, "New sandbox, new container, same directory. That is the whole feature."); // C53
  await beat(page, PACE.read);
  await spotlight(page, null);

  await typeInTerminal(page, REFUSE_WRITE);
  // REHEARSAL: bash prints "Read-only file system"; dash's wording differs.
  // Pin the regex to whatever this image's shell actually prints.
  await pollScreen(screen, /Read-only file system/, "the write into a read-only mount was NOT refused by the filesystem");
  await caption(page, "Now try to change it."); // C54
  await beat(page, BEAT_SHORT);

  await spotlight(page, screen);
  // Narrowing proved AT THE ENFORCEMENT POINT — and what could undo it named:
  // nothing inside the box, only the person's next request (E04d-38).
  await caption(
    page,
    "Refused by the filesystem. The mount was read-only from launch; nothing inside the box can flip it — only the person's next request.", // C55
  );
  await beat(page, PACE.read);
  await spotlight(page, null);
  await caption(page, "The admin allowed writes; the person narrowed their own mount."); // C56
  await beat(page, PACE.read);
  // Said ONCE, and only what the FORM shows. The server's own widen refusal
  // (REFUSED_WRITABLE, for a forged read_only:false) is neither filmed nor
  // claimed nor promised — the console has no control that widens.
  await caption(
    page,
    "A request can narrow what it was given. Nothing on that form widens it — its one switch only takes writes away.", // C57
  );
  await beat(page, PACE.read);
});

// ---------------------------------------------------------------------------
// Act 5 — the close: when someone leaves (C58-C65)
// ---------------------------------------------------------------------------

test("V04d act 5 — when someone leaves", async () => {
  test.setTimeout(300_000);
  const page = stage();

  // Cheaper and more reliable than a second UI sign-out (12b's shape), and the
  // role flip has already been filmed once.
  await page.context().clearCookies();
  await page.goto("/");
  await dexSignIn(page, ADMIN);
  await page.waitForURL(/\/(runs|setup)/, { timeout: 60_000 });
  await page.goto("/drives");
  await expect(page.getByRole("heading", { name: DRIVES.TITLE, level: 1 })).toBeVisible({ timeout: 30_000 });
  await caption(page, "Back to the admin — and the one thing this release does not do for you."); // C58
  await beat(page, PACE.read);

  const driveRow = page.getByRole("table").first().getByRole("row", { name: new RegExp(DRIVE_NAME) });
  const reclaimCell = driveRow.getByText(DRIVES.RECLAIM_RETAIN);
  await centerInFrame(reclaimCell);
  await spotlight(page, reclaimCell);
  // "remembers", not "records": the heteronym leaves the soundtrack instead of
  // being pinned (E04d-40).
  await caption(page, "The drive remembers what should happen when that person leaves."); // C59
  await beat(page, PACE.read);
  await spotlight(page, null);

  // Nothing is removed on camera. The frozen RECLAIM_HINT is quoted AS the
  // screen's promise — C60 is that sentence with its attribution in front.
  await act(page, page.getByRole("button", { name: `${DRIVES.EDIT} ${DRIVE_NAME}`, exact: true }));
  const editor = page.getByTestId("drives-drive-editor");
  const reclaimHint = editor.getByText(DRIVES.RECLAIM_HINT);
  await centerInFrame(reclaimHint);
  await spotlight(page, reclaimHint);
  await caption(
    page,
    "The screen's words: recorded here, carried out by you — removing an allocation below never deletes data.", // C60
  );
  await beat(page, PACE.read);
  await spotlight(page, null);
  await act(page, editor.getByRole("button", { name: PEOPLE.CANCEL, exact: true }));

  // The preview again — the claims field is a fresh mount after the re-sign-in,
  // so it is re-filled off camera; the CLICK and its answer are the beat.
  await page.locator("#drive-preview-claims").fill(MEMBER);
  await act(page, page.getByRole("button", { name: DRIVES.PREVIEW_CTA, exact: true }));
  const result = page.getByTestId("drives-preview-result");
  await expect(result).toBeVisible({ timeout: 30_000 });
  await expect(result).toContainText(OBJECT_NAME);
  await centerInFrame(result);
  await spotlight(page, result);
  // "exactly" is anchored to the record shown at 3.16, not to the template.
  await caption(page, "Which is why the preview prints that name, exactly — the same one the run's record showed."); // C61
  await beat(page, PACE.read);

  await spotlight(page, result.getByText(DRIVES.PREVIEW_OBJECT_HINT));
  await caption(page, "What the reclaim command names — copy it when someone leaves."); // C62 — verbatim
  await beat(page, PACE.read);
  await spotlight(page, null);

  // The command is READ, never spoken: nothing in this episode says "pvc", and
  // no object name, hostname or filename reaches the soundtrack.
  await silentCard(page, `kubectl -n wardyn delete pvc ${OBJECT_NAME}`);

  // The only line that says what the command does — "volume claim" is the
  // canon's own member-facing word.
  await caption(page, "One cluster command, typed by you, on purpose — it removes the volume claim by that name, and their drive with it."); // C63
  await beat(page, PACE.read);
  // "cannot", with the mechanism in plain words: the runner Role holds `get`
  // and `create` on claims and no `delete` (DESIGN.md §3.3).
  await caption(page, "Wardyn cannot delete a person's data — it was never given permission to. It hands you the name and stands aside."); // C64
  await beat(page, PACE.read);
  // Owns the blind pick at C39a.
  await caption(
    page,
    "Next: your first policy — the rules a run inherits before it asks for anything. You picked one today; next time you write it.", // C65
  );
  await beat(page, PACE.read);
  await caption(page, "");
  await silentCard(page, "Next — 05: Your first policy");
});
