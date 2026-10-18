/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { beforeEach, describe, expect, it, vi } from "vitest";
import type { AuditEvent } from "../../../lib/types";
import { withNewestEvents } from "./audit-window";

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
    listAuditMock.mockRejectedValue(new Error("boom"));
    const head = rows(0, 1000);
    expect(await withNewestEvents("r", head)).toEqual({ events: head, gap: true });
  });
});
