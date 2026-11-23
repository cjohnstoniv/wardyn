/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { HttpError } from "../../lib/api/core";
import type { RunSchedule } from "../../lib/api/run-schedules";
import { PAUSE_REASON, RESUME_OFF_SENTENCE, RUN_SCHEDULE } from "../wardyn/copy/run-schedules";

const api = vi.hoisted(() => ({ list: vi.fn(), pause: vi.fn(), resume: vi.fn(), remove: vi.fn(), create: vi.fn() }));
vi.mock("../../lib/api/run-schedules", () => ({ runSchedules: api }));
const shell = vi.hoisted(() => ({ on: true, refresh: vi.fn() }));
vi.mock("../wardyn/model-access-context", () => ({
  useShellSetupStatus: () => ({ status: { features: { scheduled_runs: shell.on } }, refresh: shell.refresh }),
}));
const mode = vi.hoisted(() => ({ view: "user" as "user" | "admin" }));
vi.mock("../wardyn/console-view", () => ({ useConsoleMode: () => mode.view }));

import { SchedulesScreen } from "./schedules";

const active: RunSchedule = {
  id: "s-1", name: "Nightly bump", every: "weekdays", time: "09:00", time_zone: "Europe/London", next_at: "2026-11-30T09:00:00Z",
  last: { due_at: "2026-11-27T09:00:00Z", run_id: "r-1", outcome: "booked" },
};
const paused: RunSchedule = {
  id: "s-2", name: "Docs sync", every: "weekly", weekday: 5, time: "16:00", time_zone: "UTC", next_at: "2026-12-04T16:00:00Z",
  paused: { at: "2026-11-21T00:00:00Z", reason: "by_owner" },
};

function show() {
  return render(
    <MemoryRouter>
      <SchedulesScreen />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  shell.on = true;
  mode.view = "user";
  api.list.mockResolvedValue([active, paused]);
});

describe("SchedulesScreen", () => {
  it("lists each schedule's rule, next start in its zone, last start and state, and the booked-start note", async () => {
    show();
    expect(await screen.findByText("Every weekday at 09:00")).toBeInTheDocument();
    expect(screen.getByText("Mon 30 Nov, 09:00 (Europe/London)")).toBeInTheDocument();
    expect(screen.getByText("Fri 27 Nov, 09:00")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open the last run of Nightly bump" })).toHaveAttribute("href", "/runs/r-1");
    expect(screen.getByText("Every week on Friday at 16:00")).toBeInTheDocument();
    expect(screen.getByText(PAUSE_REASON.by_owner.member)).toBeInTheDocument();
    expect(screen.getByText(RUN_SCHEDULE.BOOKED_NOTE)).toBeInTheDocument();
    expect(api.list).toHaveBeenCalledWith(false);
  });

  it("says the empty state", async () => {
    api.list.mockResolvedValue([]);
    show();
    expect(await screen.findByText(RUN_SCHEDULE.EMPTY_TITLE)).toBeInTheDocument();
    expect(screen.getByText(RUN_SCHEDULE.EMPTY_BODY)).toBeInTheDocument();
  });

  it("keeps focus on the swapped button after Pause", async () => {
    api.pause.mockResolvedValue({ ...active, paused: { at: "2026-11-23T00:00:00Z", reason: "by_owner" } });
    show();
    await userEvent.click(await screen.findByRole("button", { name: "Pause Nightly bump" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Resume Nightly bump" })).toHaveFocus());
  });

  it("shows a refused Resume's sentence under its row, reads the switch again, and keeps the sentence in the off form", async () => {
    api.resume.mockRejectedValue(new HttpError(422, RESUME_OFF_SENTENCE, "scheduled_runs_off"));
    shell.refresh.mockImplementation(() => {
      shell.on = false;
    });
    const view = show();
    await userEvent.click(await screen.findByRole("button", { name: "Resume Docs sync" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(RESUME_OFF_SENTENCE);
    expect(shell.refresh).toHaveBeenCalled();
    view.rerender(
      <MemoryRouter>
        <SchedulesScreen />
      </MemoryRouter>,
    );
    expect(screen.getByRole("alert")).toHaveTextContent(RESUME_OFF_SENTENCE);
    expect(screen.getByText(RUN_SCHEDULE.OFF_BANNER)).toBeInTheDocument();
    const resume = screen.getByRole("button", { name: "Resume Docs sync" });
    expect(resume).toHaveAttribute("aria-disabled", "true");
    expect(resume).toHaveClass("hatch-unavailable");
    await userEvent.click(resume);
    expect(api.resume).toHaveBeenCalledTimes(1);
  });

  it("while scheduled runs are off: Resume is unavailable with its reason, the pause sentence drops its instruction, and Next start is a dash", async () => {
    shell.on = false;
    show();
    const resume = await screen.findByRole("button", { name: "Resume Docs sync" });
    expect(resume).toHaveAttribute("aria-disabled", "true");
    expect(resume).toHaveAccessibleDescription(RUN_SCHEDULE.RESUME_OFF_REASON);
    expect(screen.getByText(PAUSE_REASON.by_owner.off)).toBeInTheDocument();
    expect(screen.queryByText("Mon 30 Nov, 09:00 (Europe/London)")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Pause Nightly bump" })).toBeEnabled();
  });

  it("maps a refused start's reason, and shows an unmapped one as its token", async () => {
    api.list.mockResolvedValue([
      { ...paused, id: "a", name: "A", paused: { at: "x", reason: "refused:run_schedule_limit" } },
      { ...paused, id: "b", name: "B", paused: { at: "x", reason: "refused:example_reason" } },
    ]);
    show();
    expect(await screen.findByText(/Paused because a start was refused: You have 20 schedules, the most allowed\. Delete one first\. Resume it once that is fixed\./)).toBeInTheDocument();
    expect(screen.getByText("example_reason")).toBeInTheDocument();
  });

  it("admin view: the owner column, no Resume, the admin sentences and per-row names with the owner", async () => {
    mode.view = "admin";
    api.list.mockResolvedValue([{ ...active, owner: "sub-alex" }, { ...paused, owner: "sub-sam" }]);
    show();
    expect(await screen.findByText("sub-alex")).toBeInTheDocument();
    expect(api.list).toHaveBeenCalledWith(true);
    expect(screen.queryByRole("button", { name: /^Resume/ })).not.toBeInTheDocument();
    expect(screen.getByText(PAUSE_REASON.by_owner.admin)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Delete Docs sync by sub-sam" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open the last run of Nightly bump by sub-alex" })).toHaveAttribute("href", "/admin/runs/r-1");
    expect(screen.getByText(RUN_SCHEDULE.BOOKED_NOTE_ADMIN)).toBeInTheDocument();
  });

  it("Delete confirms first, with focus on Delete schedule, and removes the row", async () => {
    api.remove.mockResolvedValue(undefined);
    show();
    await userEvent.click(await screen.findByRole("button", { name: "Delete Docs sync" }));
    const confirm = await screen.findByRole("button", { name: RUN_SCHEDULE.DELETE_CONFIRM });
    await waitFor(() => expect(confirm).toHaveFocus());
    expect(screen.getByText(`${RUN_SCHEDULE.DELETE_BODY} ${RUN_SCHEDULE.BOOKED_NOTE}`)).toBeInTheDocument();
    await userEvent.click(confirm);
    await waitFor(() => expect(screen.queryByText("Docs sync")).not.toBeInTheDocument());
    expect(api.remove).toHaveBeenCalledWith("s-2");
  });
});
