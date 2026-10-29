/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Mock r15 T1-c: every state the Service panel can show, in the mock's words.
import { describe, expect, it } from "vitest";
import { render, screen, within } from "@testing-library/react";
import type { RunDetail, RunService, RunServiceStatus } from "../../../lib/types";
import { SERVICE_PANEL as T } from "../../wardyn/copy/run-service";
import { ServicePanel } from "./service-panel";
import { keptUntilText } from "./run-ends-row";
import { shortDate } from "../../../lib/format";

const NOW = Date.parse("2020-10-29T12:00:00Z");
const ENDS = "2020-11-28T09:40:00Z";
const WITH_PATH: RunService = { restart: "on_failure", port: 8080, health: { path: "/healthz", interval_sec: 15, failure_threshold: 3 } };
const status = (over: Partial<RunServiceStatus> & Pick<RunServiceStatus, "health">): RunServiceStatus => ({
  restarts: 0, observed_at: new Date(NOW - 40_000).toISOString(), ...over,
});

function show(service: RunService | undefined, service_status?: RunServiceStatus, over: Partial<RunDetail> = {}) {
  const run = { id: "r1", state: "RUNNING", ends_at: ENDS, service, service_status, ...over } as RunDetail;
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
});
