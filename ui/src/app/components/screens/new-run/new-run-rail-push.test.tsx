/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #181 — the New Run rail's "Push rules" section. A SEPARATE file from
// new-run-rail.test.tsx (already at file-size-gate scale) rather than a
// bigger describe block there — split by seam, per AGENTS.md.
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { RunRail } from "./new-run-rail";
import { PUSH } from "../../wardyn/copy/push";
import type { PushRulesSpec } from "../../../lib/types";

function renderRail(pushRules: PushRulesSpec | undefined, unattended = false) {
  return render(
    <RunRail
      cc="CC1"
      showModelWarning={false}
      startup="It starts."
      showHoldNote={false}
      toolRules={null}
      pushRules={pushRules}
      unattended={unattended}
      launch={{
        onLaunch: () => {},
        disabled: false,
        spinning: false,
        inFlight: false,
        problem: null,
        error: null,
        credentialRefused: false,
        warnings: [],
        onOpenRun: null,
      }}
      preflight={{ error: null, result: null }}
      adoDialog={{
        open: false,
        connecting: false,
        org: "",
        blockedUrl: null,
        onConfirm: () => {},
        onFallbackClick: () => {},
        onCancel: () => {},
      }}
    />,
  );
}

describe("New run rail — Push rules section (#181)", () => {
  it("renders no section at all when the policy has no push rules", () => {
    renderRail(undefined);
    expect(screen.queryByText(PUSH.RAIL_TITLE)).toBeNull();
  });

  it("renders no section for a push_rules that is present but empty ({}) — mirrors Go's IsSet()", () => {
    renderRail({});
    expect(screen.queryByText(PUSH.RAIL_TITLE)).toBeNull();
  });

  // Review finding 6 — pushRulesIsSet alone is true here (max_inspect_pack_mib
  // counts toward Go's own IsSet()), but there is nothing to say about PATHS,
  // and "0 paths denied · 0 paths held for review" would read as a real
  // (empty) rule rather than "no path rule at all".
  it("renders no section when only max_inspect_pack_mib is set — nothing to say about paths", () => {
    renderRail({ max_inspect_pack_mib: 16 });
    expect(screen.queryByText(PUSH.RAIL_TITLE)).toBeNull();
  });

  it("counts deny_paths and require_review_paths, pluralized", () => {
    renderRail({ deny_paths: [".github/workflows/**", "secrets/**"], require_review_paths: ["infra/**"] });
    expect(screen.getByText(PUSH.RAIL_TITLE)).toBeInTheDocument();
    expect(screen.getByText("2 paths denied · 1 path held for review")).toBeInTheDocument();
  });

  it("singularizes exactly 1 of each", () => {
    renderRail({ deny_paths: ["a"], require_review_paths: ["b"] });
    expect(screen.getByText("1 path denied · 1 path held for review")).toBeInTheDocument();
  });

  it("reads 0 for the side with no entries", () => {
    renderRail({ require_review_paths: ["infra/**"] });
    expect(screen.getByText("0 paths denied · 1 path held for review")).toBeInTheDocument();
  });

  it("renders the unattended line only when the run is unattended", () => {
    renderRail({ require_review_paths: ["infra/**"] }, true);
    expect(screen.getByText(PUSH.RAIL_UNATTENDED)).toBeInTheDocument();
  });

  it("omits the unattended line for an interactive run", () => {
    renderRail({ require_review_paths: ["infra/**"] }, false);
    expect(screen.queryByText(PUSH.RAIL_UNATTENDED)).toBeNull();
  });

  // Review finding 6 — an unattended run with ONLY deny_paths (no review
  // rule at all) never sees the unattended note: a deny_paths match refuses
  // identically whether the run is attended or not, so the note would claim
  // a fact true only of the review half of the section.
  it("omits the unattended line when the section has deny_paths but no require_review_paths", () => {
    renderRail({ deny_paths: [".github/workflows/**"] }, true);
    expect(screen.getByText(PUSH.RAIL_TITLE)).toBeInTheDocument();
    expect(screen.queryByText(PUSH.RAIL_UNATTENDED)).toBeNull();
  });
});
