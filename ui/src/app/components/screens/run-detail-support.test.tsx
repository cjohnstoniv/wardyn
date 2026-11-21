/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The run page's support strip: hidden until the setup answer carries support_sessions, the helper's Request help
// dialog, the one banner each party reads, and the line for every way help ends.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { OperatorProvider } from "../wardyn/operator-context";
import { ModelAccessProvider } from "../wardyn/model-access-context";
import { baseStatus } from "../../lib/test-fixtures";
import { makeRun } from "../../../test/factories";
import { setField } from "../../../test/set-field";
import type { RunDetail, SetupStatus } from "../../lib/types";
import type { SupportDoor, SupportGrantView, SupportStatus } from "../../lib/api/support";
import { HttpError } from "../../lib/api/core";
import { aheadByHours } from "../../lib/test-clock";
import { utcClock } from "./run-detail-support-consent";
import { supportEndLine, SUPPORT_REQUEST, type SupportViewer } from "../wardyn/copy/support";

const api = {
  status: vi.fn(),
  request: vi.fn(),
  accept: vi.fn(),
  decline: vi.fn(),
  end: vi.fn(),
};
vi.mock("../../lib/api/support", () => ({ support: { status: (...a: unknown[]) => api.status(...a), request: (...a: unknown[]) => api.request(...a), accept: (...a: unknown[]) => api.accept(...a), decline: (...a: unknown[]) => api.decline(...a), end: (...a: unknown[]) => api.end(...a) } }));

import { RunSupportStrip, useRunSupport } from "./run-detail-support";

const OFFERED = ["terminal", "commands", "files", "forward", "web_apps", "desktop"];
const run = { ...makeRun({ created_by: "maya" }), } as RunDetail;
// Times from the clock: asked six minutes ago, seen two minutes ago, ending in 24 minutes.
const REQUESTED = aheadByHours(-0.1);
const SEEN = aheadByHours(-2 / 60);
const UNTIL = aheadByHours(0.4);

const grant = (o: Partial<SupportGrantView> = {}): SupportGrantView => ({
  grant: "g-1",
  helper: "sam",
  reason: "The build keeps stalling.",
  doors_asked: ["terminal", "commands", "files"],
  requested_at: REQUESTED,
  state: "requested",
  ...o,
});
const active = (o: Partial<SupportGrantView> = {}) =>
  grant({ state: "active", doors: ["terminal", "files"], minutes: 30, expires_at: UNTIL, last_seen_at: SEEN, ...o });

function Page({ r }: { r: RunDetail }) {
  const sup = useRunSupport(r);
  return (
    <>
      <RunSupportStrip run={r} sup={sup} />
      {/* What the run page hands the cockpit (SupportHelperContext), and a read on demand in place of the poll. */}
      <span data-testid="helper-terminal">{String(sup.helperTerminal)}</span>
      <span data-testid="helper-doors">{JSON.stringify(sup.helperDoors)}</span>
      <button type="button" onClick={() => void sup.refresh()}>
        read again
      </button>
    </>
  );
}

function mount(who: { principal: string; operator?: boolean; security?: boolean }, on = true, r: RunDetail = run) {
  const status = baseStatus({ features: { scheduled_runs: false, ...(on ? { support_sessions: true } : {}) } } as Partial<SetupStatus>);
  return render(
    <ModelAccessProvider status={status} onRefresh={() => {}}>
      <OperatorProvider operator={who.operator ?? false} securityOperator={who.security ?? who.operator ?? false} principal={who.principal}>
        <Page r={r} />
      </OperatorProvider>
    </ModelAccessProvider>,
  );
}

const answer = (s: SupportStatus) => api.status.mockResolvedValue({ doors_offered: OFFERED, recorded_doors: ["terminal"], ...s });

beforeEach(() => {
  Object.values(api).forEach((m) => m.mockReset());
  window.localStorage.clear();
});
afterEach(cleanup);

