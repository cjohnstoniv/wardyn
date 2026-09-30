/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it, expect } from "vitest";
import { SIGNIN_PROGRESS } from "./settings/login-pane-copy";
import {
  isImagePullFailure,
  isTerminalStatusReason,
  RUN_PENDING_OVERDUE_MS,
  RUN_POLL_SLOW_START_MS,
  RUN_START_OVERDUE_MS,
  RUN_STARTUP,
  runStartupView,
  STATUS_REASON_BUILDING,
  type StartupLastStep,
  parseStatusDetail,
  statusDetailChip,
  statusDetailSentence,
  TERMINAL_STATUS_REASONS,
  CHIP_DOWNLOADING,
  CHIP_IMAGE_PULL_FAILED,
  CHIP_SETTING_UP,
  CHIP_CONTAINER_WONT_START,
  CHIP_WAITING_FOR_MACHINE,
  STARTING_CONTAINER_CREATING,
  STARTING_FIRST_PULL,
  STARTING_RAW_PREFIX,
  STARTING_UNSCHEDULABLE,
  STARTING_WAITING_FOR_NODE,
  STUCK_CRASH_LOOP,
  STUCK_CREATE_CONTAINER,
  STUCK_IMAGE_NAME,
  STUCK_IMAGE_PULL,
} from "./run-status-detail";

// 0.7.5 field report, finding 6: without this, every slow start looks
// identical — the run sits in STARTING and nothing says whether it is
// pulling an image, waiting to schedule, or stuck on a reference that would
// never resolve. The substrate's answer reaches the wire; this module is
// where it becomes a sentence.

