/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";

// Route-level code-splitting. Runs is the landing route (every "/" redirects
// there) so it stays eager — lazying it would only add a load waterfall to the
// first paint. Everything else is fetched on navigation, which keeps the heavy
// terminal deps out of the entry chunk: xterm rides on run-detail/workspaces/
// setup (the demo steps' own lazy chunk), asciinema-player on
// run-detail/recordings. Rollup hoists
// what several lazy routes share into its own chunk automatically.
const NewRunRoute = React.lazy(() =>
  import("./components/screens/new-run-flow/new-run-route").then((m) => ({
    default: m.NewRunRoute,
  })),
);
const RunDetailScreen = React.lazy(() =>
  import("./components/screens/run-detail").then((m) => ({
    default: m.RunDetailScreen,
  })),
);
const ApprovalsScreen = React.lazy(() =>
  import("./components/screens/approvals").then((m) => ({
    default: m.ApprovalsScreen,
  })),
);
const PoliciesScreen = React.lazy(() =>
  import("./components/screens/policies").then((m) => ({
    default: m.PoliciesScreen,
  })),
);
const BoundaryScreen = React.lazy(() =>
  import("./components/screens/boundary/boundary-screen").then((m) => ({
    default: m.BoundaryScreen,
  })),
);
const GovernanceScreen = React.lazy(() =>
  import("./components/screens/governance/governance-screen").then((m) => ({
    default: m.GovernanceScreen,
  })),
);
// The admin's drive registry. No nav item and no member path: it is reached
// from the Workspaces header's outline button, the setup Workspaces step's card
// and the Settings card, each of which renders for an operator only.
const DrivesScreen = React.lazy(() =>
  import("./components/screens/drives/drives-screen").then((m) => ({
    default: m.DrivesScreen,
  })),
);
// The org's workspace-provider policy (0.7.2) — git hosts + storage ceilings.
// No nav item and no member path: reached from the funnel's `providers` step
// card and the Settings card, each of which renders for an operator only.
const ProvidersScreen = React.lazy(() =>
  import("./components/screens/providers/providers-screen").then((m) => ({
    default: m.ProvidersScreen,
  })),
);
// A security admin's Images page (design final §3): pre-wrapped images only. A super admin reaches the
// same tab under Providers, so it has no nav item in that tier.
const ImagesScreen = React.lazy(() =>
  import("./components/screens/providers/images-tab").then((m) => ({
    default: m.ImagesScreen,
  })),
);
// The org's custom components (0.8.9): the catalog and who may use each. Super admin only, no nav item:
// reached from the Settings card.
const ComponentsScreen = React.lazy(() =>
  import("./components/screens/components/components-screen").then((m) => ({
    default: m.ComponentsScreen,
  })),
);
// Saved starting points (0.9): /templates in the User view, /admin/templates in the Admin view. The copy
// modules it reads are lazy-only, so the screen is never imported by the entry chunk.
const TemplatesScreen = React.lazy(() =>
  import("./components/screens/templates/templates-screen").then((m) => ({
    default: m.TemplatesScreen,
  })),
);
// The workloads Wardyn keeps running (0.10): /workloads in the User view, /admin/workloads in the Admin view.
const WorkloadsScreen = React.lazy(() =>
  import("./components/screens/workloads").then((m) => ({
    default: m.WorkloadsScreen,
  })),
);
const PermissionsScreen = React.lazy(() =>
  import("./components/screens/permissions").then((m) => ({
    default: m.PermissionsScreen,
  })),
);
// User types (0.8, UT-7a) — the org-defined kinds of person every "Available
// to" control and subject picker names. securityOps server-side, like
// Governance; no member route.
const UserTypesScreen = React.lazy(() =>
  import("./components/screens/user-types/user-types-screen").then((m) => ({
    default: m.UserTypesScreen,
  })),
);
// Credentials (CS-8, design F-1) — who holds a credential for which
// model provider, and offboarding's erase. securityOps server-side, like
// User types; no member route.
const CredentialsScreen = React.lazy(() =>
  import("./components/screens/credentials").then((m) => ({
    default: m.CredentialsScreen,
  })),
);
// People (0.8.6 ppl-p2, mock M12) — everyone with access and every per-person action. securityOps
// server-side, so both admin tiers reach it; no member route.
const PeopleScreen = React.lazy(() =>
  import("./components/screens/people/people-screen").then((m) => ({
    default: m.PeopleScreen,
  })),
);
// Runners (0.9, packet M-RN-1) — the computers people registered to run their own runs. securityOps
// server-side, so both admin tiers reach it; no member route.
const RunnersScreen = React.lazy(() =>
  import("./components/screens/runners/runners-screen").then((m) => ({
    default: m.RunnersScreen,
  })),
);
// A pool's form: an organization pool's edit (super admin) and a person's own user pool, new or edited.
const PoolFormScreen = React.lazy(() =>
  import("./components/screens/runners/pools/pool-form").then((m) => ({
    default: m.PoolFormScreen,
  })),
);
const SecretsScreen = React.lazy(() =>
  import("./components/screens/secrets").then((m) => ({
    default: m.SecretsScreen,
  })),
);
// M-5 (#636): the settings split — one screen per view (§4.3), replacing the
// single SettingsScreen that used to mount unchanged at both routes.
const AdminSettingsScreen = React.lazy(() =>
  import("./components/screens/settings/admin-settings-screen").then((m) => ({
    default: m.AdminSettingsScreen,
  })),
);
const YourAccountScreen = React.lazy(() =>
  import("./components/screens/settings/your-account-screen").then((m) => ({
    default: m.YourAccountScreen,
  })),
);
// /cli/sign-in: the approve page `wardyn login` opens (ATT-LOGIN-UI). Outside the setup gate, like /setup: a
// person finishing a sign-in must reach it whatever the setup checks say.
const CliSignInScreen = React.lazy(() =>
  import("./components/screens/cli-sign-in").then((m) => ({
    default: m.CliSignInScreen,
  })),
);
// /open/{run}/{app}: the link people are given for an inbound app. Outside the setup gate, like /cli/sign-in.
const OpenAppScreen = React.lazy(() =>
  import("./components/screens/open-app").then((m) => ({
    default: m.OpenAppScreen,
  })),
);
const WorkspacesScreen = React.lazy(() =>
  import("./components/screens/workspaces").then((m) => ({
    default: m.WorkspacesScreen,
  })),
);
const WorkspaceDetailScreen = React.lazy(() =>
  import("./components/screens/workspace-detail/workspace-detail").then(
    (m) => ({
      default: m.WorkspaceDetailScreen,
    }),
  ),
);
const AuditScreen = React.lazy(() =>
  import("./components/screens/audit").then((m) => ({
    default: m.AuditScreen,
  })),
);
const RecordingScreen = React.lazy(() =>
  import("./components/screens/recording").then((m) => ({
    default: m.RecordingScreen,
  })),
);
// The guided Getting Started funnel — an operator-chosen route, not a gate:
// no redirect anywhere sends anyone here (see setup-gate.ts). Handles its own
// first-boot Welcome hero vs. the step funnel (onboarding-screen.tsx).
const GettingStarted = React.lazy(() =>
  import("./components/screens/onboarding/onboarding-screen").then((m) => ({
    default: m.GettingStarted,
  })),
);

export {
  NewRunRoute,
  RunDetailScreen,
  ApprovalsScreen,
  PoliciesScreen,
  BoundaryScreen,
  GovernanceScreen,
  DrivesScreen,
  ProvidersScreen,
  ImagesScreen,
  ComponentsScreen,
  TemplatesScreen,
  WorkloadsScreen,
  PermissionsScreen,
  UserTypesScreen,
  CredentialsScreen,
  PeopleScreen,
  RunnersScreen,
  PoolFormScreen,
  SecretsScreen,
  AdminSettingsScreen,
  YourAccountScreen,
  CliSignInScreen,
  OpenAppScreen,
  WorkspacesScreen,
  WorkspaceDetailScreen,
  AuditScreen,
  RecordingScreen,
  GettingStarted,
};
