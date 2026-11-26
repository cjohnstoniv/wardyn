/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// An open run page's poll reads the newest window of its audit trail, not the oldest page again, and reads the three
// recording lists again only when the trail shows a new recording row or the last reads were not whole.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { AuditEvent } from "../../../lib/types";

type Filter = { action?: string; actionPrefix?: string; newest?: boolean; limit?: number } | undefined;
const answer = vi.hoisted(() => ({ fn: (_f: Filter): Promise<unknown[]> => Promise.resolve([]) }));
const calls = vi.hoisted(() => ({ list: [] as Filter[] }));
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    getRun: (id: string) => Promise.resolve({ id, state: "RUNNING", task: "t", agent: "a" }),
    getGrants: () => Promise.resolve([]),
  },
}));
vi.mock("../../../lib/api/approvals", () => ({ approvals: { listApprovals: () => Promise.resolve([]) } }));
vi.mock("../../../lib/api/audit", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../../lib/api/audit")>()),
  audit: {
    listAudit: (_id: string, f?: Filter) => {
      calls.list.push(f);
      return answer.fn(f);
    },
  },
}));
vi.mock("../../../lib/api/recordings", () => ({ recordings: { getRecording: () => Promise.resolve(null) } }));
vi.mock("../../../lib/api/health", () => ({ health: { health: () => Promise.resolve({}) } }));

import { aheadByHours } from "../../../lib/test-clock";
import { useRunDetail } from "./use-run-detail";

const row = (id: string, action = "egress.allow"): AuditEvent =>
  ({ id, time: aheadByHours(-1), actor_type: "system", actor: "wardynd", action, target: "run-1", outcome: "success" }) as AuditEvent;
const REC = (id: string) => row(id, "session.recording.write");
const isWindow = (f: Filter) => f?.newest === true && f.limit === 100;
const isPlain = (f: Filter) => f === undefined;
const isSessions = (f: Filter) => f?.actionPrefix === "session.recording";
const count = (is: (f: Filter) => boolean) => calls.list.filter(is).length;

// The plain read answers [a, b]; each poll's window answers the next list, newest-first as the server sends it
// (listAudit hands it back oldest-first), and every recording read answers [].
function serve(windows: AuditEvent[][], sessions: (n: number) => Promise<unknown[]> = () => Promise.resolve([])) {
  let w = 0;
  let s = 0;
  answer.fn = (f) => {
    if (isWindow(f)) return Promise.resolve(windows[Math.min(w++, windows.length - 1)]);
    if (isSessions(f)) return sessions(++s);
    return Promise.resolve(isPlain(f) ? [row("a"), row("b")] : []);
  };
}

function hook() {
  return renderHook(() => useRunDetail("run-1"), { wrapper: ({ children }) => <MemoryRouter>{children}</MemoryRouter> });
}

const poll = async () => {
  await vi.advanceTimersByTimeAsync(4100);
};

beforeEach(() => {
  calls.list = [];
  vi.useFakeTimers({ shouldAdvanceTime: true });
});
afterEach(() => vi.useRealTimers());

describe("useRunDetail trail window", () => {
  it("a poll after the first read asks for the newest window, not the oldest page", async () => {
    serve([[row("b"), row("c")]]);
    const { result } = hook();
    await waitFor(() => expect(result.current.status).toBe("ready"));
    expect(count(isWindow)).toBe(0);
    await poll();
    expect(count(isWindow)).toBeGreaterThan(0);
    expect(count(isPlain)).toBe(1);
    await waitFor(() => expect(result.current.audit.map((e) => e.id)).toEqual(["a", "b", "c"]));
  });

  it("a poll skips the recording reads until the window shows a recording row", async () => {
    serve([[row("b"), row("c")], [row("c"), row("d")], [row("d"), REC("e")], [REC("e"), row("f")]]);
    const { result } = hook();
    await waitFor(() => expect(result.current.status).toBe("ready"));
    expect(count(isSessions)).toBe(1);
    // The first read was whole, so the recordings are due on the first poll.
    await poll();
    expect(count(isSessions)).toBe(2);
    // A window that adds only egress.allow, then one that adds a recording row: no read in either poll.
    await poll();
    expect(count(isSessions)).toBe(2);
    await poll();
    expect(count(isSessions)).toBe(2);
    // The poll after the recording row reads them.
    await poll();
    expect(count(isSessions)).toBe(3);
  });

  it("a failed recording read is read again on the next poll", async () => {
    // The sessions read made on the fourth poll is the third one made; it rejects.
    serve(
      [[row("b"), row("c")], [row("c"), row("d")], [row("d"), REC("e")], [REC("e"), row("f")], [row("f"), row("g")]],
      (n) => (n === 3 ? Promise.reject(new Error("audit store down")) : Promise.resolve([])),
    );
    const { result } = hook();
    await waitFor(() => expect(result.current.status).toBe("ready"));
    await poll(); // first poll: due after the whole first read
    expect(count(isSessions)).toBe(2);
    await poll(); // the window adds only egress.allow: no sessions read (control)
    expect(count(isSessions)).toBe(2);
    await poll(); // the window adds a recording row
    expect(count(isSessions)).toBe(2);
    await poll(); // the fourth poll reads again, and the sessions read rejects; its window adds only egress.allow
    expect(count(isSessions)).toBe(3);
    await waitFor(() => expect(result.current.recordingIndex).toBe("failed"));
    await poll(); // the fifth poll reads the sessions again: the last recording reads were not whole
    expect(count(isSessions)).toBe(4);
  });
});
