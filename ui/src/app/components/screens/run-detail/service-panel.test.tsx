/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Mock r15 T1-c: every state the Service panel can show, in the mock's words.
import { describe, expect, it } from "vitest";
import { render, screen, within } from "@testing-library/react";
import type { RunDetail, RunService, RunServiceProbe, RunServiceStatus } from "../../../lib/types";
import { SERVICE_PANEL as T } from "../../wardyn/copy/run-service";
import { ServicePanel } from "./service-panel";
import { keptUntilText } from "./run-ends-row";
import { shortDate } from "../../../lib/format";

const NOW = Date.parse("2020-10-29T12:00:00Z");
const ENDS = "2020-11-28T09:40:00Z";
const CAP = new Date(Date.parse(ENDS) + 30 * 86_400_000).toISOString();
const WITH_PATH: RunService = { restart: "on_failure", port: 8080, health: { path: "/healthz", interval_sec: 15, failure_threshold: 3 } };
const status = (over: Partial<RunServiceStatus> & Pick<RunServiceStatus, "health">): RunServiceStatus => ({
  restarts: 0, observed_at: new Date(NOW - 40_000).toISOString(), ...over,
});

function show(service: RunService | undefined, service_status?: RunServiceStatus, over: Partial<RunDetail> = {}) {
  const run = { id: "r1", state: "RUNNING", ends_at: ENDS, lifetime_cap_at: CAP, service, service_status, ...over } as RunDetail;
  return render(<ServicePanel run={run} now={NOW} />);
}
const row = (label: string) => (screen.getByText(label, { selector: "dt" }).nextElementSibling as HTMLElement);