describe("statusDetailSentence", () => {
  it("renders the TERMINAL image failure with the registry's own words", () => {
    const raw = "agent: ImagePullBackOff: rpc error: code = Unknown desc = pull access denied";
    const s = statusDetailSentence(raw, "ImagePullBackOff");
    expect(s).toContain(STUCK_IMAGE_PULL);
    // The message is the fix: without it the sentence names a problem and no
    // way to act on it.
    expect(s).toContain("pull access denied");
    expect(isTerminalStatusReason("ImagePullBackOff")).toBe(true);
  });

  it("a message containing colons survives whole", () => {
    // "rpc error: code = …" is the SHAPE the kubelet actually produces, so a
    // parser that split on every colon would truncate every real message.
    const raw = "agent: ErrImagePull: rpc error: code = NotFound desc = manifest unknown: manifest tagged v9 not found";
    expect(statusDetailSentence(raw, "ErrImagePull")).toContain(
      "rpc error: code = NotFound desc = manifest unknown: manifest tagged v9 not found",
    );
    expect(parseStatusDetail(raw, "ErrImagePull").component).toBe("agent");
  });

  it("renders the ordinary wait CONDITIONALLY and does not call it terminal", () => {
    expect(statusDetailSentence("agent: ContainerCreating", "ContainerCreating")).toBe(STARTING_CONTAINER_CREATING);
    // The hedge is the point (U-12): the kubelet says ContainerCreating for a
    // pull and for everything else it does before a container runs.
    expect(STARTING_CONTAINER_CREATING).toContain("can take");
    expect(isTerminalStatusReason("ContainerCreating")).toBe(false);
  });

  it("only DOCKER's own Pulling asserts a download outright", () => {
    expect(statusDetailSentence("image: Pulling: wardyn/agent-claude:0.7.6", "Pulling")).toBe(STARTING_FIRST_PULL);
    expect(isTerminalStatusReason("Pulling")).toBe(false);
  });

  it("names SCHEDULING for a wait that is not a pull", () => {
    const raw = "pod: Unschedulable: 0/1 nodes are available: 1 node(s) had untolerated taint";
    expect(statusDetailSentence(raw, "Unschedulable")).toBe(STARTING_UNSCHEDULABLE);
    expect(statusDetailSentence("pod: Pending", "Pending")).toBe(STARTING_WAITING_FOR_NODE);
  });

  it("renders the other three terminal reasons with their messages", () => {
    expect(statusDetailSentence("agent: InvalidImageName: couldn't parse image", "InvalidImageName")).toBe(
      `${STUCK_IMAGE_NAME} couldn't parse image`,
    );
    expect(
      statusDetailSentence("agent: CreateContainerConfigError: secret not found", "CreateContainerConfigError"),
    ).toBe(`${STUCK_CREATE_CONTAINER} secret not found`);
    expect(statusDetailSentence("agent: CrashLoopBackOff: back-off 5m0s", "CrashLoopBackOff")).toBe(
      `${STUCK_CRASH_LOOP} back-off 5m0s`,
    );
  });

  it("hands an UNKNOWN reason over as the platform's own words, prefixed", () => {
    const raw = "agent: SomeFutureReason: a thing happened";
    expect(statusDetailSentence(raw, "SomeFutureReason")).toBe(STARTING_RAW_PREFIX + raw);
    // …and never claims it is terminal, because nothing here knows that.
    expect(isTerminalStatusReason("SomeFutureReason")).toBe(false);
  });

  // A line that did not parse at all is NOT the "nothing has taken
  // the pod" fact. Saying "Waiting for a machine to start it on." over a string
  // nobody recognised invents a diagnosis, which is the whole defect this module
  // exists to end.
  it("hands an UNPARSEABLE line over raw rather than calling it a node wait", () => {
    expect(statusDetailSentence("something the substrate said")).toBe(
      STARTING_RAW_PREFIX + "something the substrate said",
    );
    // …while a line that DID parse — it named a component — and merely carried
    // no reason is that fact, and keeps the node-wait sentence.
    expect(parseStatusDetail("pod: : nothing has taken it").component).toBe("pod");
    expect(statusDetailSentence("pod: : nothing has taken it")).toBe(STARTING_WAITING_FOR_NODE);
  });

  // The server always sends the detail beside a terminal
  // reason, but a UI that returns "" for one is a blank sentence under a lead-in
  // that promises words. Defence in depth, both registers.
  it("never falls silent on a terminal reason, even with no detail at all", () => {
    expect(statusDetailSentence("", "ImagePullBackOff")).toBe(STARTING_RAW_PREFIX + "ImagePullBackOff");
    expect(statusDetailChip("", "ImagePullBackOff")).toBe(CHIP_IMAGE_PULL_FAILED);
    expect(statusDetailChip(null, "CrashLoopBackOff")).toBe(CHIP_CONTAINER_WONT_START);
    // A NON-terminal reason with no detail stays silent: nothing is wrong, and
    // there is nothing to say.
    expect(statusDetailSentence("", "ContainerCreating")).toBe("");
    expect(statusDetailChip("", "ContainerCreating")).toBe("");
  });

  it("says NOTHING when there is nothing to say", () => {
    expect(statusDetailSentence(undefined)).toBe("");
    expect(statusDetailSentence(null)).toBe("");
    expect(statusDetailSentence("")).toBe("");
    expect(statusDetailChip("")).toBe("");
  });

  it("parses the string when a pre-0.7.6 daemon sends no reason token", () => {
    expect(statusDetailSentence("agent: ImagePullBackOff: denied")).toBe(`${STUCK_IMAGE_PULL} denied`);
    expect(parseStatusDetail("agent: ImagePullBackOff: denied").reason).toBe("ImagePullBackOff");
  });

  it("prefers the SERVER's reason over its own parse", () => {
    // The server did the same split and is the authority; the console must not
    // become a second parser with a different opinion.
    expect(parseStatusDetail("agent: ImagePullBackOff: denied", "ErrImagePull").reason).toBe("ErrImagePull");
  });
});

