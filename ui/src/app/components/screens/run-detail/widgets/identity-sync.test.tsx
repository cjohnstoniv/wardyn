/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import type { AuditEvent, RunDetail } from "../../../../lib/types";
import { RUN_SYNC } from "../../../wardyn/copy/runners";
import { SyncRows, syncDirs } from "./identity-sync";

vi.mock("../../../../lib/api/audit", () => ({ audit: { listAudit: vi.fn().mockResolvedValue([]) } }));
vi.mock("../../../../lib/api/health", () => ({ health: { health: vi.fn().mockResolvedValue({ status: "ok" }) } }));

afterEach(cleanup);

// The server's sentence for a sync session it ended at its size limit (localFolderCapText), as the session's row keeps it.
const CAP = "This sync session reached its 512.0 MB limit for sending in, set by the Platform unit (locked)";
const ended = (data: Record<string, unknown>): AuditEvent => ({
  id: "e1",
  time: "2026-11-23T14:31:00Z",
  actor_type: "human",
  actor: "dana",
  action: "ssh.sync.transfer",
  outcome: "failure",
  data: { dir: "/home/agent/work/proj", bytes_in: 512 * 1024 * 1024, bytes_out: 0, ...data },
});

describe("Sync rows: a session ended at its size limit", () => {
  it("keeps the server's sentence as the session's error, word for word", () => {
    expect(syncDirs([ended({ error: CAP })])[0].error).toBe(CAP);
  });

  it("shows the sentence inside the last-session line, with no local_folder: prefix", () => {
    render(<dl>{<SyncRows run={{ id: "r1", state: "COMPLETED", placement: "remote" } as unknown as RunDetail} audit={[ended({ error: CAP })]} onGoAudit={() => {}} />}</dl>);
    expect(screen.getByText(RUN_SYNC.ERROR(CAP))).toBeInTheDocument();
    expect(screen.getByText(`Last session ended with an error: ${CAP}`)).toBeInTheDocument();
    expect(screen.getByText(RUN_SYNC.BYTES("512.0 MB", "0 B"))).toBeInTheDocument();
    expect(screen.queryByText(/local_folder:/)).toBeNull();
  });
});
