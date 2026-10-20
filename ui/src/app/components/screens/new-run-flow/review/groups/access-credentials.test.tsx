/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MY_COMPONENTS, ORG_KUBECTL } from "../../../../../lib/new-run-fixtures/scenarios/catalogue";
import { SCENARIO_ACCESS } from "../../../../../lib/new-run-fixtures/scenarios/tools";
import type { AccessCredential, AccessFacts } from "../../../../../lib/types/new-run-access";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { AccessCredentials } from "./access-credentials";

const h = vi.hoisted(() => ({ mine: vi.fn() }));
vi.mock("../../../../../lib/api/components", () => ({ components: { mine: h.mine } }));

const KUBE: AccessCredential = { id: `${ORG_KUBECTL.id}:kube_api`, row: ORG_KUBECTL.id, kind: "kube_api", owner: "shared", label: "Your organisation's cluster account", delivery: "proxy", renewed: true, after_end: "gone" };
const facts = (credentials: AccessCredential[]): AccessFacts => ({
  ...SCENARIO_ACCESS.member,
  rows: [{ id: ORG_KUBECTL.id, kind: "cli", from: "added", name: ORG_KUBECTL.name, status: "ready", hosts: [], credentials: credentials.map((c) => c.id), source: { kind: "component" } }],
  credentials,
});

beforeEach(() => { h.mine.mockResolvedValue({ ...MY_COMPONENTS, org: [ORG_KUBECTL] }); });
afterEach(() => { cleanup(); vi.clearAllMocks(); });

describe("Review, Access: a kubectl cluster's credential", () => {
  it("names the service account after the cluster, then the row, the namespaces and what the run may do there", async () => {
    render(<AccessCredentials access={facts([KUBE])} />);
    expect(screen.getByRole("heading", { name: NR_REVIEW.POLICY_CREDENTIALS })).toBeTruthy();
    await waitFor(() => expect(screen.getByText(NR_TOOLS.KUBE_CREDENTIAL("staging"))).toBeTruthy());
    expect(screen.getByText(NR_TOOLS.FOR_ROW(ORG_KUBECTL.name))).toBeTruthy();
    expect(screen.getByText(`${NR_REVIEW.ACCESS_KUBE_LINE("web, jobs")} · ${NR_TOOLS.KUBE_READ_ONLY} · ${NR_TOOLS.KUBE_NO_EXEC}`)).toBeTruthy();
    expect(screen.getByText(NR_TOOLS.DELIVERY_PROXY)).toBeTruthy();
  });

  it("says the server's own label until the organisation's row has been read", () => {
    h.mine.mockReturnValue(new Promise(() => {}));
    render(<AccessCredentials access={facts([KUBE])} />);
    expect(screen.getByText("Your organisation's cluster account")).toBeTruthy();
  });

  it("shows nothing, and reads nothing, for a run without one", () => {
    render(<AccessCredentials access={facts([])} />);
    expect(screen.queryByRole("heading", { name: NR_REVIEW.POLICY_CREDENTIALS })).toBeNull();
    expect(h.mine).not.toHaveBeenCalled();
  });
});