describe("support strip: the switch", () => {
  it("draws nothing and reads nothing when support_sessions is absent from setup", async () => {
    answer({ open: null });
    mount({ principal: "sam", operator: true }, false);
    await Promise.resolve();
    expect(screen.queryByTestId("support-strip")).toBeNull();
    expect(screen.queryByText(SUPPORT_REQUEST.TITLE)).toBeNull();
    expect(api.status).not.toHaveBeenCalled();
  });

  it("offers Request help to a super admin who did not start the run, and to nobody else", async () => {
    answer({ open: null });
    mount({ principal: "sam", operator: true });
    expect(await screen.findByRole("button", { name: SUPPORT_REQUEST.TITLE })).toBeTruthy();
    cleanup();
    mount({ principal: "maya", operator: true }); // the owner
    await waitFor(() => expect(api.status).toHaveBeenCalledTimes(2));
    expect(screen.queryByRole("button", { name: SUPPORT_REQUEST.TITLE })).toBeNull();
    cleanup();
    mount({ principal: "pat", operator: false }); // a member: not a party, reads nothing
    expect(screen.queryByRole("button", { name: SUPPORT_REQUEST.TITLE })).toBeNull();
  });
});

describe("support strip: the helper's request dialog", () => {
  it("sends the ticked doors and the trimmed reason, with no minutes, and Send is off until both are given", async () => {
    answer({ open: null });
    api.request.mockResolvedValue(grant());
    const user = userEvent.setup();
    mount({ principal: "sam", operator: true });
    await user.click(await screen.findByRole("button", { name: SUPPORT_REQUEST.TITLE }));
    const send = screen.getByRole("button", { name: SUPPORT_REQUEST.SEND });
    expect((send as HTMLButtonElement).disabled).toBe(true);
    await user.click(screen.getByRole("checkbox", { name: /Terminal/ }));
    await user.click(screen.getByRole("checkbox", { name: /Commands/ }));
    expect((send as HTMLButtonElement).disabled).toBe(true); // no reason yet
    setField(screen.getByRole("textbox", { name: SUPPORT_REQUEST.REASON }), "  stuck build  ");
    expect(screen.getByTestId("support-reason-count").textContent).toBe("11 / 500");
    await user.click(send);
    expect(api.request).toHaveBeenCalledWith("run-1", ["terminal", "commands"], "stuck build");
  });

  it("says use 500 characters or fewer past 500 and keeps Send off", async () => {
    answer({ open: null });
    const user = userEvent.setup();
    mount({ principal: "sam", operator: true });
    await user.click(await screen.findByRole("button", { name: SUPPORT_REQUEST.TITLE }));
    await user.click(screen.getByRole("checkbox", { name: /Files/ }));
    await user.click(screen.getByRole("textbox", { name: SUPPORT_REQUEST.REASON }));
    await user.paste("é".repeat(501));
    expect(screen.getByText(SUPPORT_REQUEST.TOO_LONG)).toBeTruthy();
    expect((screen.getByRole("button", { name: SUPPORT_REQUEST.SEND }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByTestId("support-reason-count").textContent).toBe("501 / 500");
  });

  it("offers four doors and says so in path mode, and shows the server's refusal in the dialog", async () => {
    answer({ open: null, doors_offered: ["terminal", "commands", "files", "forward"] as never });
    const { HttpError } = await import("../../lib/api/core");
    api.request.mockRejectedValue(new HttpError(409, "this run already has a help request or session open"));
    const user = userEvent.setup();
    mount({ principal: "sam", operator: true });
    await user.click(await screen.findByRole("button", { name: SUPPORT_REQUEST.TITLE }));
    expect(screen.queryByRole("checkbox", { name: /Web apps/ })).toBeNull();
    expect(screen.queryByRole("checkbox", { name: /Desktop/ })).toBeNull();
    expect(screen.getByText(SUPPORT_REQUEST.PATH_MODE)).toBeTruthy();
    await user.click(screen.getByRole("checkbox", { name: /Forward/ }));
    setField(screen.getByRole("textbox", { name: SUPPORT_REQUEST.REASON }), "x");
    await user.click(screen.getByRole("button", { name: SUPPORT_REQUEST.SEND }));
    expect((await screen.findByRole("alert")).textContent).toContain("already has a help request");
  });
});

describe("support strip: the banner", () => {
  it("shows the owner a waiting request with Review request, and the helper Waiting for the owner with Cancel request", async () => {
    answer({ open: grant() });
    mount({ principal: "maya" });
    expect(await screen.findByText("sam asked to help in this run")).toBeTruthy();
    expect(screen.getByText(`Requested ${utcClock(REQUESTED)}. A request nobody answers lapses after 15 minutes.`)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Review request" })).toBeTruthy();
    cleanup();
    api.end.mockResolvedValue(grant({ state: "ended", end_reason: "ended_by_helper" }));
    mount({ principal: "sam", operator: true });
    expect(await screen.findByText("Waiting for maya to answer.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Review request" })).toBeNull();
    await userEvent.setup().click(screen.getByRole("button", { name: "Cancel request" }));
    expect(api.end).toHaveBeenCalledWith("run-1", "g-1");
  });

  it("shows an active session its helper, doors, until and last seen, with End now for owner, helper and security admin", async () => {
    answer({ open: active() });
    for (const [who, title] of [
      [{ principal: "maya" }, "sam is helping in this run"],
      [{ principal: "sam", operator: true }, "You are helping in this run"],
      [{ principal: "sec", operator: true }, "sam is helping in this run"],
    ] as const) {
      mount(who);
      expect(await screen.findByText(title)).toBeTruthy();
      const banner = screen.getByTestId("support-active");
      expect(banner.textContent).toContain("Terminal");
      expect(banner.textContent).toContain("Files");
      expect(banner.textContent).not.toContain("Commands"); // asked, not opened
      expect(banner.textContent).toContain(utcClock(UNTIL));
      expect(banner.textContent).toContain(utcClock(SEEN));
      expect(screen.getByRole("button", { name: "End now" })).toBeTruthy();
      expect(screen.queryByRole("button", { name: "Review request" })).toBeNull();
      cleanup();
    }
  });

  it("ends the grant on End now and reads the state again", async () => {
    const ended = active({ state: "ended", end_reason: "revoked" });
    const full = (s: SupportStatus): SupportStatus => ({ doors_offered: OFFERED as SupportDoor[], recorded_doors: ["terminal"], ...s });
    api.status.mockResolvedValueOnce(full({ open: active() })).mockResolvedValueOnce(full({ open: null, ended }));
    api.end.mockResolvedValue(ended);
    mount({ principal: "maya" });
    await userEvent.setup().click(await screen.findByRole("button", { name: "End now" }));
    expect(api.end).toHaveBeenCalledWith("run-1", "g-1");
    expect(await screen.findByTestId("support-ended")).toBeTruthy();
    await waitFor(() => expect(api.status).toHaveBeenCalledTimes(2));
    expect(screen.queryByRole("button", { name: "End now" })).toBeNull();
    expect(screen.queryByTestId("support-active")).toBeNull();
  });

  it("keeps the confirmed end when the status read fails", async () => {
    const ended = active({ state: "ended", end_reason: "revoked" });
    api.status
      .mockResolvedValueOnce({ doors_offered: OFFERED as SupportDoor[], open: active() })
      .mockRejectedValueOnce(new Error("network down"))
      .mockRejectedValueOnce(new Error("network down"));
    api.end.mockResolvedValue(ended);
    mount({ principal: "maya" });
    await userEvent.setup().click(await screen.findByRole("button", { name: "End now" }));
    await waitFor(() => expect(api.status).toHaveBeenCalledTimes(2));
    expect(await screen.findByTestId("support-ended")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "End now" })).toBeNull();
  });

  it("ignores a status read that started before the end", async () => {
    const ended = active({ state: "ended", end_reason: "revoked" });
    let release: (s: SupportStatus) => void = () => {};
    const held = new Promise<SupportStatus>((res) => {
      release = res;
    });
    api.status
      .mockResolvedValueOnce({ doors_offered: OFFERED as SupportDoor[], open: active() })
      .mockReturnValueOnce(held)
      .mockRejectedValueOnce(new Error("network down"));
    api.end.mockResolvedValue(ended);
    const user = userEvent.setup();
    mount({ principal: "maya" });
    await screen.findByRole("button", { name: "End now" });
    await user.click(screen.getByRole("button", { name: "read again" })); // started before the end
    await user.click(screen.getByRole("button", { name: "End now" }));
    expect(await screen.findByTestId("support-ended")).toBeTruthy();
    await act(async () => release({ doors_offered: OFFERED as SupportDoor[], open: active() })); // the old answer lands late
    expect(screen.getByTestId("support-ended")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "End now" })).toBeNull();
  });

  it("a late end for one grant keeps a different open grant", async () => {
    const endedA = active({ state: "ended", end_reason: "revoked" });
    const b = active({ grant: "g-2", helper: "pat" });
    let release: (g: SupportGrantView) => void = () => {};
    api.end.mockReturnValueOnce(
      new Promise<SupportGrantView>((res) => {
        release = res;
      }),
    );
    api.status
      .mockResolvedValueOnce({ doors_offered: OFFERED as SupportDoor[], open: active() })
      .mockResolvedValueOnce({ doors_offered: OFFERED as SupportDoor[], open: b })
      .mockRejectedValueOnce(new Error("status unavailable"));
    const user = userEvent.setup();
    mount({ principal: "maya" });
    await user.click(await screen.findByRole("button", { name: "End now" })); // ends g-1, response held
    await user.click(screen.getByRole("button", { name: "read again" })); // g-2 is now open
    await waitFor(() => expect(screen.getByTestId("support-active").textContent).toContain("pat"));
    await act(async () => release(endedA));
    await waitFor(() => expect(api.status).toHaveBeenCalledTimes(3));
    expect(screen.queryByTestId("support-ended")).toBeNull();
    expect(screen.getByTestId("support-active").textContent).toContain("pat");
    expect(screen.getByRole("button", { name: "End now" })).toBeTruthy();
  });

  it("a late end keeps a newer ended grant", async () => {
    const endedA = active({ state: "ended", end_reason: "revoked", ended_at: aheadByHours(-0.05) });
    const endedB = grant({ grant: "g-2", state: "ended", end_reason: "lapsed", ended_at: aheadByHours(-0.01) });
    let release: (g: SupportGrantView) => void = () => {};
    api.end.mockReturnValueOnce(
      new Promise<SupportGrantView>((res) => {
        release = res;
      }),
    );
    api.status
      .mockResolvedValueOnce({ doors_offered: OFFERED as SupportDoor[], open: active() })
      .mockResolvedValueOnce({ doors_offered: OFFERED as SupportDoor[], open: null, ended: endedB })
      .mockRejectedValueOnce(new Error("status unavailable"));
    const user = userEvent.setup();
    mount({ principal: "maya" });
    await user.click(await screen.findByRole("button", { name: "End now" })); // ends g-1, response held
    await user.click(screen.getByRole("button", { name: "read again" })); // g-2 came and went
    expect((await screen.findByTestId("support-ended")).textContent).toContain("Nobody answered");
    await act(async () => release(endedA));
    await waitFor(() => expect(api.status).toHaveBeenCalledTimes(3));
    expect(screen.getByTestId("support-ended").textContent).toContain("Nobody answered");
    expect(screen.getByTestId("support-ended").textContent).not.toContain("you ended it");
  });

  it("a late end orders ended grants within one millisecond", async () => {
    // Same millisecond, microseconds apart: the later ended_at wins whichever grant it belongs to.
    const cases = [
      { shown: "2026-01-01T00:00:00.400090Z", confirmed: "2026-01-01T00:00:00.400010Z", line: "Nobody answered" },
      { shown: "2026-01-01T00:00:00.400010Z", confirmed: "2026-01-01T00:00:00.400090Z", line: "you ended it" },
    ];
    for (const c of cases) {
      Object.values(api).forEach((m) => m.mockReset());
      const endedA = active({ state: "ended", end_reason: "revoked", ended_at: c.confirmed });
      const endedB = grant({ grant: "g-2", state: "ended", end_reason: "lapsed", ended_at: c.shown });
      let release: (g: SupportGrantView) => void = () => {};
      api.end.mockReturnValueOnce(
        new Promise<SupportGrantView>((res) => {
          release = res;
        }),
      );
      api.status
        .mockResolvedValueOnce({ doors_offered: OFFERED as SupportDoor[], open: active() })
        .mockResolvedValueOnce({ doors_offered: OFFERED as SupportDoor[], open: null, ended: endedB })
        .mockRejectedValueOnce(new Error("status unavailable"));
      const user = userEvent.setup();
      mount({ principal: "maya" });
      await user.click(await screen.findByRole("button", { name: "End now" }));
      await user.click(screen.getByRole("button", { name: "read again" }));
      expect((await screen.findByTestId("support-ended")).textContent).toContain("Nobody answered");
      await act(async () => release(endedA));
      await waitFor(() => expect(api.status).toHaveBeenCalledTimes(3));
      expect(screen.getByTestId("support-ended").textContent).toContain(c.line);
      cleanup();
    }
  });

  it("shows the ended line with Close, and Close is remembered for that grant", async () => {
    answer({ open: null, ended: active({ state: "ended", end_reason: "expired" }) });
    const user = userEvent.setup();
    mount({ principal: "maya" });
    expect(await screen.findByText("Help ended: the time ran out.")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.queryByText("Help ended: the time ran out.")).toBeNull();
    cleanup();
    mount({ principal: "maya" });
    await waitFor(() => expect(api.status).toHaveBeenCalledTimes(2));
    expect(screen.queryByText("Help ended: the time ran out.")).toBeNull();
  });

  it("shows the owner no line for a request they declined", async () => {
    answer({ open: null, ended: grant({ state: "ended", end_reason: "declined" }) });
    mount({ principal: "maya" });
    await waitFor(() => expect(api.status).toHaveBeenCalled());
    expect(screen.queryByTestId("support-ended")).toBeNull();
  });
});