// The header chip is max-w-[160px]. The sentences above truncate
// there to a restatement of the STARTING badge sitting right beside them, and
// the registry's words — the entire point of a terminal reason — never appear.
describe("statusDetailChip — the short register", () => {
  it("is short, and says what the STARTING badge does not", () => {
    expect(statusDetailChip("image: Pulling: ref", "Pulling")).toBe(CHIP_DOWNLOADING);
    expect(statusDetailChip("agent: ContainerCreating", "ContainerCreating")).toBe(CHIP_SETTING_UP);
    expect(statusDetailChip("pod: Unschedulable: taint", "Unschedulable")).toBe(CHIP_WAITING_FOR_MACHINE);
    expect(statusDetailChip("agent: ImagePullBackOff: denied", "ImagePullBackOff")).toBe(CHIP_IMAGE_PULL_FAILED);
    // An unknown reason must not be asserted to be a machine wait.
    // The chip is the only register a narrow header shows, so a wrong short
    // answer there is worse than a long true one.
    expect(statusDetailChip("agent: SomeFutureReason: x", "SomeFutureReason")).toBe(
      STARTING_RAW_PREFIX + "SomeFutureReason",
    );
    expect(statusDetailChip("pod: Pending", "Pending")).toBe(CHIP_WAITING_FOR_MACHINE);
    for (const chip of [CHIP_DOWNLOADING, CHIP_SETTING_UP, CHIP_WAITING_FOR_MACHINE, CHIP_IMAGE_PULL_FAILED]) {
      expect(chip.length).toBeLessThanOrEqual(24);
      expect(chip.toLowerCase()).not.toContain("starting");
    }
  });
});

// The one cheap guard against mirror drift. internal/runner/waiting.go is the
// canonical list — the k8s poll loops, the control plane's read projection and
// this console all read from it — and nothing but this test would notice a
// seventh reason being added on the Go side.
describe("TERMINAL_STATUS_REASONS mirrors the Go list", () => {
  it("equals internal/runner/waiting.go's TerminalWaitingReasons", () => {
    // process.cwd() is the vitest root — ui/ — as console-rules-citations.test.ts
    // documents for its own doc resolution.
    const go = readFileSync(resolve(process.cwd(), "../internal/runner/waiting.go"), "utf8");
    const block = go.slice(go.indexOf("var TerminalWaitingReasons"));
    const fromGo = [...block.slice(0, block.indexOf("}")).matchAll(/"([A-Za-z]+)":\s*true/g)].map((m) => m[1]);
    expect(fromGo.length).toBeGreaterThan(0);
    expect([...fromGo].sort()).toEqual([...TERMINAL_STATUS_REASONS].sort());
  });
});

// ---- #1419: the run page's startup view --------------------------------------

const T0 = Date.UTC(2000, 0, 1, 12); // fixed reference instant for the mocked clock
const ago = (ms: number) => new Date(T0 - ms).toISOString();
const S = 1000;
const MIN = 60 * S;

type RunIn = Parameters<typeof runStartupView>[0];
const mk = (over: Partial<RunIn> & { createdAgo?: number; updatedAgo?: number }): RunIn => {
  const { createdAgo = 0, updatedAgo = 0, ...rest } = over;
  return { state: "STARTING", created_at: ago(createdAgo), updated_at: ago(updatedAgo), ...rest };
};
const view = (run: RunIn, o: { lastStep?: StartupLastStep; sawBuilding?: boolean } = {}) =>
  runStartupView(run, T0, { lastStep: "terminal", sawBuilding: false, ...o });
const marks = (v: ReturnType<typeof view>) => v!.rows.map((r) => `${r.label}=${r.mark}`);

const START = RUN_STARTUP.STEP_START;
const DL = CHIP_DOWNLOADING;
const TERM = RUN_STARTUP.STEP_TERMINAL;
const PULLING = { status_detail: "image: Pulling: wardyn/agent:1", status_reason: "Pulling" };
const BUILDING = { status_detail: "image: Building", status_reason: STATUS_REASON_BUILDING };

