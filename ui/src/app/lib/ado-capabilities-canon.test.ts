/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The CANON strings of the owner-approved Azure DevOps per-area packet
// (mock-08/ado-capabilities-per-area-packet.html, strings table), verbatim.
// Canon strings are app strings: a change to any of these is a change to an
// approved mock, not a copy edit.
import { describe, it, expect } from "vitest";
import {
  ADO_CAP_COPY,
  ADO_ENTRA_EDITOR,
  ADO_GROUP_COPY,
} from "./workspace-providers-copy";
import { ADO_ACCESS } from "./ado-access-copy";
import { FIELD_HELP } from "../components/wardyn/policy-field-help";

const CANON_GROUPS: Record<string, { name: string; lead: string }> = {
  repos: { name: "Repos", lead: "Code, branches and pull requests." },
  boards: { name: "Boards", lead: "Work items, queries and boards." },
  wiki: { name: "Wiki", lead: "Project and code wikis." },
  pipelines: {
    name: "Pipelines",
    lead: "Builds, releases, service connections and the library.",
  },
  artifacts: { name: "Artifacts", lead: "Feeds and packages." },
  test_plans: { name: "Test Plans", lead: "Test plans, runs and results." },
  organization: {
    name: "Organization",
    lead: "Projects, people, and reporting across every area.",
  },
};

const CANON_CAPABILITIES: Record<
  string,
  { name: string; consequence: string; ado: string }
> = {
  code_read: {
    name: "Read code",
    consequence:
      "Clone, fetch and browse repositories, commits, branches, pull requests and branch policies; search code.",
    ado: "ADO: vso.code — Git repositories: Read",
  },
  code_write: {
    name: "Push to the run's own branch",
    consequence:
      "Push commits and move branches — nothing outside this run's own branch unless its policy allows any branch.",
    ado: "ADO: vso.code_write — Contribute, Create branch",
  },
  pr: {
    name: "Contribute to pull requests",
    consequence:
      "Open, update, comment on, vote on and complete pull requests — without bypassing a branch policy.",
    ado: "ADO: vso.code_write — Contribute to pull requests",
  },
  policy_admin: {
    name: "Edit branch policies",
    consequence:
      "Create, change or delete branch policies (required reviewers, build validation) — affects every future push and pull request, not just this run's.",
    ado: "ADO: vso.code_write — Edit policies",
  },
  policy_bypass: {
    name: "Bypass policies when completing pull requests",
    consequence:
      "Complete a pull request without its required reviewers or checks.",
    ado: "ADO: vso.code_write — Bypass policies when completing pull requests",
  },
  repo_admin: {
    name: "Create, rename and delete repositories",
    consequence:
      "Create, rename, import into or delete a repository — other people's work included.",
    ado: "ADO: vso.code_manage — Create repository, Rename repository, Delete repository",
  },
  work_read: {
    name: "View work items",
    consequence:
      "Read work items, queries, boards, backlogs, areas and iterations; run queries and search work items.",
    ado: "ADO: vso.work — View work items in this node",
  },
  work_write: {
    name: "Edit work items",
    consequence:
      "Create and update work items — their fields, comments, links and attachments — and saved queries.",
    ado: "ADO: vso.work_write — Edit work items in this node",
  },
  work_admin: {
    name: "Delete work items & manage work tracking",
    consequence:
      "Delete, restore or permanently destroy work items, and change area and iteration paths, fields and tags for everyone.",
    ado: "ADO: vso.work_write — Delete and restore work items, Permanently delete work items, Delete this node, Delete field from organization",
  },
  wiki_read: {
    name: "Read wikis",
    consequence:
      "Read wiki pages, their history and attachments; search wikis.",
    ado: "ADO: vso.wiki — Read (on the wiki's repository)",
  },
  wiki_write: {
    name: "Edit wikis",
    consequence: "Create, edit and delete wiki pages.",
    ado: "ADO: vso.wiki_write — Contribute (on the wiki's repository)",
  },
  build_read: {
    name: "View builds & pipelines",
    consequence: "Read pipelines, runs, builds, logs and artifacts.",
    ado: "ADO: vso.build — View builds, View build pipeline",
  },
  build_execute: {
    name: "Queue builds",
    consequence:
      "Queue a pipeline run, cancel it, or update a build's properties — the pipeline itself runs as its own identity.",
    ado: "ADO: vso.build_execute — Queue builds, Stop builds, Update build information",
  },
  build_admin: {
    name: "Edit build pipelines",
    consequence:
      "Create, change or delete a pipeline definition — what every future run executes.",
    ado: "ADO: vso.build_execute — Edit build pipeline, Delete build pipeline",
  },
  release_read: {
    name: "View releases",
    consequence: "Read classic release pipelines, releases and their stages.",
    ado: "ADO: vso.release — View releases, View release pipeline",
  },
  release_execute: {
    name: "Create releases & deploy",
    consequence: "Create a release, deploy it to a stage, or delete a release.",
    ado: "ADO: vso.release_execute — Create releases, Manage deployments, Delete releases",
  },
  release_admin: {
    name: "Edit release pipelines",
    consequence:
      "Create, change or delete a release pipeline, and answer release approvals — what every future release deploys.",
    ado: "ADO: vso.release_manage — Edit release pipeline, Delete release pipeline, Manage release approvers",
  },
  serviceendpoint_read: {
    name: "View service connections",
    consequence: "Read service connection names, types and settings.",
    ado: "ADO: vso.serviceendpoint — Service connections: Reader",
  },
  serviceendpoint_admin: {
    name: "Manage service connections",
    consequence:
      "Create or change a service connection, including the cloud credential it holds.",
    ado: "ADO: vso.serviceendpoint_manage — Service connections: Administrator",
  },
  library_read: {
    name: "View variable groups & secure files",
    consequence: "Read variable groups and secure-file details.",
    ado: "ADO: vso.variablegroups_read, vso.securefiles_read — Library: Reader",
  },
  packaging_read: {
    name: "Read feeds & packages",
    consequence: "List feeds, and download or restore packages.",
    ado: "ADO: vso.packaging — Feed Reader",
  },
  packaging_write: {
    name: "Publish packages",
    consequence: "Publish, promote, deprecate or unlist a package version.",
    ado: "ADO: vso.packaging_write — Feed Publisher (Contributor)",
  },
  packaging_manage: {
    name: "Delete packages & manage feeds",
    consequence:
      "Delete or unpublish package versions, and create, change or delete feeds, views and their permissions.",
    ado: "ADO: vso.packaging_manage — Feed Owner",
  },
  test_read: {
    name: "View test plans & results",
    consequence: "Read test plans, suites, cases, runs and results.",
    ado: "ADO: vso.test — View test runs",
  },
  project_read: {
    name: "View projects & teams",
    consequence: "Read projects, teams and your own profile.",
    ado: "ADO: vso.project, vso.profile — View project-level information",
  },
  identity_read: {
    name: "Read users & groups",
    consequence:
      "Read the organisation's users, groups, memberships and licences, and directory identities.",
    ado: "ADO: vso.graph, vso.identity, vso.memberentitlementmanagement — Identity: Read",
  },
  project_admin: {
    name: "Manage projects & teams",
    consequence: "Create, rename, change or delete a project or a team.",
    ado: "ADO: vso.project_manage — Create new projects, Rename team project, Delete team project",
  },
  security_admin: {
    name: "Manage permissions & identities",
    consequence:
      "Change who can do what across the whole organisation — permissions, groups, directory identities.",
    ado: "ADO: vso.security_manage, vso.graph_manage, vso.identity_manage — Manage permissions",
  },
};

