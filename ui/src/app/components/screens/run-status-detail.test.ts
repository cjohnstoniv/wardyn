/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it, expect } from "vitest";
import {
  isTerminalStatusReason,
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

// 0.7.5 field report, finding 6. Every slow start looked identical: the run sat
// in STARTING and nothing said whether it was pulling an image, waiting to
// schedule, or stuck on a reference that would never resolve. The substrate's
// answer now reaches the wire; this module is where it becomes a sentence.

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

  // N2 (review): a line that did not parse at all is NOT the "nothing has taken
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

  // S2 (review): the server now always sends the detail beside a terminal
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

// round-2 UX S9: the header chip is max-w-[160px]. The sentences above truncate
// there to a restatement of the STARTING badge sitting right beside them, and
// the registry's words — the entire point of a terminal reason — never appear.
describe("statusDetailChip — the short register", () => {
  it("is short, and says what the STARTING badge does not", () => {
    expect(statusDetailChip("image: Pulling: ref", "Pulling")).toBe(CHIP_DOWNLOADING);
    expect(statusDetailChip("agent: ContainerCreating", "ContainerCreating")).toBe(CHIP_SETTING_UP);
    expect(statusDetailChip("pod: Unschedulable: taint", "Unschedulable")).toBe(CHIP_WAITING_FOR_MACHINE);
    expect(statusDetailChip("agent: ImagePullBackOff: denied", "ImagePullBackOff")).toBe(CHIP_IMAGE_PULL_FAILED);
    // N1 (review): an unknown reason must not be asserted to be a machine wait.
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