describe("support strip: where Request help is never offered (the server always refuses)", () => {
  it.each<[string, Partial<RunDetail>]>([
    ["an operator-owned run", { operator_owned: true }],
    ["a service identity's run", { created_by: "service:nightly" }],
    ["a background run", { experience: "background" } as Partial<RunDetail>],
    ["a lost run", { lost_at: aheadByHours(-0.5) }],
    ["a sign-in run", { task: "harness login" }],
    ["a workspace record run", { task: "workspace record" }],
    ["a workspace verify run", { task: "workspace verify" }],
    ["a run that is not running", { state: "SUCCEEDED" }],
  ])("not on %s", async (_name, over) => {
    answer({ open: null });
    mount({ principal: "sam", operator: true }, true, { ...run, ...over } as RunDetail);
    await waitFor(() => expect(api.status).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: SUPPORT_REQUEST.TITLE })).toBeNull();
  });

  it.each([
    ["the admin token, which is no person", "admin-token"],
    ["a session with no principal", ""],
  ])("not to %s", async (_name, principal) => {
    answer({ open: null });
    mount({ principal, operator: true });
    await waitFor(() => expect(api.status).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: SUPPORT_REQUEST.TITLE })).toBeNull();
  });

  it("is still offered on the same run with none of those facts (the control row)", async () => {
    answer({ open: null });
    mount({ principal: "sam", operator: true }, true, { ...run, task: "fix the build" } as RunDetail);
    expect(await screen.findByRole("button", { name: SUPPORT_REQUEST.TITLE })).toBeTruthy();
  });
});

