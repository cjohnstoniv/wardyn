/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Add tools and access' fixtures: GET /me/components as a member sees it (five
// CLI cards, granted org rows only) and the person's saved secret names.

import type { MyComponents, OrgComponentView } from "../../types/components";

export const ORG_LINEAR: OrgComponentView = {
  id: "11111111-1111-4111-8111-111111111111", name: "Linear", category: "mcp", hosts: ["mcp.linear.app"], secrets: [], config_keys: [],
  mcp: { url: "https://mcp.linear.app/mcp", transport: "http" },
};
export const ORG_BILLING: OrgComponentView = {
  id: "22222222-2222-4222-8222-222222222222", name: "Billing API", category: "api", hosts: ["billing.acme.internal"],
  secrets: [{ delivery: { mode: "header", host: "billing.acme.internal" } }], config_keys: [],
};
/** A row whose secret is delivered as an environment variable: refused while the switch is off. */
export const ORG_POSTGRES: OrgComponentView = {
  id: "33333333-3333-4333-8333-333333333333", name: "Postgres", category: "api", hosts: ["db.acme.internal:5432"],
  secrets: [{ delivery: { mode: "env", var: "PGPASSWORD" } }], config_keys: [],
};
export const ORG_AWS_DEV: OrgComponentView = {
  id: "44444444-4444-4444-8444-444444444444", name: "AWS ReadOnly-Dev", category: "cli", hosts: ["*.amazonaws.com"], secrets: [], config_keys: [],
  cli: { kind: "aws", account_id: "123456789012", role_name: "ReadOnly-Dev", region: "us-east-1" },
};

/** A kubectl row (WARDYN_KUBECTL_CLI): one cluster, two namespaces, read only. The API server's host is the row's `cluster`. */
export const ORG_KUBECTL: OrgComponentView = {
  id: "55555555-5555-4555-8555-555555555555", name: "kubectl · staging", category: "cli", hosts: [], secrets: [], config_keys: [],
  cli: { kind: "kubectl", cluster: "api.staging.example.com", namespaces: ["web", "jobs"], access: "read" } as OrgComponentView["cli"],
};
/** A GitHub CLI row: the row carries no host of its own; the server adds api.github.com. */
export const ORG_GITHUB: OrgComponentView = {
  id: "66666666-6666-4666-8666-666666666666", name: "GitHub CLI", category: "cli", hosts: [], secrets: [], config_keys: [],
  cli: { kind: "github", capabilities: ["actions"] },
};

/** Granted: AWS. Not turned on for this person: Azure and GitHub. Not built: gcloud and kubectl. */
export const MY_COMPONENTS: MyComponents = {
  may_define: true,
  resident_delivery_allowed: true,
  autonomy_cap: "",
  mine: [],
  org: [ORG_LINEAR, ORG_BILLING, ORG_POSTGRES, ORG_AWS_DEV],
  clis: [
    { kind: "azure", state: "not_granted", rows: [] },
    { kind: "github", state: "not_granted", rows: [] },
    { kind: "aws", state: "available", rows: [ORG_AWS_DEV.id] },
    { kind: "gcloud", state: "coming_soon", rows: [] },
    { kind: "kubectl", state: "coming_soon", rows: [] },
  ],
};

export const SAVED_SECRETS: string[] = ["linear-token", "billing-key", "deploy-key"];
