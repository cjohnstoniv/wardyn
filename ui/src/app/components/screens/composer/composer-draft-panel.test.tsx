/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { ASSISTANT as T } from "../../wardyn/copy/assistant";
import { SUMMARY } from "../run-detail/policy-tab-copy";
import { ComposerDraftPanel, draftLines, valueLines } from "./composer-draft-panel";

afterEach(cleanup);

describe("valueLines", () => {
  it.each([
    [["a.example", "b.example"], ["a.example", "b.example"]],
    ["node-20-dev", ["node-20-dev"]],
    [{ pushes: "hold" }, ["pushes: hold"]],
    [null, []],
    [[["x"], "y"], ["x", "y"]],
  ])("%j reads as %j", (value, lines) => expect(valueLines(value)).toEqual(lines));
});

describe("draftLines", () => {
  it("counts each value line, a part with none as one, and an empty draft as none", () => {
    expect(draftLines(null)).toBe(0);
    expect(draftLines({ parts: [{ section: "allowed_hosts", value: ["a", "b"] }, { section: "barrier", value: null }], dropped: [] })).toBe(3);
  });
});

describe("ComposerDraftPanel", () => {
  it("names each part in the Summary's words and marks it proposed", () => {
    render(<ComposerDraftPanel proposal={{ parts: [{ section: "allowed_hosts", value: ["a.example"] }, { section: "credentials", value: "git" }], dropped: [] }} />);
    expect(screen.getByText(SUMMARY.allowedHosts)).toBeInTheDocument();
    expect(screen.getByText(SUMMARY.credentials)).toBeInTheDocument();
    expect(screen.getAllByText(T.PART_PROPOSED)).toHaveLength(2);
    expect(screen.getByText(T.DRAFT_LEAD)).toBeInTheDocument();
  });

  it("lists a left-out host or field under the parts", () => {
    render(<ComposerDraftPanel proposal={{ parts: [], dropped: [{ host: "x.example", unit: "Payments" }, { field: "image", unit: "Payments" }] }} />);
    expect(screen.getByText(T.LEFT_OUT("x.example", "Payments"))).toBeInTheDocument();
    expect(screen.getByText(T.LEFT_OUT("image", "Payments"))).toBeInTheDocument();
  });

  it("shows the lead and nothing else before the first proposal", () => {
    render(<ComposerDraftPanel proposal={null} />);
    expect(screen.getByText(T.DRAFT_LEAD)).toBeInTheDocument();
    expect(screen.queryByText(T.PART_PROPOSED)).toBeNull();
  });
});