describe("support strip: a security admin who is not a super admin", () => {
  it("sees the active banner with End now and is never offered Request help", async () => {
    answer({ open: active() });
    mount({ principal: "sec", operator: false, security: true });
    expect(await screen.findByText("sam is helping in this run")).toBeTruthy();
    expect(screen.getByRole("button", { name: "End now" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: SUPPORT_REQUEST.TITLE })).toBeNull();
    expect(screen.queryByRole("button", { name: "Review request" })).toBeNull();
  });

  it("gets End now on a request still waiting, which ends it with the same call", async () => {
    answer({ open: grant() });
    api.end.mockResolvedValue(grant({ state: "ended", end_reason: "ended_by_security" }));
    mount({ principal: "sec", operator: false, security: true });
    expect(await screen.findByText("sam asked to help in this run")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Review request" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Cancel request" })).toBeNull();
    await userEvent.setup().click(screen.getByRole("button", { name: "End now" }));
    expect(api.end).toHaveBeenCalledWith("run-1", "g-1");
  });
});

describe("support strip: the doors the run page's entry points read", () => {
  const doors = () => screen.getByTestId("helper-doors").textContent;

  it("hands the open grant's doors to the helper and null to everyone else", async () => {
    answer({ open: active({ doors: ["files", "web_apps"] }) });
    mount({ principal: "sam", operator: true });
    await waitFor(() => expect(doors()).toBe('["files","web_apps"]'));
    expect(screen.getByTestId("helper-terminal").textContent).toBe("false");
    cleanup();
    for (const who of [{ principal: "maya" }, { principal: "other", operator: true }, { principal: "sec", operator: false, security: true }]) {
      answer({ open: active({ doors: ["files", "web_apps"] }) });
      mount(who);
      await waitFor(() => expect(api.status).toHaveBeenCalled());
      await screen.findByTestId("helper-doors");
      expect(doors()).toBe("null");
      cleanup();
      api.status.mockReset();
    }
  });

  it("is null for a grant that is only requested or has ended", async () => {
    answer({ open: grant() });
    mount({ principal: "sam", operator: true });
    await screen.findByText("Cancel request");
    expect(doors()).toBe("null");
    cleanup();
    answer({ open: null, ended: grant({ state: "ended", end_reason: "expired", doors: ["terminal"] }) });
    mount({ principal: "sam", operator: true });
    await screen.findByText("Help ended: the time ran out.");
    expect(doors()).toBe("null");
  });
});

