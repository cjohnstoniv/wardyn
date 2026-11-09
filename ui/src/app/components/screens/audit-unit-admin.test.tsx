/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, fireEvent, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import type { AuditEvent } from "../../lib/types";
import { aheadByHours } from "../../lib/test-clock";

const listAuditMock = vi.fn();
const getRunMock = vi.fn();
const listRunsMock = vi.fn();
vi.mock("../../lib/api/audit", async (importOriginal) => ({
  audit: {
    listAudit: (...a: unknown[]) => listAuditMock(...a),
  },
  gatewayCallId: (await importOriginal<typeof import("../../lib/api/audit")>()).gatewayCallId,
}));
vi.mock("../../lib/api/runs", () => ({
  runs: {
    getRun: (...a: unknown[]) => getRunMock(...a),
    listRuns: (...a: unknown[]) => listRunsMock(...a),
  },
}));
const healthMock = vi.fn();
const whoamiMock = vi.fn();
vi.mock("../../lib/api/health", () => ({
  health: {
    health: () => healthMock(),
    whoami: () => whoamiMock(),
  },
}));

import { AuditScreen } from "./audit";
import { AUDIT } from "./audit-copy";

function ev(partial: Partial<AuditEvent> & { id: string }): AuditEvent {
  return {
    time: aheadByHours(-1),
    actor_type: "agent",
    actor: "spiffe://wardyn/agent",
    action: "tool.call",
    outcome: "success",
    ...partial,
  } as AuditEvent;
}

