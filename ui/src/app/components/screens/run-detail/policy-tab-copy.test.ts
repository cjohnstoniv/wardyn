/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Pins the Policy tab's copy to the owner-approved mock (run-policy-view-
// packet.html, strings S-1..S-48), character for character. The literals here
// are the mock's; a rewording in policy-tab-copy.ts fails this file rather than
// drifting. S-36, S-39's values, S-42's labels, S-43, S-44's idle value and S-46
// are reused from their existing sources and pinned where they live.
import { describe, it, expect } from "vitest";
import { POLICY_DOCUMENT } from "../../wardyn/copy/policy-document";
import { CHANGE_HEADING, IDENTITY_POLICY_VIEW, POLICY_TAB, SUMMARY } from "./policy-tab-copy";

describe("Policy tab copy (S-1..S-48)", () => {
  it("S-1..S-13: tab, lead, source lines, preset, banners, changes", () => {
    expect(POLICY_TAB.tab).toBe("Policy");
    expect(POLICY_TAB.lead).toBe("What this run was allowed to do, including anything Wardyn changed when it started.");
    expect(POLICY_TAB.sourceStored("ci")).toBe('Started from the saved policy "ci".');
    expect(POLICY_TAB.sourceStoredDeleted("ci")).toBe('Started from the saved policy "ci", which has since been deleted.');
    expect(POLICY_TAB.sourceStoredDeletedNoName).toBe("Started from a saved policy that has since been deleted.");
    expect(POLICY_TAB.sourceInline).toBe("Started from a policy written for this run.");
    expect(POLICY_TAB.sourceDefault).toBe("Started from your organization's default policy.");
    expect(POLICY_TAB.sourceProfile("walled")).toBe("Started from the walled governance profile.");
    expect(POLICY_TAB.sourceUnknown).toBe("Wardyn set this policy for this run.");
    expect(POLICY_TAB.preset("nightly-audit", 3)).toBe('Launched from the preset "nightly-audit", version 3.');
    expect(POLICY_TAB.changedSince("ci")).toBe(
      'The saved policy "ci" has changed since this run started. This page shows what the run got.',
    );
    expect(POLICY_TAB.updatedSince("ci")).toBe(
      'The saved policy "ci" was updated after this run started, so it may read differently now. This page shows what the run got.',
    );
    expect(POLICY_TAB.changesHeading).toBe("Changed when the run started");
    expect(POLICY_TAB.nothingChanged).toBe("Nothing was changed. The run got the policy exactly as written.");
  });

  it("S-14a..S-23: change headings", () => {
    expect(CHANGE_HEADING.limitsOwn).toBe("Narrowed to fit your limits");
    expect(CHANGE_HEADING.limitsOther("dana@acme.example")).toBe("Narrowed to fit the limits set for dana@acme.example");
    expect(CHANGE_HEADING.workspace).toBe("Added for the workspace");
    expect(CHANGE_HEADING.source_control).toBe("Added so the run can reach its code");
    expect(CHANGE_HEADING.git_broker).toBe("Routed through Wardyn's GitHub connection");
    expect(CHANGE_HEADING.model_access).toBe("Added so the agent can reach its model");
    expect(CHANGE_HEADING.mirror).toBe("Switched to your organization's package mirror");
    expect(CHANGE_HEADING.profile("walled")).toBe("Limited by the walled governance profile");
    expect(CHANGE_HEADING.restart("Sep 29, 2026")).toBe("Blocked when the run was restarted on Sep 29, 2026");
    expect(CHANGE_HEADING.org_disk).toBe("Disk size set from your organization's default");
    expect(CHANGE_HEADING.launch).toBe("Set by Wardyn when the run started");
  });

  // M-R version 7 amends S-25/S-26: Summary and YAML keep their names and gain a
  // third view beside them, with its own copy. Nothing here is renamed.
  it("S-25/S-26 amendment: the JSON view and Copy JSON", () => {
    expect([POLICY_TAB.viewSummary, POLICY_TAB.viewYaml, POLICY_DOCUMENT.JSON]).toEqual(["Summary", "YAML", "JSON"]);
    expect([POLICY_TAB.copyYaml, POLICY_DOCUMENT.COPY_JSON]).toEqual(["Copy YAML", "Copy JSON"]);
  });

  it("S-24..S-33: chips, switch, states, hidden, scope, redaction note", () => {
    expect(POLICY_TAB.chipAdded).toBe("Added at start");
    expect(POLICY_TAB.chipRemoved).toBe("Removed at start");
    expect(POLICY_TAB.viewSummary).toBe("Summary");
    expect(POLICY_TAB.viewYaml).toBe("YAML");
    expect(POLICY_TAB.copyYaml).toBe("Copy YAML");
    expect(POLICY_TAB.notYet).toBe(
      "Wardyn records this run's policy when its sandbox is set up. This run hasn't reached that step.",
    );
    expect(POLICY_TAB.never).toBe("This run stopped before its sandbox was set up, so no policy was applied to it.");
    expect(POLICY_TAB.loadError).toBe("Couldn't load this run's policy.");
    expect(POLICY_TAB.incomplete).toBe(
      "This run started before Wardyn recorded each change, so some changes may not be listed.",
    );
    expect(POLICY_TAB.hidden).toBe("Hidden");
    expect(POLICY_TAB.hiddenTip).toBe("Only admins can see this.");
    expect(POLICY_TAB.scope).toBe(
      "Not shown here: hosts approved while the run was running (see Approvals), credentials it was handed (see Credentials on Overview), folders added from a workspace or drive, and Azure DevOps access that came from the connection's defaults.",
    );
    expect(POLICY_TAB.redacted).toBe(
      "Values shown as <redacted> are hidden from you. Fill them in before using this as a policy.",
    );
  });

  it("S-34..S-47: summary sections and rows", () => {
    expect([SUMMARY.network, SUMMARY.barrier, SUMMARY.credentials, SUMMARY.files, SUMMARY.tools, SUMMARY.apps, SUMMARY.limits, SUMMARY.traffic, SUMMARY.ado]).toEqual([
      "Network",
      "Barrier",
      "Credentials",
      "Files and code",
      "Tools and pushes",
      "Apps",
      "Limits",
      "Traffic checks",
      "Azure DevOps access",
    ]);
    expect([SUMMARY.allowedHosts, SUMMARY.blockedHosts, SUMMARY.otherHost, SUMMARY.requestTypes]).toEqual([
      "Allowed hosts",
      "Blocked hosts",
      "Any other host",
      "Request types",
    ]);
    expect([SUMMARY.refused, SUMMARY.refusedThenApproval, SUMMARY.held(30)]).toEqual([
      "Refused",
      "Refused, then sent for approval",
      "Held for up to 30 seconds while someone decides",
    ]);
    expect(SUMMARY.allMethods).toBe("All");
    expect([SUMMARY.minimum, SUMMARY.used]).toEqual(["Minimum", "This run used"]);
    expect(SUMMARY.grantKinds).toEqual({
      github_token: "GitHub access",
      api_key: "API key",
      git_pat: "Git access token",
      ssh_key: "SSH key",
      cloud_sts: "Cloud credentials",
      env_secret: "Environment secret",
    });
    expect(SUMMARY.needsApproval).toBe("Needs approval");
    expect([SUMMARY.folders, SUMMARY.repos, SUMMARY.readOnly]).toEqual(["Folders from the host", "Repositories", "Read-only"]);
    expect([SUMMARY.toolRules, SUMMARY.pushes, SUMMARY.anyBranch, SUMMARY.ownBranch, SUMMARY.pushDeny, SUMMARY.pushHold]).toEqual([
      "Tool rules",
      "Pushes",
      "Any branch",
      "Only this run's own branch",
      "Deny",
      "Hold for review",
    ]);
    expect([SUMMARY.cpu, SUMMARY.memory, SUMMARY.processes, SUMMARY.disk, SUMMARY.idle, SUMMARY.standardLimit]).toEqual([
      "CPU",
      "Memory",
      "Processes",
      "Disk",
      "When idle",
      "Standard limit",
    ]);
    expect([SUMMARY.cpuValue("2"), SUMMARY.mibValue(4096)]).toEqual(["2 CPU", "4096 MiB"]);
    expect([SUMMARY.off, SUMMARY.on, SUMMARY.none]).toEqual(["Off", "On", "None"]);
  });

  it("S-48: the identity row's link", () => {
    expect(IDENTITY_POLICY_VIEW).toBe("View");
  });
});
