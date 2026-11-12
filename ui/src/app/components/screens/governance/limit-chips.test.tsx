/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import type { GovernanceLimits } from "../../../lib/api/governance";
import { BACKGROUND_WATCH_DOOR, GOVERNANCE as GOV, LIMITS_CHIP } from "../../../lib/governance-copy";
import { MAC_BASES } from "../../wardyn/copy/mac-bases";
import { DENY_LISTS } from "../../wardyn/copy/deny-lists";
import { LIMIT_FIELDS } from "./profile-overlay";
import { limitChips, limitsBoundNothing } from "./limit-chips";

// #2111/#2506: every GovernanceLimits field is a limit, and "None" is the claim that a profile bounds none of
// them. A profile bounding only a placement, the UI apps, CPU, memory or a disk cap used to read "None" while
// the server enforced it.
describe("limitChips and limitsBoundNothing", () => {
  it.each([
    ["deny_placements", { deny_placements: ["remote"] }, LIMITS_CHIP.PLACEMENT("remote")],
    ["allowed_macos_bases", { allowed_macos_bases: ["Standard", "xcode-16"] }, `${MAC_BASES.B14}: Standard, xcode-16`],
    ["allowed_models, up to three named", { allowed_models: ["model-a", "small-model-a"] }, "Allowed models: model-a, small-model-a"],
    ["allowed_models, more than three counted", { allowed_models: ["model-a", "model-b", "model-c", "model-d"] }, "Allowed models: 4"],
    ["deny_components, one", { deny_components: ["11111111-1111-4111-8111-111111111111"] }, DENY_LISTS.COMPONENTS_CHIP(1)],
    ["deny_model_providers, two", { deny_model_providers: ["11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"] }, DENY_LISTS.PROVIDERS_CHIP(2)],
    ["deny_ui_apps", { deny_ui_apps: true }, LIMITS_CHIP.UI_APPS],
    ["deny_background_desktop_watch", { deny_background_desktop_watch: true }, BACKGROUND_WATCH_DOOR.LABEL],
    ["max_cpu_millis", { max_cpu_millis: 2000 }, `${LIMITS_CHIP.CPU}: 2000`],
    ["max_memory_mib", { max_memory_mib: 2048 }, `${LIMITS_CHIP.MEMORY}: 2048`],
    ["max_ephemeral_disk_mib", { max_ephemeral_disk_mib: 4096 }, `${GOV.LIMIT_EPHEMERAL_LABEL}: 4096`],
    ["max_drive_size_mib", { max_drive_size_mib: 1024 }, `${GOV.LIMIT_DRIVE_SIZE_LABEL}: 1024`],
  ] as [string, GovernanceLimits, string][])("a profile whose only limit is %s draws a chip and is not 'None'", (_field, limits, chip) => {
    render(<>{limitChips(limits)}</>);
    expect(screen.getByText(chip)).toBeInTheDocument();
    expect(limitsBoundNothing(limits)).toBe(false);
  });

  it("a run limit the summary chip does not name still counts, and no limits at all is None", () => {
    expect(limitsBoundNothing({ default_end_sec: 86400 } as GovernanceLimits)).toBe(false);
    expect(limitsBoundNothing({})).toBe(true);
    expect(limitsBoundNothing({ max_cpu_millis: 0, deny_placements: [], allowed_macos_bases: [], allowed_models: [] })).toBe(true);
  });
});

describe("the unit deny lists in a composed profile's overlay rows", () => {
  it("are list rows with their label and hint", () => {
    expect(LIMIT_FIELDS.find((f) => f.key === "deny_components")).toMatchObject({ kind: "list", label: DENY_LISTS.COMPONENTS_LABEL, mono: false, hint: DENY_LISTS.COMPONENTS_HINT_COMPOSED });
    expect(LIMIT_FIELDS.find((f) => f.key === "deny_model_providers")).toMatchObject({ kind: "list", label: DENY_LISTS.PROVIDERS_LABEL, mono: false, hint: DENY_LISTS.PROVIDERS_HINT_COMPOSED });
  });
});
