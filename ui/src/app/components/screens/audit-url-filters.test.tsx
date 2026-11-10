/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Audit search text and Event/Actor facets live in the URL (q, kind, actor), and opening a run pushes a
// history entry so Back removes it. Its own file: audit.test.tsx is at the file-size limit.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";
import userEvent from "@testing-library/user-event";
import { LaggingRouter } from "../../../test/lagging-router";
import type { AuditEvent } from "../../lib/types";
import { aheadByHours } from "../../lib/test-clock";

const listAuditMock = vi.fn();
vi.mock("../../lib/api/audit", async (importOriginal) => ({
  audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) },
  gatewayCallId: (await importOriginal<typeof import("../../lib/api/audit")>()).gatewayCallId,
}));
vi.mock("../../lib/api/runs", () => ({
  runs: { getRun: vi.fn().mockResolvedValue(undefined), listRuns: vi.fn().mockResolvedValue([]) },
}));
vi.mock("../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({}), whoami: () => Promise.resolve(null) },
}));

import { AuditScreen } from "./audit";

function ev(partial: Partial<AuditEvent> & { id: string }): AuditEvent {
  return { time: aheadByHours(-1), actor_type: "agent", actor: "spiffe://wardyn/agent", action: "tool.call", outcome: "success", ...partial } as AuditEvent;
}

describe("AuditScreen URL filters", () => {
  beforeEach(() => listAuditMock.mockReset());

  function Probe() {
    const location = useLocation();
    const navigate = useNavigate();
    return (
      <>
        <output data-testid="search">{location.search}</output>
        <button onClick={() => navigate(-1)}>history back</button>
      </>
    );
  }
  const renderAt = (entries: string[], index?: number) =>
    render(
      <MemoryRouter initialEntries={entries} initialIndex={index}>
        <AuditScreen />
        <Probe />
      </MemoryRouter>,
    );

  it("keeps the search and the Event and Actor facets in the URL, and reads them back on a fresh mount", async () => {
    listAuditMock.mockResolvedValue([
      ev({ id: "e1", run_id: "run_111", action: "egress.allow", actor_type: "agent" }),
      ev({ id: "e2", run_id: "run_222", action: "tool.exec", actor_type: "human" }),
    ]);
    renderAt(["/?q=run_111&kind=egress&actor=agent"]);
    expect(await screen.findByLabelText("Search events")).toHaveValue("run_111");
    expect(screen.getByRole("combobox", { name: "Actor" })).toHaveTextContent("Agent");
    fireEvent.change(screen.getByLabelText("Search events"), { target: { value: "run_1112" } });
    expect(screen.getByTestId("search")).toHaveTextContent("?q=run_1112&kind=egress&actor=agent");
  });

  it("opening a run pushes an entry that keeps the search, and Back removes the drill-in only", async () => {
    listAuditMock.mockResolvedValueOnce([
      ev({ id: "e1", run_id: "run_111", action: "egress.allow" }),
      ev({ id: "e2", run_id: "run_222", action: "egress.deny" }),
    ]);
    listAuditMock.mockResolvedValue([ev({ id: "e1", run_id: "run_111", action: "egress.allow" })]);
    renderAt(["/", "/?q=egress"], 1);
    fireEvent.click((await screen.findAllByRole("button", { name: "Run" }))[0]);
    await waitFor(() => expect(screen.getByTestId("search")).toHaveTextContent("run_id=run_111"));
    expect(screen.getByTestId("search")).toHaveTextContent("q=egress");
    fireEvent.click(screen.getByRole("button", { name: "history back" }));
    await waitFor(() => expect(screen.getByTestId("search")).toHaveTextContent(/^\?q=egress$/));
  });

  it("typing several characters quickly keeps them all while the URL answers late", async () => {
    listAuditMock.mockResolvedValue([ev({ id: "e1", run_id: "run_111" })]);
    render(
      <LaggingRouter>
        <AuditScreen />
      </LaggingRouter>,
    );
    await screen.findByLabelText("Search events");
    const box = screen.getByLabelText("Search events");
    const user = userEvent.setup({ delay: null });
    await user.type(box, "abc");
    expect(box).toHaveValue("abc");
    await new Promise((r) => setTimeout(r, 80));
    expect(box).toHaveValue("abc");
  });

});
