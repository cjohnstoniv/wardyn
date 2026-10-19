/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #2130: under the suffixes box, one line per entry says what it covers, read live off the box.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

const getMock = vi.fn();
vi.mock("../../../lib/api/egress-baseline", () => ({
  egressBaseline: { get: () => getMock(), put: vi.fn() },
}));

import { setField } from "../../../../test/set-field";
import { BASELINE } from "../../../lib/governance-copy";
import { EgressBaselineTab } from "./egress-baseline";
import { coveredSuffixes } from "./egress-baseline-coverage";

beforeEach(() => {
  getMock.mockReset().mockResolvedValue({ baseline_hosts: [], internal_host_suffixes: ["corp.example"], etag: '"e1"' });
});

describe("EgressBaselineTab coverage notice", () => {
  it("canon: the coverage line and the exact-hosts sentence, character for character", () => {
    expect(BASELINE.COVERS("corp.example")).toBe("corp.example covers corp.example only, not the hosts under it (*.corp.example).");
    expect(BASELINE.HOSTS_HINT).toBe("One hostname per line, for example llm.corp.example. No wildcards, ports or IP addresses. Covers only the host named, not its subdomains.");
  });

  it("names the reach of a stored entry and of one typed, without a save", async () => {
    render(
      <MemoryRouter>
        <EgressBaselineTab />
      </MemoryRouter>,
    );
    const list = await screen.findByTestId("baseline-suffixes-covers");
    expect(within(list).getByText(BASELINE.COVERS("corp.example"))).toBeInTheDocument();
    setField(screen.getByLabelText(BASELINE.SUFFIXES_LABEL), "corp.example\n  build.internal  \n\n");
    expect(within(list).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      BASELINE.COVERS("corp.example"),
      BASELINE.COVERS("build.internal"),
    ]);
    expect(screen.getByText(BASELINE.HOSTS_HINT)).toBeInTheDocument();
  });

  it("lists nothing for an empty box", async () => {
    getMock.mockResolvedValue({ baseline_hosts: [], internal_host_suffixes: [], etag: '"e1"' });
    render(
      <MemoryRouter>
        <EgressBaselineTab />
      </MemoryRouter>,
    );
    const list = await screen.findByTestId("baseline-suffixes-covers");
    expect(within(list).queryAllByRole("listitem")).toHaveLength(0);
  });

  it("describes only a line that can be a hostname: a wildcard, scheme, port, path or address is left out", async () => {
    expect(coveredSuffixes(["corp.example", "*.corp.example", "https://corp.example", "corp.example:8443", "corp.example/x", "10.0.0.1", "Build.Internal"])).toEqual([
      "corp.example",
      "build.internal",
    ]);
    render(
      <MemoryRouter>
        <EgressBaselineTab />
      </MemoryRouter>,
    );
    const list = await screen.findByTestId("baseline-suffixes-covers");
    setField(screen.getByLabelText(BASELINE.SUFFIXES_LABEL), "*.corp.example");
    expect(within(list).queryAllByRole("listitem")).toHaveLength(0);
    expect(list).not.toHaveTextContent("*.*.");
  });
});
