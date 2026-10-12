/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { NR_FILES } from "../../../wardyn/copy/new-run/files";
import { NR_REVIEW } from "../../../wardyn/copy/new-run/review";
import { NR_SHARED } from "../../../wardyn/copy/new-run/shared";
import { NR_TEMPLATES } from "../../../wardyn/copy/new-run/templates";
import type { SetupItem } from "../../../../lib/types/preflight";
import { buildBlockers, missingSetup, specifies, type BlockerInput } from "./blockers";

const ITEM: SetupItem = { id: "secret:deploy-key", kind: "secret", label: "deploy-key", required_by: "Application", status: "missing", detail: "Add the deploy key." };
const NOTHING: BlockerInput = { issues: [], unsupported: [], templateNeeds: [], refusal: null, setupItems: [], stale: false };
const EVERYTHING: BlockerInput = {
  issues: [{ home: "files", code: "main_drive_size_required", focus: "nr-main-drive-size" }],
  unsupported: ["legacy_field"],
  templateNeeds: [{ field: "inline_policy.model_provider" }],
  refusal: { text: "None of your runners is online.", reason: "runner_offline" },
  setupItems: [ITEM],
  stale: true,
};

describe("Review blockers", () => {
  it("lists local issues, unsupported fields, template setup, the refusal, then missing setup, in that order", () => {
    expect(buildBlockers(EVERYTHING).map((b) => b.kind)).toEqual(["local", "unsupported", "template_setup", "refusal", "setup_item"]);
  });

  it("says each blocker in its own words and links to where the fix lives", () => {
    const [local, unsupported, template, refusal, setup] = buildBlockers(EVERYTHING);
    expect(local).toMatchObject({ text: NR_FILES.SIZE_ERROR, to: { screen: "step", step: "files" } });
    expect(unsupported).toMatchObject({ text: NR_REVIEW.BLOCKER_UNSUPPORTED("legacy_field"), to: { screen: "document" } });
    expect(template).toMatchObject({ text: NR_TEMPLATES.NEEDS_SETUP("Model provider") });
    expect(refusal).toMatchObject({ text: "None of your runners is online.", to: { screen: "step", step: "run" } });
    expect(setup).toMatchObject({ text: "Add the deploy key.", to: { screen: "step", step: "tools" } });
  });

  it("falls back to the screen's name for a local issue the canon has no sentence for, and links a refusal to nowhere when it is not known", () => {
    const out = buildBlockers({ ...NOTHING, issues: [{ home: "run", code: "pool_required", focus: "nr-pool" }], refusal: { text: "Refused.", reason: "something_new" } });
    expect(out[0].text).toBe(NR_SHARED.STEP_RUN);
    expect(out[1].to).toBeUndefined();
  });

  it("adds the stale line only when nothing else holds Launch", () => {
    expect(buildBlockers({ ...NOTHING, stale: true })).toEqual([{ id: "stale", kind: "stale", text: NR_REVIEW.BLOCKER_STALE }]);
    expect(buildBlockers({ ...EVERYTHING }).some((b) => b.kind === "stale")).toBe(false);
    expect(buildBlockers(NOTHING)).toEqual([]);
  });

  it("reads missing setup from the checklist and the access block once each", () => {
    const ok: SetupItem = { ...ITEM, id: "secret:ok", status: "ready" };
    const rows = missingSetup({ setup_items: [ITEM, ok], access: { setup: [ITEM, { ...ITEM, id: "secret:other" }] } });
    expect(rows.map((r) => r.id)).toEqual(["secret:deploy-key", "secret:other"]);
    expect(missingSetup(null)).toEqual([]);
  });

  it("counts a template field as set once the draft specifies it", () => {
    expect(specifies({}, "inline_policy.model_provider")).toBe(false);
    expect(specifies({ inline_policy: { allowed_domains: [] } }, "inline_policy.allowed_domains")).toBe(true);
    expect(specifies({ no_end: false }, "no_end")).toBe(true);
  });
});
