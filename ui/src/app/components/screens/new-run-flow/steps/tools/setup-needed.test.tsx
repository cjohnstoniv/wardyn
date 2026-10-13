/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ADO } from "../../../../../lib/ado-entra-copy";
import { ADO_PAT } from "../../../../../lib/ado-pat-copy";
import { secrets } from "../../../../../lib/api/secrets";
import { HARNESS_NAMES, SCENARIO_ACCESS, SETUP_ITEMS } from "../../../../../lib/new-run-fixtures/scenarios/tools";
import type { SetupItem } from "../../../../../lib/types/preflight";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import type { RunIntent } from "../../draft/run-intent";
import { needsSetup, providerControlId, setupDoor, setupRowId, signInProvider } from "../../lenses/tools-lens";
import { SetupNeeded, type SetupNeededProps } from "./setup-needed";

const openDoor = vi.fn();
vi.mock("../../../../wardyn/model-access-context", () => ({
  useModelAccessDoor: () => ({ openDoor, open: false }),
  useShellSetupStatus: () => ({ status: { model_providers: [{ id: "bedrock", name: "Bedrock" }] }, refresh: () => {} }),
}));

const adoConnect = vi.hoisted(() => ({ connect: vi.fn(), connectFallback: vi.fn(), cancel: vi.fn(), connecting: false, blockedUrl: null as string | null }));
vi.mock("../../../../../lib/hooks/use-ado-connect", () => ({ useAdoConnect: () => adoConnect }));

const BEDROCK_RUN: RunIntent = { tools: [{ id: "claude-code", kind: "harness", model_provider: "bedrock" }] };
const COMPONENT_SECRET: SetupItem = { ...SETUP_ITEMS.addSecret, id: "secret:billing-key", label: "billing-key", required_by: "Billing API", fix: { action: "add_secret", secret_name: "billing-key" } };
// A secret the run names through overrides.agent.add_secrets, as a template or Run again replays it.
const ADDED_SECRET: SetupItem = { ...SETUP_ITEMS.addSecret, id: "secret:sentry-token", label: "sentry-token", required_by: "claude-code", fix: { action: "add_secret", secret_name: "sentry-token" } };

const onDone = vi.fn();
function show(items: SetupItem[], over: Partial<SetupNeededProps> = {}) {
  render(<SetupNeeded items={items} pending={new Set()} done={new Set()} intent={BEDROCK_RUN} names={HARNESS_NAMES} onDone={onDone} {...over} />);
}

let setSecret: Mock<typeof secrets.setSecret>;
beforeEach(() => {
  setSecret = vi.spyOn(secrets, "setSecret").mockResolvedValue(undefined);
});
afterEach(() => {
  cleanup();
  adoConnect.connect.mockReset();
  adoConnect.connectFallback.mockReset();
  adoConnect.blockedUrl = null;
  vi.restoreAllMocks();
  openDoor.mockReset();
  onDone.mockReset();
});