describe("runStartupView: the plan's table", () => {
  it("is null unless the run is PENDING or STARTING", () => {
    for (const state of ["RUNNING", "COMPLETED", "FAILED", "KILLED", "STOPPED", "WAITING_FOR_CONFIRMATION"]) {
      expect(view(mk({ state }))).toBeNull();
    }
  });

  it("PENDING, no detail: Start active, quiet before 60 s", () => {
    const v = view(mk({ state: "PENDING", createdAgo: 5 * S }))!;
    expect(marks(v)).toEqual([`${START}=active`, `${DL}=pending`, `${TERM}=pending`]);
    expect(v.hint).toBe("");
    expect(v.alert).toBe("");
  });

  it("PENDING + Building: Build active, the rest pending, BUILD_HINT at once", () => {
    const v = view(mk({ state: "PENDING", createdAgo: 2 * S, ...BUILDING }))!;
    expect(marks(v)).toEqual([
      `${RUN_STARTUP.STEP_BUILD}=active`,
      `${START}=pending`,
      `${DL}=pending`,
      `${TERM}=pending`,
    ]);
    expect(v.hint).toBe(RUN_STARTUP.BUILD_HINT);
  });

  it("STARTING after a Building read (sawBuilding): Build done, then the STARTING rows", () => {
    const v = view(mk({ updatedAgo: 5 * S, createdAgo: 10 * MIN }), { sawBuilding: true })!;
    expect(marks(v)).toEqual([
      `${RUN_STARTUP.STEP_BUILD}=done`,
      `${START}=active`,
      `${DL}=pending`,
      `${TERM}=pending`,
    ]);
  });

  it("STARTING without sawBuilding (a reload): no Build row, and the row is never pending", () => {
    const v = view(mk({ updatedAgo: 5 * S }))!;
    expect(v.rows.map((r) => r.key)).toEqual(["start", "download", "last"]);
    for (const sawBuilding of [true, false]) {
      for (const state of ["PENDING", "STARTING"]) {
        const r = view(mk({ state, ...(state === "PENDING" ? BUILDING : {}) }), { sawBuilding })!;
        expect(r.rows.find((x) => x.key === "build")?.mark ?? "done").not.toBe("pending");
      }
    }
  });

  it("STARTING + a Building line (the server blanks it; defence in depth): reads as no detail", () => {
    const v = view(mk({ updatedAgo: 90 * S, ...BUILDING }))!;
    expect(marks(v)[0]).toBe(`${START}=active`);
    expect(v.rows.some((r) => r.key === "build")).toBe(false);
    expect(v.hint).toBe(RUN_STARTUP.SLOW);
  });

  it("STARTING + Pulling: Start done, Download active, DOWNLOAD_HINT at once", () => {
    const v = view(mk({ updatedAgo: 3 * S, ...PULLING }))!;
    expect(marks(v)).toEqual([`${START}=done`, `${DL}=active`, `${TERM}=pending`]);
    expect(v.hint).toBe(SIGNIN_PROGRESS.DOWNLOAD_HINT);
  });

  it("STARTING + ContainerCreating / Unschedulable / Pending: the substrate sentence, once slow", () => {
    const cases: Array<[string, string, string]> = [
      ["agent: ContainerCreating", "ContainerCreating", STARTING_CONTAINER_CREATING],
      ["pod: Unschedulable: 0/1 nodes", "Unschedulable", STARTING_UNSCHEDULABLE],
      ["pod: Pending", "Pending", STARTING_WAITING_FOR_NODE],
    ];
    for (const [status_detail, status_reason, sentence] of cases) {
      const quiet = view(mk({ updatedAgo: 59 * S, status_detail, status_reason }))!;
      expect(marks(quiet)).toEqual([`${START}=active`, `${DL}=pending`, `${TERM}=pending`]);
      expect(quiet.hint).toBe("");
      expect(view(mk({ updatedAgo: 60 * S, status_detail, status_reason }))!.hint).toBe(sentence);
    }
  });

  it("STARTING, no detail: SLOW from 60 s", () => {
    expect(view(mk({ updatedAgo: 59 * S }))!.hint).toBe("");
    expect(view(mk({ updatedAgo: 60 * S }))!.hint).toBe(RUN_STARTUP.SLOW);
    expect(RUN_POLL_SLOW_START_MS).toBe(60 * S);
  });

  it("PENDING with no detail: SLOW from 60 s measured from created_at", () => {
    expect(view(mk({ state: "PENDING", createdAgo: 59 * S }))!.hint).toBe("");
    expect(view(mk({ state: "PENDING", createdAgo: 60 * S }))!.hint).toBe(RUN_STARTUP.SLOW);
  });

  it("STARTING clock is updated_at: created 10 min ago, updated 5 s ago -> no hint (a build just finished)", () => {
    const v = view(mk({ createdAgo: 10 * MIN, updatedAgo: 5 * S }))!;
    expect(v.hint).toBe("");
    expect(v.rows[0].mark).toBe("active");
  });

  it("PENDING clock is created_at, whatever updated_at says", () => {
    expect(view(mk({ state: "PENDING", createdAgo: 61 * S, updatedAgo: 1 * S }))!.hint).toBe(RUN_STARTUP.SLOW);
  });

  it("overdue in STARTING: 4:29 is live, 4:30 drops the spinner and says OVERDUE", () => {
    const live = view(mk({ updatedAgo: 4 * MIN + 29 * S, ...PULLING }))!;
    expect(marks(live)).toEqual([`${START}=done`, `${DL}=active`, `${TERM}=pending`]);
    expect(live.hint).toBe(SIGNIN_PROGRESS.DOWNLOAD_HINT);

    const late = view(mk({ updatedAgo: 4 * MIN + 30 * S, ...PULLING }))!;
    expect(marks(late)).toEqual([`${START}=done`, `${DL}=pending`, `${TERM}=pending`]);
    expect(late.rows.some((r) => r.mark === "active")).toBe(false);
    expect(late.hint).toBe(RUN_STARTUP.OVERDUE);
    expect(late.alert).toBe("");
    // Only the row that was active is marked stale.
    expect(late.rows.filter((r) => r.stale).map((r) => r.label)).toEqual([DL]);
    expect(live.rows.some((r) => r.stale)).toBe(false);
  });

  it("overdue in STARTING with no detail: Start loses its spinner", () => {
    const late = view(mk({ updatedAgo: RUN_START_OVERDUE_MS }))!;
    expect(marks(late)[0]).toBe(`${START}=pending`);
    expect(late.hint).toBe(RUN_STARTUP.OVERDUE);
  });

  it("overdue in PENDING: 29:59 is live, 30:00 drops the spinner (Building or not)", () => {
    const live = view(mk({ state: "PENDING", createdAgo: 30 * MIN - S, ...BUILDING }))!;
    expect(live.rows[0].mark).toBe("active");
    expect(live.hint).toBe(RUN_STARTUP.BUILD_HINT);
    const late = view(mk({ state: "PENDING", createdAgo: 30 * MIN, ...BUILDING }))!;
    expect(late.rows.some((r) => r.mark === "active")).toBe(false);
    expect(late.rows[0].label).toBe(RUN_STARTUP.STEP_BUILD);
    expect(late.hint).toBe(RUN_STARTUP.OVERDUE);
    const plain = view(mk({ state: "PENDING", createdAgo: 30 * MIN }))!;
    expect(plain.rows.some((r) => r.mark === "active")).toBe(false);
    expect(plain.hint).toBe(RUN_STARTUP.OVERDUE);
  });

  it("STARTING + a terminal PULL reason: Start done, Download failed, list stops, alert carries the registry's words", () => {
    for (const reason of ["ImagePullBackOff", "ErrImagePull", "InvalidImageName"]) {
      const detail = `agent: ${reason}: rpc error: code = NotFound desc = not found`;
      const v = view(mk({ updatedAgo: 2 * S, status_detail: detail, status_reason: reason }))!;
      expect(marks(v)).toEqual([`${START}=done`, `${RUN_STARTUP.STEP_DOWNLOAD_FAILED}=failed`]);
      expect(v.hint).toBe("");
      expect(v.alert).toBe(statusDetailSentence(detail, reason));
      expect(v.alert).toContain("rpc error: code = NotFound desc = not found");
    }
    expect(view(mk({ status_detail: "agent: ImagePullBackOff: x", status_reason: "ImagePullBackOff" }))!.alert).toContain(
      STUCK_IMAGE_PULL,
    );
  });

  it("a terminal reason outranks overdue", () => {
    const v = view(
      mk({ updatedAgo: 10 * MIN, status_detail: "agent: ImagePullBackOff: x", status_reason: "ImagePullBackOff" }),
    )!;
    expect(v.hint).toBe("");
    expect(v.rows[1].mark).toBe("failed");
  });

  it("STARTING + another terminal reason: Start failed, list stops, existing sentence", () => {
    for (const [reason, lead] of [
      ["CrashLoopBackOff", STUCK_CRASH_LOOP],
      ["CreateContainerError", STUCK_CREATE_CONTAINER],
      ["CreateContainerConfigError", STUCK_CREATE_CONTAINER],
    ]) {
      const v = view(mk({ status_detail: `agent: ${reason}: back-off`, status_reason: reason }))!;
      expect(marks(v)).toEqual([`${RUN_STARTUP.STEP_START_FAILED}=failed`]);
      expect(v.alert).toBe(`${lead} back-off`);
    }
  });

  it("a terminal reason with no detail still alerts (the server rebuilds it, but the guard holds)", () => {
    const v = view(mk({ status_reason: "CrashLoopBackOff" }))!;
    expect(v.alert).not.toBe("");
  });

  it("an unparseable clock reads as zero elapsed, never overdue", () => {
    const v = view(mk({ created_at: "nope", updated_at: "nope" }))!;
    expect(v.hint).toBe("");
    expect(v.rows[0].mark).toBe("active");
  });
});

