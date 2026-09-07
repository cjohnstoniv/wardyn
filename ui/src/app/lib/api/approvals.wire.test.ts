/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The approve/deny DECISION BODY, pinned against the real module.
//
// Why this file has to exist (F125): every UI test that decides an approval
// mocks lib/api/approvals WHOLESALE — that is a deliberate rule, written down
// in approvals.ts's own header and in lib/types/approvals.ts's decisionArgs
// note — so before this suite nothing anywhere observed the JSON approve()
// and deny() actually build. The daemon does not catch a wrong key either: its
// decision body is decoded BY HAND precisely so DisallowUnknownFields is NOT
// set (internal/api/approvals.go's decodeDecisionRequest), so an unknown key is
// dropped in silence and Normalize() keeps the default `run` scope — no 400, no
// log, no console error. Renaming decision_scope/decision_expires_at used to
// leave the whole 1800-test suite green; it fails HERE now.
//
// Same three layers as runs.wire.fields.test.ts, which is this file's pattern:
//   A. the exact body for every DecisionOptions shape, approve AND deny;
//   B. the deliberate omissions (a bare 2-arg call is byte-identical to the
//      pre-scope wire body — the property decisionArgs("run") exists to keep);
//   C. SOURCE PARITY with the Go DTO: every json tag on internal/api's
//      decisionRequest is a key this module can send, read out of the Go source
//      rather than hand-retyped, so a rename on either side goes red.
//
// Run: cd ui && pnpm vitest run src/app/lib/api/approvals.wire.test.ts

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { approvals } from "./approvals";
import { decisionArgs } from "../types";

let fetchMock: ReturnType<typeof vi.fn>;
beforeEach(() => {
  // A FRESH Response per call — a Response body reads once, and the
  // approve+deny cases below call fetch twice inside one test.
  fetchMock = vi.fn().mockImplementation(
    async () =>
      new Response(JSON.stringify({ id: "ap_1", state: "APPROVED" }), {
        status: 200,
        headers: { "content-type": "application/json" },
      }),
  );
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => vi.unstubAllGlobals());

const sentBody = (call = 0): Record<string, unknown> =>
  JSON.parse(String(fetchMock.mock.calls[call][1]?.body ?? "{}"));
const sentPath = (call = 0): string => String(fetchMock.mock.calls[call][0]);
const sentMethod = (call = 0): string => String(fetchMock.mock.calls[call][1]?.method);

const UNTIL = "2026-09-30T12:00:00.000Z";

// ---------------------------------------------------------------------------
// A. The exact body, both verbs, every scope shape.
// ---------------------------------------------------------------------------
describe("approvals.approve/deny — the decision body on the wire", () => {
  it("sends ONLY {reason} for a bare 2-argument call (the pre-scope body, byte for byte)", async () => {
    await approvals.approve("ap_1", "looks fine");
    expect(sentBody()).toEqual({ reason: "looks fine" });
    expect(sentMethod()).toBe("POST");
    expect(sentPath()).toContain("/approvals/ap_1/approve");

    await approvals.deny("ap_2", "no");
    expect(sentBody(1)).toEqual({ reason: "no" });
    expect(sentPath(1)).toContain("/approvals/ap_2/deny");
  });

  it("puts the scope on decision_scope — NOT `scope`, which collides with the request's own requested_scope", async () => {
    await approvals.approve("ap_1", "once only", { scope: "once" });
    expect(sentBody()).toEqual({ reason: "once only", decision_scope: "once" });

    await approvals.deny("ap_1", "never", { scope: "always" });
    expect(sentBody(1)).toEqual({ reason: "never", decision_scope: "always" });
  });

  it("puts the deadline on decision_expires_at — NOT `expires_at`, which collides with the EXPIRED state", async () => {
    await approvals.approve("ap_1", "until standup", { scope: "until", until: UNTIL });
    expect(sentBody()).toEqual({
      reason: "until standup",
      decision_scope: "until",
      decision_expires_at: UNTIL,
    });

    await approvals.deny("ap_1", "until standup", { scope: "until", until: UNTIL });
    expect(sentBody(1)).toEqual({
      reason: "until standup",
      decision_scope: "until",
      decision_expires_at: UNTIL,
    });
  });

  it("carries the four ApprovalScope values verbatim, approve and deny alike", async () => {
    const scopes = ["once", "run", "until", "always"] as const;
    for (const scope of scopes) {
      fetchMock.mockClear();
      await approvals.approve("ap_1", "r", { scope });
      expect(sentBody()).toEqual({ reason: "r", decision_scope: scope });
      fetchMock.mockClear();
      await approvals.deny("ap_1", "r", { scope });
      expect(sentBody()).toEqual({ reason: "r", decision_scope: scope });
    }
  });

  it("id is URL-encoded into the path, never interpolated raw", async () => {
    await approvals.approve("ap/../1 2", "r");
    expect(sentPath()).toContain(`/approvals/${encodeURIComponent("ap/../1 2")}/approve`);
    expect(sentPath()).not.toContain("ap/../1 2");
  });
});

// ---------------------------------------------------------------------------
// B. The deliberate omissions — an absent option is an ABSENT KEY, not null.
//    Go reads both fields as omitempty and Normalize() keeps `run`, so a key
//    present-but-empty would be a different wire fact than "not sent".
// ---------------------------------------------------------------------------
describe("approvals.approve/deny — omissions", () => {
  it("omits decision_expires_at entirely when no `until` is given (never null, never empty)", async () => {
    await approvals.approve("ap_1", "r", { scope: "always" });
    expect(Object.keys(sentBody()).sort()).toEqual(["decision_scope", "reason"]);
  });

  it("omits decision_scope entirely when opts carries none", async () => {
    await approvals.approve("ap_1", "r", { until: UNTIL });
    expect(Object.keys(sentBody()).sort()).toEqual(["decision_expires_at", "reason"]);
  });

  it("decisionArgs('run') spreads to a 2-argument call, so the default scope puts NOTHING on the wire", async () => {
    await approvals.approve("ap_1", "r", ...decisionArgs("run"));
    expect(sentBody()).toEqual({ reason: "r" });
    // …and any other scope does put it there, through the same helper the
    // three decision surfaces spread.
    fetchMock.mockClear();
    await approvals.approve("ap_1", "r", ...decisionArgs("until", UNTIL));
    expect(sentBody()).toEqual({ reason: "r", decision_scope: "until", decision_expires_at: UNTIL });
  });
});

// ---------------------------------------------------------------------------
// C. Source parity with the Go wire — the TS-side twin of
//    internal/types/terminal_parity_test.go, same discipline as
//    runs.wire.fields.test.ts's section C.
// ---------------------------------------------------------------------------
function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found walking up from " + process.cwd());
}