describe("Setup still needed", () => {
  it("says there is nothing to set up when the server lists nothing", () => {
    show([]);
    expect(screen.getByRole("heading", { level: 2, name: NR_TOOLS.SETUP_TITLE })).toBeInTheDocument();
    expect(screen.getByText(NR_TOOLS.SETUP_BODY)).toBeInTheDocument();
    expect(screen.getByText(NR_TOOLS.SETUP_ALL_SET)).toBeInTheDocument();
  });

  it("Choose focuses the harness's model-provider control", async () => {
    render(<select id={providerControlId("codex-cli")} />);
    show(SCENARIO_ACCESS.interactive2.setup);
    expect(screen.getByText(NR_TOOLS.SETUP_CHOOSE_PROVIDER("Codex CLI"))).toBeInTheDocument();
    await userEvent.click(await screen.findByRole("button", { name: NR_TOOLS.SETUP_CHOOSE }));
    expect(document.getElementById(providerControlId("codex-cli"))).toHaveFocus();
  });

  it("Sign in opens the shared model-provider door for the provider chosen for that harness, and is Done once signed in", async () => {
    show([SETUP_ITEMS.signIn]);
    expect(screen.getByText(NR_TOOLS.SETUP_SIGN_IN_TO("Bedrock"))).toBeInTheDocument();
    const trigger = screen.getByRole("button", { name: NR_TOOLS.SETUP_SIGN_IN });
    await userEvent.click(trigger);
    expect(openDoor).toHaveBeenCalledWith(expect.objectContaining({ for: { provider: "bedrock" }, returnTo: trigger }));
    expect(onDone).not.toHaveBeenCalled();
    openDoor.mock.calls[0][0].onSignedIn();
    expect(onDone).toHaveBeenCalledWith(SETUP_ITEMS.signIn.id);
  });

  it("shows an item the server now reports satisfied as Done with no door, and leaves the rest", () => {
    show([SETUP_ITEMS.signIn, COMPONENT_SECRET], { done: new Set([SETUP_ITEMS.signIn.id]) });
    expect(screen.getAllByText(NR_TOOLS.SETUP_DONE)).toHaveLength(1);
    expect(screen.queryByRole("button", { name: NR_TOOLS.SETUP_SIGN_IN })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: NR_TOOLS.SETUP_ADD })).toBeInTheDocument();
  });

  it("shows an item finished here but not yet answered with neither Done nor a door", () => {
    show([SETUP_ITEMS.signIn], { pending: new Set([SETUP_ITEMS.signIn.id]) });
    expect(screen.queryByText(NR_TOOLS.SETUP_DONE)).not.toBeInTheDocument();
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });

  it("never shows a wire id as text when the name is unknown", () => {
    const item: SetupItem = { ...SETUP_ITEMS.chooseProvider, label: "Choose a model provider" };
    show([item, { ...SETUP_ITEMS.signIn, required_by: "claude-code" }], { names: undefined, intent: {} });
    expect(screen.queryByText(/codex-cli|claude-code/)).not.toBeInTheDocument();
  });

  it("gives an item with no door here no button, still listed", () => {
    const scan: SetupItem = { id: "workspace:web", kind: "workspace", label: "Scan the repository", required_by: "acme/web", status: "missing", fix: { action: "scan_workspace", workspace_id: "w1" } };
    const azure: SetupItem = { id: "llm_access:cli", kind: "llm_access", label: "Sign in to Azure", required_by: "Azure CLI", status: "missing", fix: { action: "sign_in", target: "azure_cli", id: "az" } };
    show([scan, azure]);
    expect(screen.getByText("Scan the repository")).toBeInTheDocument();
    expect(screen.getByText("Sign in to Azure")).toBeInTheDocument();
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });
});

// The server's step-5 item for a person's own Azure DevOps lane that is not connected.
const ADO_SIGN_IN: SetupItem = { id: "repo_credential:azure_devops:acme", kind: "repo_credential", label: "Sign in to Azure DevOps", required_by: "acme", status: "missing", fix: { action: "sign_in", target: "azure_devops" } };

describe("the Azure DevOps connect door", () => {
  it("opens the existing connect popup, and is Done once the person reads connected", async () => {
    adoConnect.connect.mockResolvedValue(true);
    show([ADO_SIGN_IN], { intent: {} });
    expect(screen.getByText("Sign in to Azure DevOps")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.MEMBER_CONNECT }));
    expect(adoConnect.connect).toHaveBeenCalledTimes(1);
    expect(openDoor).not.toHaveBeenCalled();
    await waitFor(() => expect(onDone).toHaveBeenCalledWith(ADO_SIGN_IN.id));
  });

  it("is not Done when the person closes the popup, and offers the link when the browser blocked it", async () => {
    adoConnect.connect.mockResolvedValue(false);
    adoConnect.blockedUrl = "/api/v1/scm/azure-devops/signin";
    adoConnect.connectFallback.mockResolvedValue(true);
    show([ADO_SIGN_IN], { intent: {} });
    await userEvent.click(screen.getByRole("button", { name: ADO_PAT.MEMBER_CONNECT }));
    expect(onDone).not.toHaveBeenCalled();
    expect(screen.getByText(ADO.CONNECT_POPUP_BLOCKED, { exact: false })).toBeInTheDocument();
    const link = screen.getByRole("link", { name: ADO.CONNECT_POPUP_OPEN });
    expect(link).toHaveAttribute("href", "/api/v1/scm/azure-devops/signin");
    await userEvent.click(link);
    await waitFor(() => expect(onDone).toHaveBeenCalledWith(ADO_SIGN_IN.id));
  });
});