describe("support strip: a failed read keeps what was shown", () => {
  it("a 500 keeps the helper's terminal open and the owner's banner up; a 404 clears both", async () => {
    answer({ open: active() });
    mount({ principal: "sam", operator: true });
    await waitFor(() => expect(screen.getByTestId("helper-terminal").textContent).toBe("true"));
    api.status.mockRejectedValueOnce(new HttpError(500, "internal error"));
    await act(async () => screen.getByRole("button", { name: "read again" }).click());
    expect(api.status).toHaveBeenCalledTimes(2);
    expect(screen.getByTestId("helper-terminal").textContent).toBe("true");
    expect(screen.getByText("You are helping in this run")).toBeTruthy();
    cleanup();

    answer({ open: active() });
    mount({ principal: "maya" });
    expect(await screen.findByText("sam is helping in this run")).toBeTruthy();
    api.status.mockRejectedValueOnce(new HttpError(500, "internal error"));
    await act(async () => screen.getByRole("button", { name: "read again" }).click());
    expect(screen.getByText("sam is helping in this run")).toBeTruthy();
    expect(screen.getByRole("button", { name: "End now" })).toBeTruthy();

    api.status.mockRejectedValueOnce(new HttpError(404, "run not found", "run_not_found"));
    await act(async () => screen.getByRole("button", { name: "read again" }).click());
    await waitFor(() => expect(screen.queryByText("sam is helping in this run")).toBeNull());
  });
});

