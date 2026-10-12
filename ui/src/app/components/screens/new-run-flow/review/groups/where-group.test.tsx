/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { NR_REVIEW } from "../../../../wardyn/copy/new-run/review";
import { RECORDING_DISABLED_TITLE } from "../../../../wardyn/copy";
import { SCENARIO_PREFLIGHT } from "../../../../../lib/new-run-fixtures/scenarios/preflight";
import { initialDraft } from "../../draft/run-intent";
import { WhereGroup } from "./where-group";

// undefined = /healthz has not answered, or carried no recording component.
const recording = vi.hoisted(() => ({ selected: "fs" as string | undefined }));
vi.mock("../../../../../lib/api/health", () => ({
  health: { health: () => Promise.resolve({ components: { recording: { selected: recording.selected } } }) },
}));

afterEach(cleanup);

const renderWhere = () =>
  render(<WhereGroup draft={initialDraft("alice")} pre={SCENARIO_PREFLIGHT.member} preview={null} docRows={[]} go={vi.fn()} dispatch={vi.fn()} />);

describe("Review: where it runs, recording row", () => {
  it("says what is recorded when the deployment records", async () => {
    recording.selected = "pg";
    renderWhere();
    expect(await screen.findByText(NR_REVIEW.RECORDING_ON)).toBeInTheDocument();
    expect(screen.getByText(NR_REVIEW.RECORDING_LABEL)).toBeInTheDocument();
    expect(screen.queryByText(RECORDING_DISABLED_TITLE)).toBeNull();
  });

  it("says recording is off when the deployment has no recording store", async () => {
    recording.selected = "none";
    renderWhere();
    expect(await screen.findByText(RECORDING_DISABLED_TITLE)).toBeInTheDocument();
    expect(screen.queryByText(NR_REVIEW.RECORDING_ON)).toBeNull();
  });

  it("makes no claim when /healthz does not say", async () => {
    recording.selected = undefined;
    const { container } = renderWhere();
    await screen.findByText(NR_REVIEW.GROUP_WHERE);
    await new Promise((r) => setTimeout(r, 0));
    expect(container.querySelector("[data-review-recording]")).toBeNull();
  });
});