describe("Azure DevOps per-area packet — canon strings", () => {
  it("names every group as the packet does", () => {
    expect(ADO_GROUP_COPY).toEqual(CANON_GROUPS);
  });

  it("names every capability, its consequence and its Azure DevOps line as the packet does", () => {
    expect(ADO_CAP_COPY).toEqual(CANON_CAPABILITIES);
  });

  it("carries the packet's editor, legend, hint and summary strings", () => {
    expect(ADO_ENTRA_EDITOR.CEILING_TITLE).toBe("What runs may ever do");
    expect(ADO_ENTRA_EDITOR.CEILING_LEAD).toBe(
      "The ceiling. Unchecking one here removes it everywhere — from the default profile below, from a member's own saved policy, and from what a person can ever approve mid-run.",
    );
    expect(ADO_ENTRA_EDITOR.CEILING_LEAD_ADO).toBe(
      "Each row is one Azure DevOps permission; the grey line under it is Azure DevOps' own name for it — the scope Wardyn asks Entra for, then the permission as Project settings shows it. People are asked to consent only to the scopes of the rows on this ceiling.",
    );
    expect(ADO_ENTRA_EDITOR.HIGH_RISK_WARN).toBe(
      "Rows marked High risk can affect repositories, people or runs beyond this one. Grant them deliberately, not as part of a default.",
    );
    expect(ADO_ACCESS.HIGH_RISK_WARN_MEMBER).toBe(
      "High risk rows reach past this one run — grant one only when this run actually needs it.",
    );
    expect(ADO_ENTRA_EDITOR.HIGH_RISK_BADGE).toBe("High risk");
    expect(ADO_ENTRA_EDITOR.DEFAULT_TITLE).toBe("What a run gets by default");
    expect(ADO_ENTRA_EDITOR.DEFAULT_LEAD).toBe(
      "The profile every run on this row starts with, before any saved policy narrows it. Bound to the ceiling above — a capability off the ceiling can't be a default, so its box is disabled here, not just unchecked. Read-and-contribute is the obvious default; nothing High risk ever defaults on.",
    );
    expect(ADO_ENTRA_EDITOR.DEFAULT_OFF_CEILING_TIP).toBe(
      "Off the ceiling — check it above first",
    );
    expect(ADO_ENTRA_EDITOR.DEFAULT_HIGH_RISK_TIP).toBe(
      "On the ceiling, but never defaulted — grant per run instead",
    );
    expect(ADO_ENTRA_EDITOR.DEFAULT_EMPTY_HINT).toBe(
      "Nothing checked means Read code and View projects & teams.",
    );
    expect(ADO_ACCESS.LOCKED).toBe("Not allowed by your administrator.");
    expect(ADO_ACCESS.SUMMARY_EVERY_READ).toBe("Read (every area)");
    expect(
      `${ADO_ENTRA_EDITOR.ERROR_TITLE} ${ADO_ENTRA_EDITOR.ERROR_DEFAULT_OFF_CEILING("Contribute to pull requests")}`,
    ).toBe(
      "These providers can't be saved as written. “Contribute to pull requests” is a default but isn't on the ceiling — uncheck it as a default, or put it back on the ceiling.",
    );
    expect(FIELD_HELP.azure_devops_capabilities.snippet).toEqual([
      "code_read",
      "code_write",
      "pr",
      "project_read",
    ]);
  });
});