describe("runStartupView: the last row", () => {
  const label = (lastStep: StartupLastStep) => view(mk({}), { lastStep })!.rows.at(-1)!.label;
  it("names what comes next", () => {
    expect(label("terminal")).toBe(RUN_STARTUP.STEP_TERMINAL);
    expect(label("task")).toBe(RUN_STARTUP.STEP_TASK);
    expect(label("command")).toBe(RUN_STARTUP.STEP_COMMAND);
  });
  it("null: an interactive run this viewer cannot attach has no last row", () => {
    const v = view(mk({ ...PULLING }), { lastStep: null })!;
    expect(marks(v)).toEqual([`${START}=done`, `${DL}=active`]);
    expect(view(mk({ state: "PENDING", ...BUILDING }), { lastStep: null })!.rows).toHaveLength(3);
  });
});

describe("runStartupView: mirrors of the Go sources", () => {
  const go = (rel: string) => readFileSync(resolve(process.cwd(), "..", rel), "utf8");

  it("RUN_START_OVERDUE_MS = canaryWaitTimeout + podIPWaitTimeout (internal/runner/k8s/canary.go)", () => {
    const src = go("internal/runner/k8s/canary.go");
    const dur = (name: string) => {
      const m = new RegExp(`${name}\\s*=\\s*(\\d+)\\s*\\*\\s*time\\.(Minute|Second)`).exec(src);
      expect(m, name).not.toBeNull();
      return Number(m![1]) * (m![2] === "Minute" ? MIN : S);
    };
    expect(RUN_START_OVERDUE_MS).toBe(dur("canaryWaitTimeout") + dur("podIPWaitTimeout"));
  });

  it("RUN_PENDING_OVERDUE_MS = imageBuildTimeout (internal/api/runs.go)", () => {
    const m = /const imageBuildTimeout = (\d+) \* time\.Minute/.exec(go("internal/api/runs.go"));
    expect(m).not.toBeNull();
    expect(RUN_PENDING_OVERDUE_MS).toBe(Number(m![1]) * MIN);
  });

  // The Go constant is the server's own token; a rename on either side fails
  // here rather than silently dropping the "Building the image" step.
  const goBuilding = /statusReasonBuilding\s*=\s*"([^"]+)"/.exec(
    go("internal/api/runs_status_detail.go"),
  );
  it("STATUS_REASON_BUILDING equals Go's statusReasonBuilding", () => {
    expect(goBuilding).not.toBeNull();
    expect(STATUS_REASON_BUILDING).toBe(goBuilding![1]);
  });
});

describe("Building arms and the image-pull predicate", () => {
  it("the header chip and the board sentence read Building from the shared canon", () => {
    expect(statusDetailChip("image: Building", "Building")).toBe(RUN_STARTUP.STEP_BUILD);
    expect(statusDetailSentence("image: Building", "Building")).toBe(RUN_STARTUP.BUILD_HINT);
    expect(statusDetailChip("image: Building")).toBe(RUN_STARTUP.STEP_BUILD);
  });
  it("isImagePullFailure is exactly the three pull reasons", () => {
    for (const r of ["ImagePullBackOff", "ErrImagePull", "InvalidImageName"]) expect(isImagePullFailure(r)).toBe(true);
    for (const r of ["CrashLoopBackOff", "Pulling", "", null, undefined]) expect(isImagePullFailure(r)).toBe(false);
  });
});
