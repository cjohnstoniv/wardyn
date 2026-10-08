/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Canon: the approved M-R prototype, version 7 (1791427781-730a). Every string
// the shared policy document and source editor render is pinned here character
// for character, so a rewording fails a test instead of drifting from what was
// approved.
import { describe, expect, it } from "vitest";
import { PENDING_NAME, POLICY_DOCUMENT as D } from "./policy-document";
import { POLICY_TEMPLATE_COPY as C } from "./policy-templates";

describe("policy document copy — canon pins", () => {
  it("read and edit modes", () => {
    expect(D.THIS_RUN).toBe("This run's policy");
    expect(D.READ_EDIT).toBe("Read-only. Choose Edit policy to change it.");
    expect(D.READ_CUSTOMIZE).toBe("Read-only. Choose Customize for this run to change it.");
    expect(D.READ_UPDATES).toBe("Read-only. It updates as you edit the policy below.");
    expect(D.MERGED).toBe("It also includes what this run's selections add and what your limits change.");
    expect(D.EDIT).toBe("Edit policy");
    expect(D.DONE).toBe("Done editing");
    expect(D.EDITING).toBe("Editing");
    expect(D.CUSTOMIZE).toBe("Customize for this run");
    expect(D.SOURCE_POLICY).toBe("Source policy");
  });

  it("views and copies", () => {
    expect(D.JSON).toBe("JSON");
    expect(D.COPY_JSON).toBe("Copy JSON");
    expect(D.COPY_SOURCE).toBe("Copy source");
  });

  it("the source editor: YAML by default, JSON by choice", () => {
    expect(D.SPEC_YAML).toBe("Spec (YAML)");
    expect(D.SPEC_JSON).toBe("Spec (JSON)");
    expect(D.VALID_YAML).toBe("Valid YAML");
    expect(D.INVALID_YAML("x")).toBe("Invalid YAML — x");
    expect(D.VALID_JSON).toBe("Valid JSON");
    expect(D.INVALID_JSON("x")).toBe("Invalid JSON — x");
    expect(D.SOURCE_POSITION(4, 3)).toBe("Line 4, column 3");
    expect(D.INVALID_GATE).toBe("The policy spec isn't valid YAML or JSON.");
    expect(D.COMMENTS).toBe(
      "Comments are kept while you edit; they are not stored when the policy is saved or the run launches.",
    );
    expect(D.SAFE_CUSTOM).toBe("Some source settings are hidden. Customization starts from a safe policy.");
  });

  it("preview states", () => {
    expect(D.INVALID_PREVIEW).toBe("This preview is out of date. Fix the policy source to refresh it.");
    expect(D.STALE_PREVIEW).toBe("This preview is out of date. Check again to refresh it.");
    expect(D.PROVISIONAL).toBe("This preview includes your selections. Launch checks may change it.");
    expect(D.PENDING).toBe("Not checked in this preview.");
    expect(D.RATE_LIMIT(5)).toBe("Preview limit reached. Try again in 5s.");
  });

  it("the two confirmations", () => {
    expect(D.JSON_TITLE).toBe("Switch to JSON?");
    expect(D.JSON_BODY).toBe("JSON does not preserve YAML comments. Switching changes your editable source.");
    expect(D.KEEP_YAML).toBe("Keep YAML");
    expect(D.SWITCH_JSON).toBe("Switch to JSON");
    expect(D.REPLACE_TITLE).toBe("Replace your custom policy?");
    expect(D.REPLACE_BODY).toBe("Your existing custom policy will be replaced by this source.");
    expect(D.KEEP_CUSTOM).toBe("Keep custom policy");
    expect(D.REPLACE_CUSTOM).toBe("Replace custom policy");
  });

  it("the Summary's added names", () => {
    expect(D.REQUESTED_CLASS).toBe("This run requests");
    expect(D.ALLOW_ALL).toBe("Allow-all egress (block-list only)");
    expect(D.HOLDS_AT_ONCE).toBe("Holds at once");
    expect(D.OTHER_SETTINGS).toBe("Other settings");
    expect(D.STOPS_AFTER(60)).toBe("Stops after 60 minutes");
    expect(D.TOOL_RULE("Bash", "held")).toBe("Bash — held");
    expect(D.CHECKED_AT_LAUNCH).toBe("Checked at launch");
    expect(PENDING_NAME).toEqual({
      task: "Task",
      model_provider_selection: "Model provider",
      credential_liveness: "Credentials still valid",
      autonomy: "Autonomy",
      tool_approvals: "Tool approvals",
      runner_confinement: "Barrier on the runner",
      drive_readiness: "Drive ready",
      dispatch_egress: "Hosts added at dispatch",
    });
  });

  it("the mode cards keep the wording they had inline", () => {
    expect(C.DEFAULT_TITLE).toBe("Use the default policy");
    expect(C.SAVED_TITLE).toBe("Reuse a saved policy");
    expect(C.SAVED_HINT).toBe("One your operators already wrote and named.");
    expect(C.CUSTOM_TITLE).toBe("Custom policy");
    expect(C.CUSTOM_HINT).toBe("Start from a template and edit the spec for this run.");
  });
});
