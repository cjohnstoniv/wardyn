/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The User view's own Getting Started demos (#637, M-6 D5), split from
// member-getting-started.test.tsx along its describe seam for the file-size cap.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import type { SetupStatus } from "../../../lib/types";
import { WithDoor } from "../../../../test/door-harness";
import { baseMe, baseStatus } from "../../../lib/test-fixtures";

// A provider door's pane starts its sign-in at once; held pending here, so the
// case below ends at the door it opened.
const startSignInMock = vi.fn();
vi.mock("../../../lib/api/model-provider-signin", () => ({
  modelProviderSignIn: {
    startSignIn: (id: string) => {
      startSignInMock(id);
      return new Promise(() => {});
    },
    captureSignIn: vi.fn(),
  },
}));

const getSetupStatusMock = vi.fn();
vi.mock("../../../lib/api/setup", () => ({
  setup: { getSetupStatus: (...a: unknown[]) => getSetupStatusMock(...a) },
}));

// The connect popup + poll (#386), inert here.
const adoConnectMock = vi.fn();
let adoBlockedUrl: string | null = null;
vi.mock("../../../lib/hooks/use-ado-connect", () => ({
  useAdoConnect: () => ({ connecting: false, connect: adoConnectMock, connectFallback: adoConnectMock, blockedUrl: adoBlockedUrl }),
}));

const listSecretsMineMock = vi.fn();
const setSecretMock = vi.fn();
const deleteSecretMock = vi.fn();
vi.mock("../../../lib/api/secrets", () => ({
  secrets: {
    listSecretsMine: (...a: unknown[]) => listSecretsMineMock(...a),
    setSecret: (...a: unknown[]) => setSecretMock(...a),
    deleteSecret: (...a: unknown[]) => deleteSecretMock(...a),
  },
}));

const listRunsMock = vi.fn();
const createRunMock = vi.fn();
const getRunMock = vi.fn();
const killRunMock = vi.fn();
const getGrantsMock = vi.fn();
vi.mock("../../../lib/api/runs", () => ({
  runs: {
    listRuns: (...a: unknown[]) => listRunsMock(...a),
    createRun: (...a: unknown[]) => createRunMock(...a),
    getRun: (...a: unknown[]) => getRunMock(...a),
    killRun: (...a: unknown[]) => killRunMock(...a),
    getGrants: (...a: unknown[]) => getGrantsMock(...a),
  },
}));

// DemoDetail (setup/demos-step.tsx, React.lazy'd below this page's demo
// sections) drags in the same runner graph demos-step.test.tsx stubs —
// xterm, live approvals, audit and the profile sheet are irrelevant to
// whether the RIGHT demo row pre-opens, so they stay inert markers here too.
vi.mock("../../attach-terminal", () => ({
  AttachTerminal: ({ runId }: { runId: string }) => <div data-testid="attach-terminal">{runId}</div>,
}));
vi.mock("../../wardyn/live-approvals", () => ({
  LiveApprovals: ({ runId }: { runId: string }) => <div data-testid="live-approvals">{runId}</div>,
}));
const listAuditMock = vi.fn();
vi.mock("../../../lib/api/audit", () => ({
  audit: { listAudit: (...a: unknown[]) => listAuditMock(...a) },
  egressFromAudit: () => [],
  demoAuditRows: () => [],
}));
vi.mock("../profile-review", () => ({
  ProfileReview: ({ runId }: { runId: string | null }) =>
    runId ? <div data-testid="profile-review">{runId}</div> : null,
}));
vi.mock("sonner", () => ({ toast: { error: vi.fn() } }));

const listKeysMock = vi.fn();
vi.mock("../../../lib/api/ssh-keys", () => ({
  sshKeys: { listKeys: (...a: unknown[]) => listKeysMock(...a) },
}));

const listWorkspacesMock = vi.fn();
vi.mock("../../../lib/api/workspaces", () => ({
  workspaces: {
    listWorkspaces: (...a: unknown[]) => listWorkspacesMock(...a),
    scanWorkspace: vi.fn(),
  },
}));

// The member's own ceiling, named by GET /policies/default. The default answer
// carries no governance_profile_name — an UNASSIGNED member.
const getDefaultPolicyMock = vi.fn();
vi.mock("../../../lib/api/policies", () => ({
  policies: { getDefaultPolicy: (...a: unknown[]) => getDefaultPolicyMock(...a) },
}));

import { MemberGettingStarted } from "./member-getting-started";
import type { Me } from "../../../lib/api/health";
import { OperatorProvider } from "../../wardyn/operator-context";
import { ViewAccessProvider, type ViewAccess } from "../../wardyn/console-view";
import { MEMBER_GETTING_STARTED as T } from "../../wardyn/copy";

function status(overrides: Partial<SetupStatus> = {}): SetupStatus {
  return baseStatus({
    ready: true,
    llm_ready: false,
    runner: { driver: "docker", confinement_classes: ["CC1"] },
    auth: { mode: "local", local_loopback: true },
    ...overrides,
  });
}

// The page reads its drive off the shell's ONE GET /me (operator-context's
// MeIdentity.userDrive), not a fetch of its own — so a case states its /me body
// here, exactly as app-shell hands it down.
// `shell` is the shell's own /setup/status read, behind the one door the
// page's sign-in buttons open (#544) — the page itself reads its own. `search`
// is a `?step=<id>` deep link on the page's own URL.
function renderPage(me: Me = baseMe(), shell: SetupStatus | null = null, search = "", access: ViewAccess = "url") {
  return render(
    <WithDoor status={shell} path={`/setup${search}`} operator={false}>
      <ViewAccessProvider value={access}>
        <OperatorProvider
          operator={false}
          securityOperator={false}
          userDrive={me.user_drive}
          userDriveDeniedByProfile={me.user_drive_denied_by_profile}
        >
          <MemberGettingStarted />
        </OperatorProvider>
      </ViewAccessProvider>
    </WithDoor>,
  );
}

