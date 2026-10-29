/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NR_RUN } from "../../../../wardyn/copy/new-run/run";
import type { UIApp } from "../../../../../lib/types/runs";
import { PortsSection } from "./ports-section";

afterEach(cleanup);

// The approved mock (r16 T2-a, layout only): the strings are spelled out so a reword fails.
describe("Ports canon strings", () => {
  it("the section help and the checkbox with its help", () => {
    expect(NR_RUN.PORTS_LABEL).toBe("Ports");
    expect(NR_RUN.PORTS_HELP).toBe(
      "Each port opens from the run page through Wardyn's web app gateway. Only people who may enter this run can open it, unless you make it callable from outside.",
    );
    expect(NR_RUN.PORT_INBOUND_LABEL).toBe("Callable from outside the run");
    expect(NR_RUN.PORT_INBOUND_HELP).toBe(
      "People and services you name can reach this port through Wardyn's gateway, even when the run has no terminal. Every call is recorded.",
    );
  });
});

function Harness({ first }: { first: UIApp[] }) {
  const [apps, setApps] = React.useState<UIApp[]>(first);
  return (
    <>
      <h2 id="ports-title">{NR_RUN.PORTS_LABEL}</h2>
      <PortsSection
        labelId="ports-title"
        apps={apps}
        edit={(i, patch) => setApps((a) => a.map((x, n) => (n === i ? { ...x, ...patch } : x)))}
        add={() => setApps((a) => [...a, { name: "app2", port: 0, path: "/" }])}
        remove={(i) => setApps((a) => a.filter((_, n) => n !== i))}
      />
      <p data-testid="apps">{JSON.stringify(apps)}</p>
    </>
  );
}
const appsNow = (): UIApp[] => JSON.parse(screen.getByTestId("apps").textContent ?? "[]");

describe("PortsSection", () => {
  it("draws one bordered card per port, each with its own checkbox and help", () => {
    render(<Harness first={[{ name: "web", port: 3000, path: "/" }, { name: "api", port: 8080, path: "/" }]} />);
    const groups = screen.getAllByRole("group").filter((g) => g.tagName === "DIV");
    expect(groups.map((g) => g.getAttribute("aria-label"))).toEqual(["web", "api"]);
    for (const g of groups) {
      expect(within(g).getByRole("checkbox", { name: NR_RUN.PORT_INBOUND_LABEL })).not.toBeChecked();
      expect(within(g).getByText(NR_RUN.PORT_INBOUND_HELP)).toBeInTheDocument();
    }
    expect(screen.getByText(NR_RUN.PORTS_HELP)).toBeInTheDocument();
    expect(screen.getByRole("group", { name: NR_RUN.PORTS_LABEL })).toBeInTheDocument();
  });

  it("marks one port callable and writes inbound to that port only", async () => {
    render(<Harness first={[{ name: "web", port: 3000, path: "/" }, { name: "api", port: 8080, path: "/" }]} />);
    await userEvent.click(within(screen.getByRole("group", { name: "api" })).getByRole("checkbox"));
    expect(appsNow()).toEqual([{ name: "web", port: 3000, path: "/" }, { name: "api", port: 8080, path: "/", inbound: true }]);
  });

  it("keeps the callers a draft already holds while the port is callable, and drops them with it", async () => {
    render(<Harness first={[{ name: "api", port: 8080, path: "/", inbound: true, callers: ["group:payments"] }]} />);
    const box = screen.getByRole("checkbox", { name: NR_RUN.PORT_INBOUND_LABEL });
    expect(box).toBeChecked();
    expect(appsNow()[0].callers).toEqual(["group:payments"]);
    await userEvent.click(box);
    expect(appsNow()).toEqual([{ name: "api", port: 8080, path: "/" }]);
  });
});
