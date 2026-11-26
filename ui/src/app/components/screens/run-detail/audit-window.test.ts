/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { beforeEach, describe, expect, it, vi } from "vitest";
import { LIST_LIMIT } from "../../../lib/api/core";
import type { AuditEvent } from "../../../lib/types";
import { joinRows, readTrail, touchesRecordingIndex, withNewestEvents } from "./audit-window";

const listAuditMock = vi.fn();
vi.mock("../../../lib/api/audit", () => ({ audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) } }));

const rows = (from: number, to: number): AuditEvent[] =>
  Array.from({ length: to - from }, (_, i) => ({ id: `e${from + i}`, action: "egress.allow" }) as AuditEvent);

beforeEach(() => listAuditMock.mockReset());

describe("withNewestEvents", () => {
  it("leaves a trail that fits its first page alone, with no second read", async () => {
    const head = rows(0, 999);
    expect(await withNewestEvents("r", head)).toEqual({ events: head, gap: false });
    expect(listAuditMock).not.toHaveBeenCalled();
  });

  it("joins the newest page onto a full first page, so the latest events are not cut off", async () => {
    listAuditMock.mockResolvedValue(rows(1500, 2500));
    const out = await withNewestEvents("r", rows(0, 1000));
    expect(listAuditMock).toHaveBeenCalledWith("r", { newest: true });
    expect(out.events).toHaveLength(2000);
    expect(out.events[out.events.length - 1].id).toBe("e2499");
    expect(out.gap).toBe(true);
  });

  it("drops the rows the two pages share, and claims no gap when they meet", async () => {
    listAuditMock.mockResolvedValue(rows(500, 1500));
    const out = await withNewestEvents("r", rows(0, 1000));
    expect(out.events).toHaveLength(1500);
    expect(out.gap).toBe(false);
  });

  it("keeps the first page and flags the gap when the newest page cannot be read", async () => {
    listAuditMock.mockRejectedValueOnce(new Error("boom"));
    const head = rows(0, 1000);
    const out = await withNewestEvents("r", head);
    expect(listAuditMock).toHaveBeenCalledTimes(1);
    expect(out).toEqual({ events: head, gap: true });
  });
});

const trail = (events: AuditEvent[]) => ({ events, gap: false });

describe("joinRows", () => {
  it("appends only the rows it does not hold", () => {
    const out = joinRows(trail(rows(0, 3)), rows(2, 5));
    expect(out.events.map((e) => e.id)).toEqual(["e0", "e1", "e2", "e3", "e4"]);
    expect(out.gap).toBe(false);
  });

  it("keeps the oldest and newest LIST_LIMIT rows past twice the cap and marks the gap", () => {
    const out = joinRows(trail(rows(0, 1990)), rows(1990, 2010));
    expect(out.events).toHaveLength(2 * LIST_LIMIT);
    expect(out.events[0].id).toBe("e0");
    expect(out.events[LIST_LIMIT - 1].id).toBe("e999");
    expect(out.events[LIST_LIMIT].id).toBe("e1010");
    expect(out.events[2 * LIST_LIMIT - 1].id).toBe("e2009");
    expect(out.gap).toBe(true);
  });
});

describe("readTrail", () => {
  it("with a trail held reads only the newest window and joins its new rows", async () => {
    listAuditMock.mockResolvedValue(rows(3, 8));
    const out = await readTrail("r", trail(rows(0, 5)));
    expect(listAuditMock).toHaveBeenCalledTimes(1);
    expect(listAuditMock).toHaveBeenCalledWith("r", { newest: true, limit: 100 });
    expect(out.added?.map((e) => e.id)).toEqual(["e5", "e6", "e7"]);
    expect(out.trail.events).toHaveLength(8);
  });

  it("reads the oldest page again when the window holds no row the trail holds", async () => {
    listAuditMock.mockResolvedValueOnce(rows(100, 200)).mockResolvedValueOnce(rows(0, 5));
    const out = await readTrail("r", trail(rows(0, 5)));
    expect(listAuditMock.mock.calls).toEqual([["r", { newest: true, limit: 100 }], ["r"]]);
    expect(out.added).toBeNull();
  });

  it("keeps a full oldest page and reads only the newest page behind it", async () => {
    listAuditMock.mockResolvedValueOnce(rows(5000, 5100)).mockResolvedValueOnce(rows(5000, 6000));
    const out = await readTrail("r", trail(rows(0, 1000)));
    expect(listAuditMock.mock.calls.filter(([, o]) => o === undefined)).toHaveLength(0);
    expect(listAuditMock).toHaveBeenCalledWith("r", { newest: true });
    expect(out.added).toBeNull();
    expect(out.trail.events).toHaveLength(2000);
  });

  it("reads whole on a first read and on an empty trail", async () => {
    listAuditMock.mockResolvedValue(rows(0, 3));
    for (const held of [null, trail([])]) {
      listAuditMock.mockClear();
      const out = await readTrail("r", held);
      expect(listAuditMock.mock.calls).toEqual([["r"]]);
      expect(out).toEqual({ trail: trail(rows(0, 3)), added: null });
    }
  });
});

describe("touchesRecordingIndex", () => {
  it("sees session.recording and desktop.recording rows only", () => {
    const of = (action: string) => [{ id: "x", action } as AuditEvent];
    expect(touchesRecordingIndex(of("session.recording.write"))).toBe(true);
    expect(touchesRecordingIndex(of("desktop.recording.pause"))).toBe(true);
    expect(touchesRecordingIndex(of("egress.allow"))).toBe(false);
    expect(touchesRecordingIndex(of("run.kill"))).toBe(false);
  });
});
