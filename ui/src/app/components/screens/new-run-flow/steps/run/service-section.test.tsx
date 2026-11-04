/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Board PROBE-1, New Run: the switch for Wardyn's own check, off by default, disabled until a port is named, and the
// server's refusal of a service field shown as it said it.
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import type { RunService } from "../../../../../lib/types/runs";
import { SERVICE_FORM as T } from "../../../../wardyn/copy/run-service";
import { ServiceSection } from "./service-section";

const health = { path: "/healthz", interval_sec: 15, failure_threshold: 3 };

function show(service: RunService, refusal?: string) {
  const onProbe = vi.fn();
  render(<ServiceSection service={service} onKeepRunning={vi.fn()} onRestart={vi.fn()} onPort={vi.fn()} onHealth={vi.fn()} onProbe={onProbe} refusal={refusal} />);
  return { onProbe, probe: screen.getByRole("switch", { name: T.PROBE_LABEL }) };
}

describe("ServiceSection: Wardyn's check", () => {
  it("is off by default and says what it does; switching it calls onProbe", () => {
    const { onProbe, probe } = show({ restart: "on_failure", port: 8080, health });
    expect(probe).toHaveAttribute("aria-checked", "false");
    expect(probe).toBeEnabled();
    expect(probe).toHaveAccessibleDescription(T.PROBE_HELP);
    fireEvent.click(probe);
    expect(onProbe).toHaveBeenCalledWith(true);
  });

  it("reads on from the draft's health check", () => {
    const { probe } = show({ restart: "on_failure", port: 8080, health: { ...health, probe: "control_plane" } });
    expect(probe).toHaveAttribute("aria-checked", "true");
  });

  it("is disabled with the port sentence until a port is named", () => {
    const { probe } = show({ restart: "on_failure", health: { ...health, probe: "control_plane" } });
    expect(probe).toBeDisabled();
    expect(probe).toHaveAttribute("aria-checked", "false");
    expect(probe).toHaveAccessibleDescription(T.PROBE_NEEDS_PORT);
  });

  it("shows the server's refusal raw under the section", () => {
    show({ restart: "on_failure", port: 8080, health }, 'service.health.probe: invalid "tcp"');
    expect(screen.getByRole("alert")).toHaveTextContent(/^service\.health\.probe: invalid "tcp"$/);
  });
});