describe("ServicePanel", () => {
  it("healthy: every row, the lease and the footnote, and no reason", () => {
    show(WITH_PATH, status({ health: "healthy" }));
    expect(screen.getByRole("heading", { name: T.TITLE })).toBeInTheDocument();
    expect(row(T.RESTART)).toHaveTextContent("When it fails");
    expect(row(T.CHECK)).toHaveTextContent("GET /healthz on port 8080 every 15 s");
    expect(row(T.HEALTH)).toHaveTextContent("Healthy · checked 40 s ago");
    expect(row(T.RESTARTS)).toHaveTextContent(/^0$/);
    expect(row(T.LEASE)).toHaveTextContent(`Renewed while healthy · ends ${keptUntilText(ENDS, NOW)} if it stays unhealthy`);
    // A lease a month out reads as the header's Ends chip does, with its date.
    expect(row(T.LEASE)).toHaveTextContent(shortDate(ENDS));
    expect(screen.getByText(T.FOOTNOTE)).toBeInTheDocument();
    expect(screen.queryByRole("note")).not.toBeInTheDocument();
  });

  it("an uncapped service shows its plain end and the no-cap note, with no renewal clause", () => {
    show(WITH_PATH, status({ health: "healthy" }), { lifetime_cap_at: undefined });
    expect(row(T.LEASE)).toHaveTextContent(new RegExp(`^${keptUntilText(ENDS, NOW).replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}$`));
    expect(row(T.LEASE)).not.toHaveTextContent("Renewed while healthy");
    expect(screen.getByRole("note")).toHaveTextContent(T.NO_CAP);
  });

  it("starting with no read yet: waits for the first check", () => {
    show(WITH_PATH, undefined);
    expect(row(T.HEALTH)).toHaveTextContent("Starting · waiting for the first check");
    expect(screen.queryByRole("note")).not.toBeInTheDocument();
  });

  it("unhealthy and restarting: the exit reason and the restart count with the last exit code", () => {
    show(WITH_PATH, status({ health: "unhealthy", reason: "restarting", restarts: 4, last_exit_code: 1, observed_at: new Date(NOW - 12_000).toISOString() }));
    expect(row(T.HEALTH)).toHaveTextContent("Unhealthy · checked 12 s ago");
    expect(row(T.RESTARTS)).toHaveTextContent("4 · last exit code 1");
    expect(screen.getByRole("note")).toHaveTextContent(T.REASON_RESTARTING);
  });

  it("a failing check names the threshold when it restarts, and says only that it is failing when it never does", () => {
    const { unmount } = show({ ...WITH_PATH, health: { ...WITH_PATH.health!, failure_threshold: 5 } }, status({ health: "unhealthy", reason: "probe_failing", restarts: 3, last_exit_code: 1 }));
    expect(screen.getByRole("note")).toHaveTextContent("The health check failed 5 times in a row, so the service was restarted.");
    unmount();
    show({ ...WITH_PATH, restart: "never" }, status({ health: "unhealthy", reason: "probe_failing" }));
    expect(row(T.RESTART)).toHaveTextContent("Never");
    expect(screen.getByRole("note")).toHaveTextContent(/^The health check is failing\.$/);
  });

  it("health unknown: an old image and an offline runner each say so, neutral", () => {
    const { unmount } = show(WITH_PATH, status({ health: "unknown", reason: "supervisor_missing", observed_at: new Date(NOW - 60_000).toISOString() }));
    expect(row(T.HEALTH)).toHaveTextContent("Health unknown · checked 1 min ago");
    expect(screen.getByRole("note")).toHaveTextContent(T.REASON_OLD_IMAGE);
    expect(screen.getByRole("note").className).not.toContain("warning");
    unmount();
    show(WITH_PATH, status({ health: "unknown", reason: "unreachable", restarts: 2, last_exit_code: 1 }));
    expect(screen.getByRole("note")).toHaveTextContent(T.REASON_UNREACHABLE);
  });

  it("no health path checks the port; no port has no check; a run with no end has no lease to renew", () => {
    const { unmount } = show({ restart: "on_failure", port: 8080, health: { interval_sec: 15, failure_threshold: 3 } }, status({ health: "healthy" }), { ends_at: null });
    expect(row(T.CHECK)).toHaveTextContent("Port 8080 accepts connections, every 15 s");
    expect(row(T.LEASE)).toHaveTextContent(/^No end$/);
    unmount();
    show({ restart: "on_failure" }, status({ health: "healthy" }));
    expect(row(T.CHECK)).toHaveTextContent(/^None$/);
  });

  it("a port with no health block runs no probe, so the check reads None", () => {
    show({ restart: "on_failure", port: 8080 }, status({ health: "healthy" }));
    expect(row(T.CHECK)).toHaveTextContent(/^None$/);
  });

  it("draws nothing for a run that is not a live service", () => {
    const { container, unmount } = show(undefined);
    expect(container).toBeEmptyDOMElement();
    unmount();
    expect(show(WITH_PATH, status({ health: "healthy" }), { state: "FAILED" }).container).toBeEmptyDOMElement();
  });

  it("keeps the footnote on every state, because health is reported from inside the sandbox", () => {
    for (const health of ["starting", "healthy", "unhealthy", "unknown"] as const) {
      const { unmount } = show(WITH_PATH, status({ health }));
      expect(within(screen.getByTestId("service-panel")).getByText(T.FOOTNOTE)).toBeInTheDocument();
      unmount();
    }
  });

  describe("Wardyn's check", () => {
    const PROBED: RunService = { ...WITH_PATH, health: { ...WITH_PATH.health!, probe: "control_plane" } };
    const ago40 = new Date(NOW - 40_000).toISOString();
    const reading = (over: Partial<RunServiceProbe> & Pick<RunServiceProbe, "health">): RunServiceProbe => ({ failures: 0, observed_at: ago40, ...over });
    // Started an hour ago, so a missing reading is past the supervisor's grace unless a test says otherwise.
    const started = { created_at: new Date(NOW - 3_600_000).toISOString() };
    const showProbe = (sandbox: Partial<RunServiceStatus> & Pick<RunServiceStatus, "health">, probe?: RunServiceProbe, over: Partial<RunDetail> = {}) =>
      show(PROBED, status({ ...sandbox, probe }), { ...started, ...over });
    const ends = keptUntilText(ENDS, NOW);
    const notes = () => screen.queryAllByRole("note").map((n) => n.textContent);

    it("passing: Sandbox says, then Wardyn's check with what it asked, both renew, and the reworded footnote", () => {
      showProbe({ health: "healthy" }, reading({ health: "healthy", http_status: 200 }));
      expect(screen.queryByText(T.HEALTH, { selector: "dt" })).not.toBeInTheDocument();
      expect(row(T.SANDBOX_SAYS)).toHaveTextContent("Healthy · checked 40 s ago");
      expect(row(T.PROBE)).toHaveTextContent("Healthy · checked 40 s agoGET /healthz answered 200.");
      expect(row(T.LEASE)).toHaveTextContent(`Renewed while both checks say healthy or starting · ends ${ends} if renewal stops`);
      expect(screen.getByText(T.FOOTNOTE_PROBE)).toBeInTheDocument();
      expect(screen.queryByText(T.FOOTNOTE)).not.toBeInTheDocument();
      expect(notes()).toEqual([]);
    });

    it("one failure under the threshold: the chip stays and the line says why and how many it takes", () => {
      showProbe({ health: "healthy" }, reading({ health: "healthy", reason: "bad_status", http_status: 503, failures: 1 }));
      expect(row(T.PROBE)).toHaveTextContent("Healthy · checked 40 s agoThe last check failed: GET /healthz answered 503. Unhealthy after 3 in a row.");
      expect(row(T.LEASE)).toHaveTextContent("Renewed while both checks say healthy or starting");
    });

    it("the sandbox says healthy and the check fails: the note, and the lease is not renewed now", () => {
      showProbe({ health: "healthy" }, reading({ health: "unhealthy", reason: "bad_status", http_status: 503, failures: 3 }));
      expect(row(T.PROBE)).toHaveTextContent("Unhealthy · checked 40 s agoGET /healthz answered 503.");
      expect(row(T.LEASE)).toHaveTextContent(new RegExp(`^Not renewed now · ends ${ends.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}$`));
      expect(notes()).toEqual([T.DISAGREE_PROBE_FAILS(8080)]);
    });

    it("the sandbox says unhealthy and the check passes: its own reason first, then the note", () => {
      show({ ...PROBED, restart: "never" }, status({ health: "unhealthy", reason: "probe_failing", observed_at: new Date(NOW - 25_000).toISOString(), probe: reading({ health: "healthy", http_status: 200 }) }), started);
      expect(notes()).toEqual([T.REASON_PROBE_FAILING, T.DISAGREE_SANDBOX_FAILS(8080)]);
      expect(row(T.LEASE)).toHaveTextContent("Not renewed now");
    });

    it("both failing agree: no disagreement note, the sandbox's reason and the check's finding", () => {
      showProbe({ health: "unhealthy", reason: "restarting" }, reading({ health: "unhealthy", reason: "refused", failures: 3 }));
      expect(notes()).toEqual([T.REASON_RESTARTING]);
      expect(row(T.PROBE)).toHaveTextContent("Port 8080 refused the connection.");
    });

    it("pending: the check waits for its first run, starting inside the grace and renewing", () => {
      showProbe({ health: "healthy" }, undefined, { created_at: new Date(NOW - 60_000).toISOString() });
      expect(row(T.PROBE)).toHaveTextContent(/^Starting · waiting for the first check$/);
      expect(row(T.LEASE)).toHaveTextContent("Renewed while both checks say healthy or starting");
    });

    it("a check with no reading past the grace reads Health unknown, as renewal reads it, so the lease says not now", () => {
      showProbe({ health: "healthy" }, undefined);
      expect(row(T.PROBE)).toHaveTextContent(new RegExp(`^Health unknown ${T.PROBE_NONE_YET.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}$`));
      expect(row(T.PROBE)).not.toHaveTextContent(T.WAITING_FIRST_CHECK);
      expect(row(T.LEASE)).toHaveTextContent("Not renewed now");
    });

    it("paused: both rows say they are not checked while paused, and the lease is not renewed now", () => {
      showProbe({ health: "unknown", reason: "paused" }, reading({ health: "unknown", reason: "paused" }));
      expect(row(T.PROBE)).toHaveTextContent(T.PAUSED);
      expect(notes()).toEqual([T.PAUSED]);
      expect(row(T.LEASE)).toHaveTextContent("Not renewed now");
    });

    it("stale or could not run: Health unknown with the offline sentence", () => {
      const { unmount } = showProbe({ health: "healthy" }, reading({ health: "unknown", reason: "unreachable" }));
      expect(row(T.PROBE)).toHaveTextContent("Health unknown · checked 40 s ago" + T.REASON_UNREACHABLE);
      expect(row(T.LEASE)).toHaveTextContent("Not renewed now");
      unmount();
      showProbe({ health: "healthy" }, reading({ health: "unknown", reason: "not_run" }));
      expect(row(T.PROBE)).toHaveTextContent(T.REASON_UNREACHABLE);
    });

    it("every finding in its own words", () => {
      for (const [p, text] of [
        [reading({ health: "unhealthy", reason: "timeout", failures: 3 }), T.PROBE_TIMEOUT],
        [reading({ health: "unhealthy", reason: "bad_answer", failures: 3 }), T.PROBE_BAD_ANSWER],
        [reading({ health: "unhealthy", reason: "refused", failures: 3 }), T.PROBE_REFUSED(8080)],
      ] as const) {
        const { unmount } = showProbe({ health: "healthy" }, p);
        expect(row(T.PROBE)).toHaveTextContent(text);
        unmount();
      }
      show({ ...PROBED, health: { interval_sec: 15, failure_threshold: 3, probe: "control_plane" } }, status({ health: "healthy", probe: reading({ health: "healthy" }) }), started);
      expect(row(T.PROBE)).toHaveTextContent(T.PROBE_TCP(8080));
    });

    it("a service started without the check has no row, no rename and today's lease and footnote", () => {
      show(WITH_PATH, status({ health: "healthy" }));
      expect(screen.queryByText(T.PROBE, { selector: "dt" })).not.toBeInTheDocument();
      expect(screen.queryByText(T.SANDBOX_SAYS, { selector: "dt" })).not.toBeInTheDocument();
      expect(row(T.LEASE)).toHaveTextContent("Renewed while healthy");
      expect(screen.getByText(T.FOOTNOTE)).toBeInTheDocument();
    });

    it("an uncapped service with the check keeps its plain end", () => {
      showProbe({ health: "healthy" }, reading({ health: "healthy" }), { lifetime_cap_at: undefined });
      expect(row(T.LEASE)).toHaveTextContent(new RegExp(`^${ends.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}$`));
    });
  });

  it("paused with the check off: the sandbox row's paused sentence", () => {
    show(WITH_PATH, status({ health: "unknown", reason: "paused" }));
    expect(screen.getByRole("note")).toHaveTextContent(T.PAUSED);
  });
});
