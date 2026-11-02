/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { agoLong, agoShort, digestShort, WORKLOAD_BINDINGS, WORKLOAD_CHIP, WORKLOADS } from "./workloads";

// The approved mock (r16 T3-a and T3-b), spelled out so a reword fails.
describe("workload canon strings", () => {
  it("the Workloads screen", () => {
    const W = WORKLOADS;
    expect([W.NAV, W.TITLE]).toEqual(["Workloads", "Workloads"]);
    expect([W.COL_NAME, W.COL_OWNER, W.COL_SOURCE, W.COL_RUN, W.COL_STATE, W.COL_UPDATED]).toEqual([
      "Name", "Owner", "Source", "Run", "State", "Updated",
    ]);
    expect([W.OWNER_YOU, W.SOURCE_API, `${W.SOURCE_K8S_PREFIX}team-payments`]).toEqual(["You", "Wardyn CLI or API", "Kubernetes: team-payments"]);
    expect(`${W.EMPTY_LEAD} ${W.EMPTY_COMMAND}${W.EMPTY_END}`).toBe("No workloads yet. Apply one with wardyn apply -f workload.yaml.");
  });

  it("the state badges", () => {
    const W = WORKLOADS;
    expect([W.RUNNING, W.UNHEALTHY, W.QUEUED, W.ENDED, W.REFUSED, W.NOT_BOUND, W.BINDING_INACTIVE]).toEqual([
      "Running", "Unhealthy", "Queued", "Ended", "Refused", "Not bound", "Binding inactive",
    ]);
  });

  it("the drawer", () => {
    const W = WORKLOADS;
    expect([W.GENERATION, W.DIGEST, W.CURRENT_RUN, W.INBOUND_ADDRESS, W.LAST_REFUSAL, W.SPEC, W.RESTART, W.DELETE]).toEqual([
      "Generation", "Digest", "Current run", "Inbound address", "Last refusal", "Spec", "Restart", "Delete",
    ]);
    expect(digestShort("9f1c4e5d2b7a3c8e0f6a1b9d4c7e2a5b8f3d6c9e1a4b7d0f3c6e9a2b5d8a07b")).toBe("sha256:9f1c4e…a07b");
    expect([W.HISTORY, W.HISTORY_GEN(4), W.HISTORY_LAUNCHED, W.HISTORY_ENDED, W.HISTORY_REFUSED]).toEqual([
      "History", "gen 4", "launched", "ended", "refused",
    ]);
    expect(W.STARTED("2 d ago")).toBe("started 2 d ago");
    expect(W.REFUSAL_GENERATION(3)).toBe("Generation 3");
    expect(W.PREVIOUS_KEPT).toBe("the previous run kept running.");
    expect(`${W.MANAGED_LEAD} team-payments${W.MANAGED_TAIL}`).toBe("Managed by Kubernetes namespace team-payments. Change it in Git.");
    expect(W.DELETE_HIDDEN).toBe("Delete is hidden: this workload is managed by Kubernetes.");
  });

  it("the run header chip", () => {
    expect(WORKLOAD_CHIP("billing-api")).toBe("Workload: billing-api");
  });

  it("the bindings tab and its two dialogs", () => {
    const B = WORKLOAD_BINDINGS;
    expect([B.TAB, B.BIND]).toEqual(["Workload bindings", "Bind namespace"]);
    expect([B.COL_POOL, B.COL_NAMESPACE, B.COL_SERVICE, B.COL_SPONSOR, B.COL_CREATED, B.DELETE]).toEqual([
      "Pool", "Namespace", "Service identity", "Sponsor", "Created", "Delete",
    ]);
    expect([B.DIALOG_TITLE, B.POOL, B.NAMESPACE, B.SERVICE, B.CANCEL, B.CONFIRM]).toEqual([
      "Bind namespace", "Pool", "Namespace", "Service identity", "Cancel", "Bind namespace",
    ]);
    expect(B.HELP).toBe("Anyone who can create a WardynWorkload in this namespace will launch runs as this service identity, in this pool.");
    expect(B.DELETE_TITLE("team-payments")).toBe("Delete the binding for team-payments?");
    expect(B.DELETE_BODY).toBe("Runs already started keep running.");
    expect(B.DELETE_CONFIRM).toBe("Delete");
  });
});

describe("workload time words", () => {
  const now = 1_800_000_000_000;
  const ago = (ms: number) => new Date(now - ms).toISOString();
  const MIN = 60_000;
  const H = 60 * MIN;
  const D = 24 * H;

  it("agoShort is the list's and the drawer's stamp", () => {
    expect(agoShort(ago(10_000), now)).toBe("just now");
    expect(agoShort(ago(2 * MIN), now)).toBe("2 min ago");
    expect(agoShort(ago(4 * MIN), now)).toBe("4 min ago");
    expect(agoShort(ago(H), now)).toBe("1 h ago");
    expect(agoShort(ago(3 * H), now)).toBe("3 h ago");
    expect(agoShort(ago(D), now)).toBe("yesterday");
    expect(agoShort(ago(2 * D), now)).toBe("2 d ago");
    expect(agoShort(ago(9 * D), now)).toBe("9 d ago");
  });

  it("agoLong is the bindings table's stamp", () => {
    expect(agoLong(ago(12 * D), now)).toBe("12 days ago");
    expect(agoLong(ago(5 * D), now)).toBe("5 days ago");
    expect(agoLong(ago(60 * D), now)).toBe("2 months ago");
    expect(agoLong(ago(D), now)).toBe("1 day ago");
    expect(agoLong(ago(5 * H), now)).toBe("5 hours ago");
  });
});
