/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MY_COMPONENTS, ORG_KUBECTL } from "../../../../../lib/new-run-fixtures/scenarios/catalogue";
import { SCENARIO_ACCESS } from "../../../../../lib/new-run-fixtures/scenarios/tools";
import type { AccessFacts, AccessHost } from "../../../../../lib/types/new-run-access";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import { initialDraft } from "../../draft/run-intent";
import type { ReviewPreflight } from "../review-seams";
import { AccessGroup } from "./access-group";

vi.mock("../../../../../lib/api/components", () => ({ components: { mine: () => Promise.resolve({ ...MY_COMPONENTS, org: [ORG_KUBECTL] }) } }));

afterEach(cleanup);

const access: AccessFacts = {
  ...SCENARIO_ACCESS.member,
  credentials: [{ id: `${ORG_KUBECTL.id}:kube_api`, row: ORG_KUBECTL.id, kind: "kube_api", owner: "shared", delivery: "proxy", after_end: "gone" }],
};

describe("Review: Access", () => {
  it("lists a kubectl cluster's service account under the hosts once opened", async () => {
    render(<AccessGroup draft={initialDraft("alice")} pre={{ access } as unknown as ReviewPreflight} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />);
    expect(screen.queryByText(NR_REVIEW.POLICY_CREDENTIALS)).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_ACCESS}` }));
    expect(await screen.findByText(NR_TOOLS.KUBE_CREDENTIAL("staging"))).toBeTruthy();
  });

  it("lists and counts the cluster's API server the server adds for a kubectl row, named for the row", async () => {
    // The shape the server's preview gives a kubectl row's host: injected, brought by the row, its source the row.
    const kubeHost: AccessHost = {
      host: "api.staging.example.com", via: "injected", rows: [ORG_KUBECTL.id], verdict: "allowed",
      source: { kind: "component", id: ORG_KUBECTL.id, name: ORG_KUBECTL.name },
    };
    const withHost: AccessFacts = { ...access, hosts: [...access.hosts, kubeHost] };
    render(<AccessGroup draft={initialDraft("alice")} pre={{ access: withHost } as unknown as ReviewPreflight} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />);
    expect(screen.getByText(NR_REVIEW.POLICY_HOSTS(2))).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: `${NR_REVIEW.CHANGE} ${NR_REVIEW.GROUP_ACCESS}` }));
    const group = screen.getByRole("group", { name: NR_REVIEW.GROUP_ACCESS });
    const row = within(group).getByText(kubeHost.host).parentElement as HTMLElement;
    expect(within(row).getByText(ORG_KUBECTL.name)).toBeTruthy();
  });
});