describe("the add-token door", () => {
  // DR-0121: a secret that comes from an added secret has the same door as a component's.
  it.each([["a component's secret", COMPONENT_SECRET], ["an added secret (Run again, a template)", ADDED_SECRET]])(
    "%s opens a dialog on its first field and saves the token as the named secret",
    async (_name, item) => {
      show([item]);
      await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.SETUP_ADD }));
      const dialog = screen.getByRole("dialog", { name: item.label });
      expect(within(dialog).getByText(NR_TOOLS.TOKEN_INTRO)).toBeInTheDocument();
      const field = within(dialog).getByLabelText(NR_TOOLS.TOKEN_LABEL);
      await waitFor(() => expect(field).toHaveFocus());
      expect(within(dialog).getByRole("button", { name: NR_TOOLS.TOKEN_CONFIRM })).toBeDisabled();
      await userEvent.type(field, "s3cret");
      await userEvent.click(within(dialog).getByRole("button", { name: NR_TOOLS.TOKEN_CONFIRM }));
      await waitFor(() => expect(onDone).toHaveBeenCalledWith(item.id));
      expect(secrets.setSecret).toHaveBeenCalledWith(item.fix?.secret_name, "s3cret");
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    },
  );

  it("keeps the dialog open and says the server's sentence when the save is refused", async () => {
    setSecret.mockRejectedValue(new Error("secret store is read-only"));
    show([COMPONENT_SECRET]);
    await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.SETUP_ADD }));
    await userEvent.type(screen.getByLabelText(NR_TOOLS.TOKEN_LABEL), "tok");
    await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.TOKEN_CONFIRM }));
    expect(await screen.findByRole("alert")).toHaveTextContent("secret store is read-only");
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(onDone).not.toHaveBeenCalled();
  });

  it("Cancel saves nothing", async () => {
    show([COMPONENT_SECRET]);
    await userEvent.click(screen.getByRole("button", { name: NR_TOOLS.SETUP_ADD }));
    await userEvent.type(screen.getByLabelText(NR_TOOLS.TOKEN_LABEL), "tok");
    await userEvent.click(screen.getByRole("button", { name: NR_SHARED.CANCEL }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(secrets.setSecret).not.toHaveBeenCalled();
    expect(onDone).not.toHaveBeenCalled();
  });
});

describe("which items need setup", () => {
  it("only an item the server reports missing does", () => {
    expect(needsSetup(COMPONENT_SECRET)).toBe(true);
    expect(needsSetup({ ...COMPONENT_SECRET, status: "satisfied" })).toBe(false);
    expect(needsSetup({ ...COMPONENT_SECRET, status: "unverified" })).toBe(false);
  });

  it("each row carries the id a link from Review focuses", () => {
    show([COMPONENT_SECRET]);
    expect(document.getElementById(setupRowId(COMPONENT_SECRET))).toBeInTheDocument();
  });
});

describe("setup doors", () => {
  it("maps each fix action to its door, and an add-secret from any source to the add door", () => {
    expect(setupDoor(SETUP_ITEMS.chooseProvider)).toBe("choose");
    expect(setupDoor(SETUP_ITEMS.signIn)).toBe("sign_in");
    expect(setupDoor(ADO_SIGN_IN)).toBe("connect");
    expect(setupDoor({ ...ADO_SIGN_IN, fix: { action: "sign_in", target: "azure_cli" } })).toBeUndefined();
    expect(setupDoor(COMPONENT_SECRET)).toBe("add");
    expect(setupDoor(ADDED_SECRET)).toBe("add");
    expect(setupDoor({ ...COMPONENT_SECRET, fix: { action: "add_secret" } })).toBeUndefined();
    expect(setupDoor({ ...COMPONENT_SECRET, fix: undefined })).toBeUndefined();
  });

  it("signs in to the provider chosen for the harness the item names", () => {
    expect(signInProvider(BEDROCK_RUN, SETUP_ITEMS.signIn)).toBe("bedrock");
    expect(signInProvider({}, SETUP_ITEMS.signIn)).toBeUndefined();
  });
});
