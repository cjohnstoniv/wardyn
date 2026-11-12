/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { renderHook, act } from "@testing-library/react";
import { HttpError } from "../../../lib/api/core";
import { useRefusedApprovals } from "../../../lib/hooks/use-refused-approvals";
import { WallPresetQueuedChanged, WallPresetRefusalLine, WallPresetRunLine, wallPresetApprovalSentence } from "./wall-preset-run-lines";

afterEach(cleanup);

describe("run page wall preset sentences", () => {
  it("P-60: names the preset, its audience and version", () => {
    render(<WallPresetRunLine name="Sealed" audience="built in" version={1} />);
    expect(screen.getByTestId("wall-preset-run-line")).toHaveTextContent("Wall preset: Sealed (built in, version 1).");
  });

  it("P-60b: names nothing for a personal preset its reader does not own", () => {
    render(<WallPresetRunLine />);
    expect(screen.getByTestId("wall-preset-run-line")).toHaveTextContent("Wall preset: a personal wall preset.");
  });

  it("P-70: says the run's preset refused the push", () => {
    render(<WallPresetRefusalLine name="Sealed" />);
    expect(screen.getByTestId("wall-preset-refusal-line")).toHaveTextContent("Refused by this run's Sealed wall preset.");
  });

  it("P-67 and P-68: the approval sentence for the owner's run and for somebody else's", () => {
    expect(wallPresetApprovalSentence({ name: "Sealed", host: "x.example", ownRun: true, owner: "ana" }).join("")).toBe("This run's Sealed wall preset blocks x.example. Launch a new run without it.");
    expect(wallPresetApprovalSentence({ name: "Sealed", host: "x.example", ownRun: false, owner: "ana" }).join("")).toBe("ana's run uses the Sealed wall preset, which blocks x.example.");
  });
});

describe("useRefusedApprovals: wall_preset_host", () => {
  it("is kept apart from the walls and the withheld-grant refusals, and only for a 403", () => {
    const { result } = renderHook(() => useRefusedApprovals());
    act(() => {
      result.current.noteRefusal("a", new HttpError(403, "x", "wall_preset_host"));
      result.current.noteRefusal("b", new HttpError(403, "x", "personal_walls_host"));
      result.current.noteRefusal("c", new HttpError(409, "x", "wall_preset_host"));
    });
    expect([...result.current.presetRefused]).toEqual(["a"]);
    expect([...result.current.wallsRefused]).toEqual(["b"]);
    expect([...result.current.refused]).toEqual([]);
  });
});

describe("P-85: the queued run's version line", () => {
  const preset = (over: object) => ({ id: "o1", name: "Docs triage", scope: "org" as const, version: 3, ...over });
  it.each([
    ["the version changed", preset({ changed: { version: 4 } }), "The run was queued with the Docs triage wall preset, version 3. It was at version 4 when the run would have started."],
    ["the preset went", preset({ changed: { gone: true } }), "The run was queued with the Docs triage wall preset, version 3. It was no longer available to this run when it would have started."],
  ])("says so when %s", (_n, wall_preset, text) => {
    render(<WallPresetQueuedChanged preset={wall_preset} />);
    expect(screen.getByTestId("wall-preset-queued-changed")).toHaveTextContent(text);
  });

  it.each([
    ["no preset", undefined],
    ["a preset that did not change", preset({})],
    ["a changed record at the same version", preset({ changed: { version: 3 } })],
    ["a personal preset read by a non-owner (no name, no version)", { scope: "person" as const, changed: { version: 5 } }],
  ])("says nothing for %s", (_n, wall_preset) => {
    const { container } = render(<WallPresetQueuedChanged preset={wall_preset} />);
    expect(container).toBeEmptyDOMElement();
  });
});