// a unit admin (not a security admin) reads the rows of the runs in their units.
describe("a unit admin's Audit page", { timeout: 15_000 }, () => {
  const DESCRIPTION =
    "Append-only. Recorded egress decisions, brokered credentials, approvals and enforcement actions, across the runs in your units.";
  const TODAY =
    "Append-only. Recorded egress decisions, brokered credentials, approvals and enforcement actions, across the runs you can see.";
  const unitsOf = (...names: string[]) => ({
    unit_admin_of: names.map((n, i) => ({ unit_id: `u${i + 1}`, unit_name: n })),
  });

  beforeEach(() => {
    listAuditMock.mockReset();
    listAuditMock.mockResolvedValue([]);
    getRunMock.mockReset();
    getRunMock.mockResolvedValue(undefined);
    listRunsMock.mockReset();
    listRunsMock.mockResolvedValue([]);
    healthMock.mockReset();
    healthMock.mockResolvedValue({});
    whoamiMock.mockReset();
  });

  async function mount(opts: { me: unknown; securityOperator?: boolean; url?: string }) {
    whoamiMock.mockResolvedValue(opts.me);
    const { OperatorProvider } = await import("../wardyn/operator-context");
    return render(
      <MemoryRouter initialEntries={[opts.url ?? "/admin/audit"]}>
        <OperatorProvider operator={false} securityOperator={opts.securityOperator ?? false}>
          <AuditScreen />
        </OperatorProvider>
      </MemoryRouter>,
    );
  }

  it("names the units in the description and the note, and shows no member empty state", async () => {
    listAuditMock.mockResolvedValue([ev({ id: "e1", run_id: "run_111", action: "egress.allow", target: "api.github.com:443" })]);
    await mount({ me: unitsOf("payments") });

    expect(await screen.findByText(DESCRIPTION)).toBeInTheDocument();
    expect(screen.getByText("Runs in your units")).toBeInTheDocument();
    const note = screen.getByText("Runs in your units").parentElement!;
    expect(note).toHaveTextContent(
      "Showing runs in payments and the units below it, and the changes made to those units. Sign-ins aren't listed here, and a run's row leaves out names, paths and error text.",
    );
    expect(screen.getByText("payments")).toHaveClass("font-mono");
    expect(screen.queryByText(/admin-only/i)).not.toBeInTheDocument();
  });

  it("says no runs yet on an empty unfiltered feed, with Open Runs", async () => {
    await mount({ me: unitsOf("payments") });

    expect(await screen.findByText("No runs in your units yet.")).toBeInTheDocument();
    expect(
      screen.getByText(/Recorded egress decisions, brokered credentials, approvals and enforcement actions of a run in/),
    ).toHaveTextContent(/of a run in payments or below it appears here once that run starts\.$/);
    expect(screen.getByRole("button", { name: "Open Runs" })).toBeInTheDocument();
    expect(screen.queryByText(/admin-only/i)).not.toBeInTheDocument();
  });

  it("keeps today's filtered empty state", async () => {
    listAuditMock.mockResolvedValue([ev({ id: "e1", action: "egress.allow", target: "api.github.com:443" })]);
    await mount({ me: unitsOf("payments") });
    await screen.findByText("Allowed egress to api.github.com:443");

    fireEvent.change(screen.getByLabelText("Search events"), { target: { value: "no-such-thing" } });
    expect(await screen.findByText("No events match these filters.")).toBeInTheDocument();
    expect(screen.queryByText("No runs in your units yet.")).not.toBeInTheDocument();
  });

  it("names the first three units and counts the rest", async () => {
    listAuditMock.mockResolvedValue([ev({ id: "e1", action: "egress.allow", target: "api.github.com:443" })]);
    await mount({ me: unitsOf("alpha", "beta", "gamma", "delta") });

    const note = (await screen.findByText("Runs in your units")).parentElement!;
    expect(note).toHaveTextContent(
      "Showing runs in alpha, beta, gamma and 1 more, and the units below them, and the changes made to those units.",
    );
    expect(screen.queryByText("delta")).not.toBeInTheDocument();
  });

  it("opens a drill on a run whose page is closed to the viewer as a unit run, with no Open run link", async () => {
    listAuditMock.mockResolvedValue([ev({ id: "e1", run_id: "run_abcdef12", action: "egress.allow", target: "api.github.com:443" })]);
    getRunMock.mockResolvedValue(undefined);
    await mount({ me: unitsOf("payments"), url: "/admin/audit?run_id=run_abcdef12" });

    expect(await screen.findByText("This run is in your unit")).toBeInTheDocument();
    expect(screen.getByText("Its page isn't open to you. The trail below is what the audit view can show.")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /Open run/ })).not.toBeInTheDocument();
    expect(screen.queryByText(/Run not found/)).not.toBeInTheDocument();
    // The run reference inside this drill is text: nothing to open.
    expect(screen.queryByRole("button", { name: "Run" })).not.toBeInTheDocument();
    // Once in the banner, once in the row: both plain text.
    expect(screen.getAllByText("abcdef12")).toHaveLength(2);
    expect(screen.getByRole("button", { name: "Back to all runs in your units" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Show all events" })).not.toBeInTheDocument();
  });

  it("keeps the not-found line when the drilled trail is empty", async () => {
    listAuditMock.mockResolvedValue([]);
    getRunMock.mockResolvedValue(undefined);
    await mount({ me: unitsOf("payments"), url: "/admin/audit?run_id=run_abcdef12" });

    expect(await screen.findByText("Run not found — it may have been archived or deleted.")).toBeInTheDocument();
    expect(screen.queryByText("This run is in your unit")).not.toBeInTheDocument();
  });

  it("a security admin with unit_admin_of set sees today's page, no note", async () => {
    await mount({ me: unitsOf("payments"), securityOperator: true });

    expect(await screen.findByText(TODAY)).toBeInTheDocument();
    await screen.findByText(/the trail starts with your first run/i);
    expect(screen.queryByText("Runs in your units")).not.toBeInTheDocument();
    expect(screen.queryByText(DESCRIPTION)).not.toBeInTheDocument();
  });

  it("a member with no units sees the admin-only state", async () => {
    await mount({ me: { unit_admin_of: [] } });

    expect(await screen.findByText(AUDIT.MEMBER_FEED_TITLE)).toBeInTheDocument();
    expect(screen.getByText(TODAY)).toBeInTheDocument();
    expect(screen.queryByText("Runs in your units")).not.toBeInTheDocument();
  });

  it("a /me that does not answer leaves the member state", async () => {
    await mount({ me: null });

    expect(await screen.findByText(AUDIT.MEMBER_FEED_TITLE)).toBeInTheDocument();
    expect(screen.queryByText("Runs in your units")).not.toBeInTheDocument();
  });

  it("says Loading run, never Run not found, while a unit drill's trail is still on its way", async () => {
    // Both reads are held by the test: the run answers 404 first, the trail answers after.
    let answerRun: (run: undefined) => void = () => {};
    let answerTrail: (rows: AuditEvent[]) => void = () => {};
    getRunMock.mockReturnValue(new Promise((resolve) => (answerRun = resolve)));
    listAuditMock.mockReturnValue(new Promise((resolve) => (answerTrail = resolve)));
    await mount({ me: unitsOf("payments"), url: "/admin/audit?run_id=run_abcdef12" });

    expect(await screen.findByText("Loading run…")).toBeInTheDocument();
    await act(async () => answerRun(undefined));
    expect(screen.getByText("Loading run…")).toBeInTheDocument();
    expect(screen.queryByText(/Run not found/)).not.toBeInTheDocument();

    await act(async () =>
      answerTrail([ev({ id: "e1", run_id: "run_abcdef12", action: "egress.allow", target: "api.github.com:443" })]),
    );
    expect(await screen.findByText("This run is in your unit")).toBeInTheDocument();
    expect(screen.queryByText(/Run not found/)).not.toBeInTheDocument();
  });

  it("never says Run not found while /me is still held, even once the run read has answered 404", async () => {
    let answerRun: (run: undefined) => void = () => {};
    let answerMe: (me: unknown) => void = () => {};
    let answerTrail: (rows: AuditEvent[]) => void = () => {};
    getRunMock.mockReturnValue(new Promise((resolve) => (answerRun = resolve)));
    whoamiMock.mockReturnValue(new Promise((resolve) => (answerMe = resolve)));
    listAuditMock.mockReturnValue(new Promise((resolve) => (answerTrail = resolve)));
    const { OperatorProvider } = await import("../wardyn/operator-context");
    render(
      <MemoryRouter initialEntries={["/admin/audit?run_id=run_abcdef12"]}>
        <OperatorProvider operator={false} securityOperator={false}>
          <AuditScreen />
        </OperatorProvider>
      </MemoryRouter>,
    );

    expect(await screen.findByText("Loading run…")).toBeInTheDocument();
    await act(async () => answerRun(undefined));
    expect(screen.queryByText(/Run not found/)).not.toBeInTheDocument();

    await act(async () => answerMe(unitsOf("payments")));
    expect(screen.queryByText(/Run not found/)).not.toBeInTheDocument();
    await act(async () =>
      answerTrail([ev({ id: "e1", run_id: "run_abcdef12", action: "egress.allow", target: "api.github.com:443" })]),
    );
    expect(await screen.findByText("This run is in your unit")).toBeInTheDocument();
  });

  it("shows neither empty state while /me has not answered, then the unit one", async () => {
    // The feed answers (empty) while /me is still held: the page must not speak for either empty state yet.
    let answerMe: (me: unknown) => void = () => {};
    let answerFeed: (rows: AuditEvent[]) => void = () => {};
    whoamiMock.mockReturnValue(new Promise((resolve) => (answerMe = resolve)));
    listAuditMock.mockReturnValue(new Promise((resolve) => (answerFeed = resolve)));
    const { OperatorProvider } = await import("../wardyn/operator-context");
    render(
      <MemoryRouter initialEntries={["/admin/audit"]}>
        <OperatorProvider operator={false} securityOperator={false}>
          <AuditScreen />
        </OperatorProvider>
      </MemoryRouter>,
    );

    await waitFor(() => expect(listAuditMock).toHaveBeenCalled());
    await act(async () => answerFeed([]));
    expect(screen.queryByText(AUDIT.MEMBER_FEED_TITLE)).not.toBeInTheDocument();
    expect(screen.queryByText(AUDIT.UNIT_EMPTY_TITLE)).not.toBeInTheDocument();

    await act(async () => answerMe(unitsOf("payments")));
    expect(await screen.findByText(AUDIT.UNIT_EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.queryByText(AUDIT.MEMBER_FEED_TITLE)).not.toBeInTheDocument();
  });
});