/** The json tag names of one Go struct body: `type <name> struct { ... }`. */
function goJSONTags(src: string, structName: string): string[] {
  const m = new RegExp(`type\\s+${structName}\\s+struct\\s*\\{([\\s\\S]*?)\\n\\}`).exec(src);
  if (!m) throw new Error(`struct ${structName} not found`);
  const tags: string[] = [];
  for (const t of m[1].matchAll(/json:"([^",]+)(?:,[^"]*)?"/g)) {
    if (t[1] !== "-") tags.push(t[1]);
  }
  return tags;
}

describe("source parity — internal/api decisionRequest vs the body this module builds", () => {
  const approvalsGo = readFileSync(join(repoRoot(), "internal/api/approvals.go"), "utf8");

  it("every json tag on the Go decision body is a key approve() can send", async () => {
    const goTags = goJSONTags(approvalsGo, "decisionRequest");
    expect(goTags.length).toBe(3); // stale-regex guard: reason + the two decision_*
    await approvals.approve("ap_1", "r", { scope: "until", until: UNTIL });
    const wireKeys = new Set(Object.keys(sentBody()));
    const missing = goTags.filter((t) => !wireKeys.has(t));
    expect(
      missing,
      "Go decision-body fields the console can never send — forward them in " +
        "approvals.ts's approve()/deny() (and here), or the daemon silently drops them",
    ).toEqual([]);
  });

  it("and the reverse: every key deny() sends is a json tag on the Go decision body", async () => {
    const goTags = new Set(goJSONTags(approvalsGo, "decisionRequest"));
    await approvals.deny("ap_1", "r", { scope: "until", until: UNTIL });
    for (const k of Object.keys(sentBody())) {
      expect(
        goTags.has(k),
        `${k} is not a json tag on internal/api's decisionRequest — decodeDecisionRequest ` +
          "decodes BY HAND without DisallowUnknownFields, so the daemon would drop it in silence",
      ).toBe(true);
    }
  });
});