describe("supportEndLine: every end reason, for each viewer (the board's table)", () => {
  const was = (end_reason: string, accepted: boolean) => ({ end_reason, ...(accepted ? { doors: ["terminal" as const], minutes: 15, expires_at: "x" } : {}) });
  const rows: [string, boolean, Record<SupportViewer, string | null>][] = [
    ["expired", true, { owner: "Help ended: the time ran out.", helper: "Help ended: the time ran out.", security: "Help ended: the time ran out." }],
    ["revoked", true, { owner: "Help ended: you ended it.", helper: "Help ended: the owner ended it.", security: "Help ended: the owner ended it." }],
    ["run_ended", true, { owner: "Help ended: the run stopped or was lost.", helper: "Help ended: the run stopped or was lost.", security: "Help ended: the run stopped or was lost." }],
    ["declined", false, { owner: null, helper: "The owner declined this request.", security: "The owner declined this request." }],
    ["lapsed", false, { owner: "The request lapsed. Nobody answered in 15 minutes.", helper: "The request lapsed. Nobody answered in 15 minutes.", security: "The request lapsed. Nobody answered in 15 minutes." }],
    ["ended_by_helper", true, { owner: "Help ended: the helper ended it.", helper: "Help ended: you ended it.", security: "Help ended: the helper ended it." }],
    ["ended_by_helper", false, { owner: "The helper cancelled this request.", helper: "You cancelled this request.", security: "The helper cancelled this request." }],
    ["ended_by_security", true, { owner: "Help ended: a security admin ended it.", helper: "Help ended: a security admin ended it.", security: "Help ended: a security admin ended it." }],
    ["helper_signed_out", true, { owner: "Help ended: the helper's sign-in ended.", helper: "Help ended: your sign-in ended.", security: "Help ended: the helper's sign-in ended." }],
    ["owner_revoked", true, { owner: "Help ended: your account was revoked or deactivated.", helper: "Help ended: the owner's account was revoked or deactivated.", security: "Help ended: the owner's account was revoked or deactivated." }],
    ["support_off", true, { owner: "Help ended: support sessions were turned off.", helper: "Help ended: support sessions were turned off.", security: "Help ended: support sessions were turned off." }],
  ];
  for (const [reason, accepted, want] of rows) {
    it(`${reason} (${accepted ? "was accepted" : "never accepted"})`, () => {
      for (const viewer of ["owner", "helper", "security"] as const) expect(supportEndLine(was(reason, accepted), viewer)).toBe(want[viewer]);
    });
  }
  it("says nothing for a reason this console does not know", () => {
    expect(supportEndLine({ end_reason: "something_new" }, "owner")).toBeNull();
  });
});