describe("MemberGettingStarted — demos (M-6 D5)", () => {
  beforeEach(() => {
    localStorage.clear();
    listSecretsMineMock.mockReset().mockResolvedValue({ names: [], mine: [] });
    listRunsMock.mockReset().mockResolvedValue([]);
    listKeysMock.mockReset().mockResolvedValue([]);
    listWorkspacesMock.mockReset().mockResolvedValue([]);
    getDefaultPolicyMock.mockReset().mockResolvedValue({ min_confinement_class: "CC1" });
    getRunMock.mockReset().mockResolvedValue(undefined);
    listAuditMock.mockReset().mockResolvedValue([]);
  });

  // The literal redacted payload a non-operator's browser receives.
  const redacted = status({ checks: [], providers: [], secrets: { present: [], github_app: false } });

  it("offers only the demos walkableDemos(status) allows under the redacted payload", async () => {
    getSetupStatusMock.mockResolvedValue(redacted);
    renderPage();
    expect(await screen.findByText(T.DEMOS_EGRESS_TITLE)).toBeInTheDocument();
    // Keyless egress demos: always walkable.
    expect(screen.getByText("The sealed box")).toBeInTheDocument();
    expect(screen.getByText("Record a policy")).toBeInTheDocument();
    // needsModel, unmet under the redacted payload (llmReady false).
    expect(screen.queryByText("The agent in the box")).not.toBeInTheDocument();
    expect(screen.getByText(T.DEMOS_SECRETS_TITLE)).toBeInTheDocument();
    // Keyless secrets demos: always walkable, including the GitHub-App-gated
    // and refusal-only ones (demoMet ignores needsGitHubApp/refusalCompletes).
    expect(screen.getByText("Write-only, even for you")).toBeInTheDocument();
    expect(screen.getByText("A token the sandbox never even sees")).toBeInTheDocument();
    expect(screen.getByText("No identity, no credential")).toBeInTheDocument();
    // needsSecret, unmet — none of the five is in secrets.present.
    expect(screen.queryByText("The key that never enters the box")).not.toBeInTheDocument();
    expect(screen.queryByText("Authorized, not issued")).not.toBeInTheDocument();
    expect(screen.queryByText("A bearer token for a real API")).not.toBeInTheDocument();
    expect(screen.queryByText("A PAT that only ever exists in a pipe")).not.toBeInTheDocument();
    expect(screen.queryByText("The one that touches disk — briefly")).not.toBeInTheDocument();
  });

  it("offers the model- and secret-gated demos once the preconditions they need are met", async () => {
    getSetupStatusMock.mockResolvedValue(
      status({
        providers: [{ tool: "claude", installed: true, logged_in: true, auth_mode: "subscription" }],
        secrets: {
          present: ["wardyn-demo-key", "wardyn-demo-api-token", "wardyn-demo-pat", "wardyn-demo-ssh-key"],
          github_app: false,
        },
      }),
    );
    renderPage();
    expect(await screen.findByText("The agent in the box")).toBeInTheDocument();
    expect(screen.getByText("The key that never enters the box")).toBeInTheDocument();
    expect(screen.getByText("Authorized, not issued")).toBeInTheDocument();
    expect(screen.getByText("A bearer token for a real API")).toBeInTheDocument();
    expect(screen.getByText("A PAT that only ever exists in a pipe")).toBeInTheDocument();
    expect(screen.getByText("The one that touches disk — briefly")).toBeInTheDocument();
  });

  it("?step=<id> pre-opens that row's demo card", async () => {
    getSetupStatusMock.mockResolvedValue(redacted);
    renderPage(baseMe(), null, "?step=sealed-box");
    expect(await screen.findByTestId("demo-card-sealed-box")).toBeInTheDocument();
  });

  it("?step=<id> for a demo this page doesn't offer opens nothing, and errors nothing", async () => {
    getSetupStatusMock.mockResolvedValue(redacted);
    renderPage(baseMe(), null, "?step=agent-in-the-box");
    await screen.findByText(T.DEMOS_EGRESS_TITLE);
    expect(screen.queryByTestId("demo-card-agent-in-the-box")).not.toBeInTheDocument();
  });

  // Owner ruling 2026-09-25: a demo a caller's own ceiling narrows is HIDDEN
  // from this page entirely (no row, no card) rather than offered watch-only.
  // D1 "url" has no ceiling, so every demo stays visible with Start.
  it.each([
    ["session-user", "held-at-the-door", false],
    ["session-user", "record-a-policy", false],
    ["user-only", "held-at-the-door", false],
    ["session-user", "sealed-box", true],
    ["url", "held-at-the-door", true],
    ["url", "record-a-policy", true],
    ["url", "sealed-box", true],
  ] as const)("%s: %s renders (with Start): %s", async (access, id, visible) => {
    getSetupStatusMock.mockResolvedValue(redacted);
    renderPage(baseMe(), null, `?step=${id}`, access);
    if (!visible) {
      await screen.findByText(T.DEMOS_EGRESS_TITLE);
      expect(screen.queryByTestId(`demo-card-${id}`)).not.toBeInTheDocument();
      return;
    }
    const card = await screen.findByTestId(`demo-card-${id}`);
    expect(await within(card).findByTestId(`demo-start-${id}`)).toBeInTheDocument();
  });
});
