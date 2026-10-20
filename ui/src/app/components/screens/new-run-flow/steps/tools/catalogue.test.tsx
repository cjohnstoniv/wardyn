/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { setField } from "../../../../../../test/set-field";
import { MY_COMPONENTS, ORG_BILLING, ORG_GITHUB, ORG_KUBECTL, ORG_LINEAR, ORG_POSTGRES, SAVED_SECRETS } from "../../../../../lib/new-run-fixtures/scenarios/catalogue";
import { SCENARIO_ACCESS } from "../../../../../lib/new-run-fixtures/scenarios/tools";
import type { MyComponents } from "../../../../../lib/types/components";
import type { AccessFacts, AccessRow } from "../../../../../lib/types/new-run-access";
import { ADD_ACCESS, CUSTOM_COMPONENT as CC, MY_COMPONENTS as MY_COMPONENTS_COPY } from "../../../../wardyn/copy/components";
import { ModelAccessProvider } from "../../../../wardyn/model-access-context";
import type { SetupStatus } from "../../../../../lib/types/setup";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { AnnounceContext } from "../step-shell";
import { CLI_CARD } from "./catalogue-pending";
import { AddTools } from "./catalogue";

const h = vi.hoisted(() => ({ mine: vi.fn(), names: vi.fn(), saveMine: vi.fn(), answered: true, announce: vi.fn(), kubectl: false, repos: [] as string[] }));
vi.mock("../../../../../lib/api/components", () => ({ components: { mine: h.mine, saveMine: h.saveMine } }));
vi.mock("../../../../../lib/api/secrets", () => ({ secrets: { listSecrets: h.names } }));
vi.mock("../../checks/custom-component-dialog", () => ({
  CustomComponentDialog: (p: { context: string; onClose: () => void; initialDraft?: { name: string; hosts: string; secrets: { secret_name: string }[] }; initialKeep?: string }) => (
    <div role="dialog" aria-label="full editor">
      {p.context}
      <button type="button" onClick={p.onClose}>close full editor</button>
      <output data-testid="carried">{JSON.stringify({ name: p.initialDraft?.name, hosts: p.initialDraft?.hosts, secrets: p.initialDraft?.secrets.map((x) => x.secret_name), keep: p.initialKeep })}</output>
    </div>
  ),
}));

const BASE = SCENARIO_ACCESS.member;
const COMPONENTS = (over: Partial<MyComponents> = {}): MyComponents => ({ ...MY_COMPONENTS, ...over });

function Seed({ intent, children }: { intent: RunIntent; children: React.ReactNode }) {
  const dispatch = useNewRunDispatch();
  const [seeded, setSeeded] = React.useState(false);
  React.useEffect(() => { dispatch({ type: "seed", origin: { kind: "import" }, intent }); setSeeded(true); }, [dispatch, intent]);
  return seeded ? <>{children}</> : null;
}
// `access` is the answer for the seeded draft, and stays on screen across edits as a pending preview's rows do.
function Live({ access }: { access: AccessFacts | undefined }) {
  const { intent } = useNewRunDraft();
  const [answeredFor] = React.useState(intent.components);
  return <AddTools access={access} answered={h.answered} intent={intent} answeredFor={answeredFor} githubRepos={h.repos} />;
}
// Edits the draft's second component in place, as a later edit would, without a new answer.
function Replace() {
  const dispatch = useNewRunDispatch();
  const apply = (i: RunIntent): RunIntent => ({ ...i, components: i.components?.map((c, n) => (n === 1 ? { ...c, inline: { hosts: ["edited.example"] } } : c)) });
  return <button type="button" onClick={() => dispatch({ type: "edit", apply })}>replace second</button>;
}
function Draft() {
  const { intent } = useNewRunDraft();
  return <pre data-testid="draft">{JSON.stringify({ components: intent.components, overrides: intent.overrides })}</pre>;
}
async function show(intent: RunIntent = {}, access: AccessFacts | undefined = BASE) {
  const view = render(
    <ModelAccessProvider status={{ features: { kubectl_cli: h.kubectl } } as unknown as SetupStatus} onRefresh={() => {}}>
      <AnnounceContext.Provider value={h.announce}>
        <NewRunDraftProvider owner="alice"><Seed intent={intent}><Live access={access} /><Draft /><Replace /></Seed></NewRunDraftProvider>
      </AnnounceContext.Provider>
    </ModelAccessProvider>,
  );
  await screen.findByRole("heading", { name: NR_TOOLS.ADD_TITLE });
  await waitFor(() => expect(h.mine).toHaveBeenCalled());
  return view;
}
const draft = () => JSON.parse(screen.getByTestId("draft").textContent ?? "{}") as { components?: Record<string, unknown>[]; overrides?: Record<string, unknown> };
const card = (title: string) => within(screen.getByRole("heading", { name: title }).closest("li") as HTMLElement);
const row = (over: Partial<AccessRow>): AccessRow => ({ id: "x", kind: "custom", from: "added", name: "X", status: "ready", hosts: [], credentials: [], source: { kind: "component" }, ...over });

beforeEach(() => {
  h.answered = true;
  h.kubectl = false;
  h.repos = [];
  h.mine.mockResolvedValue(COMPONENTS());
  h.names.mockResolvedValue(SAVED_SECRETS);
  h.saveMine.mockResolvedValue({ id: "saved-1" });
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });

describe("catalogue", () => {
  it("lists five CLI cards with their states, and only the org rows the person is granted", async () => {
    await show();
    expect(await screen.findByRole("heading", { name: "AWS ReadOnly-Dev" })).toBeTruthy();
    for (const kind of ["azure", "github"] as const) expect(card(CLI_CARD[kind].name).getByText("Not turned on for you. Ask your admin.")).toBeTruthy();
    for (const kind of ["gcloud", "kubectl"] as const) {
      expect(card(CLI_CARD[kind].name).getByText(NR_SHARED.COMING_SOON)).toBeTruthy();
      expect(card(CLI_CARD[kind].name).queryByRole("button")).toBeNull();
    }
    expect(card(CLI_CARD.azure.name).queryByRole("button")).toBeNull();
    // The owner's wording (T9): gh reaches Actions and the REST API, not issue or pull request lists.
    expect(card(CLI_CARD.github.name).getByText(NR_TOOLS.CLI_GITHUB_BODY)).toBeTruthy();
    expect(screen.queryByText("Ungranted")).toBeNull();
  });

  it("filters by category", async () => {
    await show();
    await screen.findByRole("heading", { name: "Linear" });
    const filter = within(screen.getByRole("group", { name: NR_TOOLS.CATEGORY_ARIA }));
    await userEvent.click(filter.getByRole("button", { name: NR_TOOLS.CATEGORY_MCP }));
    expect(screen.getByRole("heading", { name: "Linear" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: NR_TOOLS.MCP_CARD_TITLE })).toBeNull();
    expect(screen.queryByRole("heading", { name: CLI_CARD.azure.name })).toBeNull();
    await userEvent.click(filter.getByRole("button", { name: NR_TOOLS.CATEGORY_SECRET }));
    expect(screen.getByRole("heading", { name: NR_TOOLS.SECRET_CARD_TITLE })).toBeTruthy();
    expect(screen.getByRole("heading", { name: NR_TOOLS.HOST_CARD_TITLE })).toBeTruthy();
    expect(screen.getByRole("heading", { name: NR_TOOLS.MCP_CARD_TITLE })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Linear" })).toBeNull();
    await userEvent.click(filter.getByRole("button", { name: NR_TOOLS.CATEGORY_API }));
    expect(screen.getByRole("heading", { name: ORG_BILLING.name })).toBeTruthy();
    // Custom component is the section's own button, whatever the category.
    expect(screen.getByRole("button", { name: NR_TOOLS.CUSTOM_COMPONENT })).toBeTruthy();
  });

  it("shows what each card brings: its hosts, and how its credential reaches the run", async () => {
    await show();
    expect(await card(ORG_BILLING.name).findByText("billing.acme.internal")).toBeTruthy();
    expect(card(ORG_BILLING.name).getByText(NR_TOOLS.DELIVERY_PROXY)).toBeTruthy();
    expect(card(ORG_POSTGRES.name).getByText(NR_TOOLS.DELIVERY_ENV)).toBeTruthy();
    expect(card("AWS ReadOnly-Dev").getByText("*.amazonaws.com")).toBeTruthy();
    expect(card("AWS ReadOnly-Dev").getByText(NR_TOOLS.DELIVERY_SHORT_LIVED)).toBeTruthy();
    expect(card(ORG_LINEAR.name).getByText(NR_TOOLS.FROM_ORG)).toBeTruthy();
  });

  it("a granted Azure CLI row says its credential stays with the proxy", async () => {
    const azure = { id: "55555555-5555-4555-8555-555555555555", name: "Azure Dev", category: "cli" as const, hosts: [], secrets: [], config_keys: [],
      cli: { kind: "azure" as const, subscriptions: 1, access: "read" } };
    h.mine.mockResolvedValue(COMPONENTS({
      org: [...MY_COMPONENTS.org, azure],
      clis: MY_COMPONENTS.clis.map((c) => (c.kind === "azure" ? { ...c, state: "available" as const, rows: [azure.id] } : c)),
    }));
    await show();
    expect(await card("Azure Dev").findByText(NR_TOOLS.DELIVERY_PROXY)).toBeTruthy();
    expect(card("Azure Dev").queryByText(NR_TOOLS.DELIVERY_SHORT_LIVED)).toBeNull();
  });

  // DR-0057: a card's Add is a repeated per-card action, so it is not teal.
  it("draws a card's Add as an outline button", async () => {
    await show();
    const add = await within(await screen.findByRole("heading", { name: "Linear" }).then((el) => el.closest("li") as HTMLElement)).findByRole("button", { name: `${NR_TOOLS.CARD_ADD} Linear` });
    expect(add).not.toHaveClass("bg-primary");
  });

  it("adds an organisation row by its id and removes it again", async () => {
    await show();
    await userEvent.click(await within(await screen.findByRole("heading", { name: "Linear" }).then((el) => el.closest("li") as HTMLElement)).findByRole("button", { name: `${NR_TOOLS.CARD_ADD} Linear` }));
    expect(draft().components).toEqual([{ id: ORG_LINEAR.id }]);
    await userEvent.click(card("Linear").getByRole("button", { name: `${NR_TOOLS.CARD_REMOVE} Linear` }));
    expect(draft().components).toBeUndefined();
  });

  it("shows the server's state for an added row, with its reason as text", async () => {
    const access: AccessFacts = {
      ...BASE,
      rows: [...BASE.rows, row({ id: ORG_BILLING.id, name: "Billing API", status: "needs_setup", reason: "component_unavailable" })],
    };
    await show({ components: [{ id: ORG_BILLING.id }] }, access);
    await screen.findByRole("heading", { name: NR_TOOLS.ADDED_HEADING });
    const added = within(screen.getByText(NR_TOOLS.ADDED_HEADING).parentElement as HTMLElement);
    expect(added.getByText(NR_TOOLS.STATUS_NEEDS_SETUP)).toBeTruthy();
    expect(added.getByText("This component isn't available to you. Ask your admin.")).toBeTruthy();
  });

  describe("resident delivery switched off", () => {
    beforeEach(() => { h.mine.mockResolvedValue(COMPONENTS({ resident_delivery_allowed: false })); });

    it("refuses an organisation row that places its secret in the sandbox, with the reason as text", async () => {
      await show();
      const postgres = card(ORG_POSTGRES.name);
      expect(await postgres.findByText(CC.ERR.RESIDENT_OFF)).toBeTruthy();
      expect(postgres.queryByRole("button", { name: `${NR_TOOLS.CARD_ADD} Postgres` })).toBeNull();
      expect((card("Billing API").getByRole("button", { name: `${NR_TOOLS.CARD_ADD} Billing API` }) as HTMLButtonElement).disabled).toBe(false);
    });

    it("keeps environment variable and file visible but off in Add a secret, and a header still works", async () => {
      await show();
      await userEvent.click(await within(await screen.findByRole("heading", { name: NR_TOOLS.SECRET_CARD_TITLE }).then((el) => el.closest("li") as HTMLElement)).findByRole("button", { name: NR_TOOLS.SECRET_CARD_BUTTON }));
      const dialog = within(await screen.findByRole("dialog", { name: NR_TOOLS.SECRET_CARD_TITLE }));
      expect((dialog.getByRole("radio", { name: NR_TOOLS.DELIVERY_ENV_OPTION }) as HTMLInputElement).disabled).toBe(true);
      expect((dialog.getByRole("radio", { name: NR_TOOLS.DELIVERY_FILE_OPTION }) as HTMLInputElement).disabled).toBe(true);
      expect((dialog.getByRole("radio", { name: NR_TOOLS.DELIVERY_HEADER_OPTION }) as HTMLInputElement).disabled).toBe(false);
      expect(dialog.getByText(CC.ERR.RESIDENT_OFF)).toBeTruthy();
    });
  });
});

describe("Allow a host", () => {
  async function open() {
    await show();
    await userEvent.click(await screen.findByRole("button", { name: NR_TOOLS.HOST_CARD_BUTTON }));
    return within(await screen.findByRole("dialog", { name: NR_TOOLS.HOST_CARD_TITLE }));
  }

  it("takes focus in the host field, refuses an empty or malformed host, and adds one to the overrides", async () => {
    const dialog = await open();
    const field = dialog.getByLabelText(NR_TOOLS.HOST_LABEL);
    expect(document.activeElement).toBe(field);
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(dialog.getByText(NR_TOOLS.HOST_ERR)).toBeTruthy();
    setField(field, "https://pypi.org");
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(dialog.getByRole("alert").textContent).toContain("https://pypi.org");
    setField(field, "pypi.org");
    await userEvent.keyboard("{Enter}");
    expect(draft().overrides).toEqual({ hosts: { add_allowed: ["pypi.org"] } });
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});

describe("Add a secret", () => {
  async function open() {
    await show();
    await userEvent.click(await screen.findByRole("button", { name: NR_TOOLS.SECRET_CARD_BUTTON }));
    return within(await screen.findByRole("dialog", { name: NR_TOOLS.SECRET_CARD_TITLE }));
  }

  it("offers the person's saved names and sends a header secret with its host", async () => {
    const dialog = await open();
    const select = (await dialog.findByLabelText(NR_TOOLS.SECRET_LABEL)) as HTMLSelectElement;
    expect([...select.options].map((o) => o.value)).toEqual(SAVED_SECRETS);
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(dialog.getByText(NR_TOOLS.SECRET_ERR_HOST)).toBeTruthy();
    setField(dialog.getByLabelText(NR_TOOLS.SECRET_HOST_LABEL), "api.linear.app");
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(draft().overrides).toEqual({
      agent: { add_secrets: [{ secret_name: "linear-token", host: "api.linear.app" }] },
      hosts: { add_allowed: ["api.linear.app"] },
    });
  });

  it("says there is nothing to add when the person has no saved secrets, and adds nothing (FR-0110)", async () => {
    h.names.mockResolvedValue([]);
    const dialog = await open();
    expect(dialog.getByText(NR_TOOLS.SECRET_NONE)).toBeTruthy();
    expect(dialog.getByLabelText(NR_TOOLS.SECRET_LABEL)).toBeDisabled();
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(JSON.stringify(draft().overrides ?? {})).not.toContain("add_secrets");
    expect(dialog.queryByText(NR_TOOLS.SECRET_ERR_HOST)).toBeNull();
  });

  it("does not add a host the run already reaches", async () => {
    const dialog = await open();
    const reached = BASE.hosts[0].host;
    setField(await dialog.findByLabelText(NR_TOOLS.SECRET_HOST_LABEL), reached);
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(draft().overrides).toEqual({ agent: { add_secrets: [{ secret_name: "linear-token", host: reached }] } });
  });

  it("places a secret as an environment variable, or as a file named under the secrets directory, by an inline component with no hosts", async () => {
    let dialog = await open();
    await userEvent.click(await dialog.findByRole("radio", { name: NR_TOOLS.DELIVERY_ENV_OPTION }));
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(dialog.getByText(NR_TOOLS.SECRET_ERR_VARIABLE)).toBeTruthy();
    setField(dialog.getByLabelText(NR_TOOLS.SECRET_VARIABLE_LABEL), "LINEAR_TOKEN");
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(draft().components).toEqual([{ name: "linear-token", inline: { hosts: [], secrets: [{ secret_name: "linear-token", delivery: { mode: "env", var: "LINEAR_TOKEN" } }] } }]);
    expect(JSON.stringify(draft().overrides ?? {})).not.toContain("add_secrets");

    dialog = await (async () => {
      await userEvent.click(await screen.findByRole("button", { name: NR_TOOLS.SECRET_CARD_BUTTON }));
      return within(await screen.findByRole("dialog", { name: NR_TOOLS.SECRET_CARD_TITLE }));
    })();
    await userEvent.click(await dialog.findByRole("radio", { name: NR_TOOLS.DELIVERY_FILE_OPTION }));
    setField(dialog.getByLabelText(NR_TOOLS.SECRET_FILE_LABEL), "deploy.key");
    expect(dialog.getByText(NR_TOOLS.SECRET_FILE_PATH("deploy.key"))).toBeTruthy();
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(draft().components?.[1]).toEqual({ name: "linear-token", inline: { hosts: [], secrets: [{ secret_name: "linear-token", delivery: { mode: "file", file: "deploy.key" } }] } });
  });
});

describe("Add an MCP server", () => {
  async function open() {
    await show();
    await userEvent.click(await screen.findByRole("button", { name: NR_TOOLS.MCP_CARD_BUTTON }));
    return within(await screen.findByRole("dialog", { name: NR_TOOLS.MCP_CARD_TITLE }));
  }

  it("adds a remote server by an https URL, its token added by the proxy as a header", async () => {
    const dialog = await open();
    setField(dialog.getByLabelText(NR_TOOLS.MCP_URL), "http://mcp.example.com/mcp");
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(dialog.getByText(NR_TOOLS.MCP_ERR_URL)).toBeTruthy();
    setField(dialog.getByLabelText(NR_TOOLS.MCP_URL), "https://mcp.example.com/mcp");
    setField(dialog.getByLabelText(NR_TOOLS.MCP_NAME), "Docs");
    await userEvent.selectOptions(dialog.getByLabelText(NR_TOOLS.MCP_TOKEN), "linear-token");
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(draft().components).toEqual([{
      name: "Docs",
      inline: {
        hosts: ["mcp.example.com"],
        secrets: [{ secret_name: "linear-token", delivery: { mode: "header", host: "mcp.example.com" } }],
        mcp: { url: "https://mcp.example.com/mcp", transport: "http" },
      },
    }]);
  });
});

describe("A saved-secret read that fails (DR-0252)", () => {
  it("says so with a retry instead of an empty list, keeps a tokenless add working, and offers the names once the retry answers", async () => {
    h.names.mockRejectedValueOnce(new Error("secrets are offline"));
    await show();
    await userEvent.click(await screen.findByRole("button", { name: NR_TOOLS.MCP_CARD_BUTTON }));
    const dialog = within(await screen.findByRole("dialog", { name: NR_TOOLS.MCP_CARD_TITLE }));
    expect((await dialog.findByRole("alert")).textContent).toContain("secrets are offline");
    expect(dialog.getByLabelText(NR_TOOLS.MCP_TOKEN)).toHaveProperty("options.length", 1);
    await userEvent.click(dialog.getByRole("button", { name: MY_COMPONENTS_COPY.RETRY }));
    await waitFor(() => expect(dialog.queryByRole("alert")).toBeNull());
    expect(dialog.getByRole("option", { name: "linear-token" })).toBeTruthy();
    setField(dialog.getByLabelText(NR_TOOLS.MCP_URL), "https://mcp.example.com/mcp");
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(draft().components?.[0].inline).toMatchObject({ mcp: { url: "https://mcp.example.com/mcp" } });
  });

  it("does not say 'no saved secrets' beside the failure notice when the names could not be read (FR-0110, DR-0252)", async () => {
    h.names.mockRejectedValueOnce(new Error("secrets are offline"));
    await show();
    await userEvent.click(await screen.findByRole("button", { name: NR_TOOLS.SECRET_CARD_BUTTON }));
    const dialog = within(await screen.findByRole("dialog", { name: NR_TOOLS.SECRET_CARD_TITLE }));
    expect((await dialog.findByRole("alert")).textContent).toContain("secrets are offline");
    expect(dialog.queryByText(NR_TOOLS.SECRET_NONE)).toBeNull();
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(dialog.queryByText(NR_TOOLS.SECRET_ERR_HOST)).toBeNull();
  });

  it("shows no failure when the read succeeds with no names", async () => {
    h.names.mockResolvedValueOnce([]);
    await show();
    await userEvent.click(await screen.findByRole("button", { name: NR_TOOLS.MCP_CARD_BUTTON }));
    const dialog = within(await screen.findByRole("dialog", { name: NR_TOOLS.MCP_CARD_TITLE }));
    expect(dialog.queryByRole("alert")).toBeNull();
  });
});

describe("MCP by URL against the server's reading of the URL", () => {
  async function add(url: string) {
    await show();
    await userEvent.click(await screen.findByRole("button", { name: NR_TOOLS.MCP_CARD_BUTTON }));
    const dialog = within(await screen.findByRole("dialog", { name: NR_TOOLS.MCP_CARD_TITLE }));
    setField(dialog.getByLabelText(NR_TOOLS.MCP_URL), url);
    await userEvent.selectOptions(dialog.getByLabelText(NR_TOOLS.MCP_TOKEN), "linear-token");
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    return dialog;
  }

  it("names a non-default port in the component's hosts and keeps the bare host for the header", async () => {
    await add("https://mcp.example.com:8443/mcp");
    expect(draft().components).toEqual([{
      name: "mcp.example.com",
      inline: {
        hosts: ["mcp.example.com:8443"],
        secrets: [{ secret_name: "linear-token", delivery: { mode: "header", host: "mcp.example.com" } }],
        mcp: { url: "https://mcp.example.com:8443/mcp", transport: "http" },
      },
    }]);
  });

  it("writes :443 as the bare host, as the server reads it", async () => {
    await add("https://mcp.example.com:443/mcp");
    expect((draft().components?.[0].inline as { hosts: string[] }).hosts).toEqual(["mcp.example.com"]);
  });

  it("refuses a URL with a fragment before it reaches the server", async () => {
    const dialog = await add("https://mcp.example.com/mcp#section");
    expect(dialog.getByText(NR_TOOLS.MCP_ERR_URL)).toBeTruthy();
    expect(draft().components).toBeUndefined();
  });
});

describe("Custom component", () => {
  async function open() {
    await show();
    await userEvent.click(await screen.findByRole("button", { name: NR_TOOLS.CUSTOM_COMPONENT }));
    return within(await screen.findByRole("dialog", { name: NR_TOOLS.CUSTOM_COMPONENT }));
  }

  it("asks for a header host only with two or more hosts, and sends the host the header goes to (DR-0050)", async () => {
    const dialog = await open();
    setField(dialog.getByLabelText(NR_TOOLS.CC_NAME), "Billing");
    setField(dialog.getByLabelText(NR_TOOLS.CC_HOSTS), "billing.acme.internal");
    await userEvent.selectOptions(dialog.getByLabelText(NR_TOOLS.CC_SECRET), "billing-key");
    expect(dialog.queryByLabelText(NR_TOOLS.CC_HEADER_HOST)).toBeNull();
    setField(dialog.getByLabelText(NR_TOOLS.CC_HOSTS), "billing.acme.internal, api.stripe.com");
    const hostSelect = dialog.getByLabelText(NR_TOOLS.CC_HEADER_HOST);
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(dialog.getByRole("alert").textContent).toBe(CC.ERR.HEADER_HOST);
    await userEvent.selectOptions(hostSelect, "api.stripe.com");
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    expect(draft().components).toEqual([{
      name: "Billing",
      inline: { hosts: ["billing.acme.internal", "api.stripe.com"], secrets: [{ secret_name: "billing-key", delivery: { mode: "header", host: "api.stripe.com" } }] },
    }]);
    expect(h.saveMine).not.toHaveBeenCalled();
  });

  it("saves to the person's components when asked, and attaches the saved row by id", async () => {
    const dialog = await open();
    setField(dialog.getByLabelText(NR_TOOLS.CC_NAME), "Billing");
    setField(dialog.getByLabelText(NR_TOOLS.CC_HOSTS), "billing.acme.internal");
    await userEvent.click(dialog.getByLabelText(NR_TOOLS.CC_SAVE));
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.ADD_TO_RUN }));
    await waitFor(() => expect(draft().components).toEqual([{ id: "saved-1" }]));
    expect(h.saveMine).toHaveBeenCalledWith({ name: "Billing", definition: { hosts: ["billing.acme.internal"] } });
  });

  it("opens the full editor, imported rather than copied, for more options", async () => {
    const dialog = await open();
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.CC_MORE_OPTIONS }));
    expect(await screen.findByRole("dialog", { name: "full editor" })).toBeTruthy();
  });

  it("returns focus to the Custom component button once the full editor is closed", async () => {
    const dialog = await open();
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.CC_MORE_OPTIONS }));
    await userEvent.click(await screen.findByRole("button", { name: "close full editor" }));
    await waitFor(() => expect(screen.getByRole("button", { name: NR_TOOLS.CUSTOM_COMPONENT })).toHaveFocus());
  });

  it("carries the typed form and the save choice into the full editor (DR-0251)", async () => {
    const dialog = await open();
    setField(dialog.getByLabelText(NR_TOOLS.CC_NAME), "Billing");
    setField(dialog.getByLabelText(NR_TOOLS.CC_HOSTS), "billing.acme.internal");
    await userEvent.selectOptions(dialog.getByLabelText(NR_TOOLS.CC_SECRET), "billing-key");
    await userEvent.click(dialog.getByLabelText(NR_TOOLS.CC_SAVE));
    await userEvent.click(dialog.getByRole("button", { name: NR_TOOLS.CC_MORE_OPTIONS }));
    await screen.findByRole("dialog", { name: "full editor" });
    expect(JSON.parse(screen.getByTestId("carried").textContent ?? "{}")).toEqual({ name: "Billing", hosts: "billing.acme.internal", secrets: ["billing-key"], keep: "save" });
  });

  it("is held, with its reason, for a person who may not define components, and so is an MCP server by URL", async () => {
    h.mine.mockResolvedValue(COMPONENTS({ may_define: false }));
    await show();
    await screen.findByRole("heading", { name: "Linear" });
    for (const name of [NR_TOOLS.CUSTOM_COMPONENT, NR_TOOLS.MCP_CARD_BUTTON]) {
      const button = screen.getByRole("button", { name }) as HTMLButtonElement;
      expect(button.disabled).toBe(true);
      expect(button).toHaveAccessibleDescription(ADD_ACCESS.NOT_ALLOWED);
    }
  });
});

describe("while the next answer is out", () => {
  it("shows Requested in place of Ready or a reason, on an attached row and an added row", async () => {
    h.answered = false;
    const access: AccessFacts = {
      ...BASE,
      rows: [...BASE.rows, row({ id: ORG_BILLING.id, name: "Billing API", status: "ready" }), row({ id: "host:pypi.org", kind: "host", name: "pypi.org", status: "needs_setup", reason: "component_unavailable" })],
    };
    await show({ components: [{ id: ORG_BILLING.id }] }, access);
    await screen.findByRole("heading", { name: NR_TOOLS.ADDED_HEADING });
    expect(screen.queryByText(NR_TOOLS.STATUS_READY)).toBeNull();
    expect(screen.queryByText(NR_TOOLS.STATUS_NEEDS_SETUP)).toBeNull();
    expect(screen.queryByText("This component isn't available to you. Ask your admin.")).toBeNull();
    expect(card("Billing API").getByText(NR_TOOLS.NET_REQUESTED)).toBeTruthy();
    expect(screen.getAllByText(NR_TOOLS.NET_REQUESTED).length).toBeGreaterThanOrEqual(3);
  });
});

describe("a catalogue that cannot be read", () => {
  it("says why, and the greyed Custom component points at that sentence rather than reading as a refusal", async () => {
    h.mine.mockRejectedValue(new Error("Wardyn couldn't load your components."));
    await show();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Wardyn couldn't load your components.");
    const custom = screen.getByRole("button", { name: NR_TOOLS.CUSTOM_COMPONENT });
    expect(custom).toBeDisabled();
    expect(custom).toHaveAttribute("aria-describedby", alert.id);
  });
});

describe("Added to this run", () => {
  it("removes a host, a header secret and a component by the carrier that added each", async () => {
    const access: AccessFacts = {
      ...BASE,
      rows: [
        ...BASE.rows,
        row({ id: "host:pypi.org", kind: "host", name: "pypi.org" }),
        row({ id: "secret:linear-token", kind: "secret", name: "linear-token" }),
        row({ id: "inline:0", kind: "custom", name: "Billing" }),
      ],
    };
    await show({
      components: [{ name: "Billing", inline: { hosts: ["b.example"] } }],
      overrides: { hosts: { add_allowed: ["pypi.org"] }, agent: { add_secrets: [{ secret_name: "linear-token", host: "api.linear.app" }] } },
    }, access);
    await userEvent.click(await screen.findByRole("button", { name: `${NR_TOOLS.ROW_REMOVE} pypi.org` }));
    expect(draft().overrides).toEqual({ hosts: { add_allowed: [] }, agent: { add_secrets: [{ secret_name: "linear-token", host: "api.linear.app" }] } });
    await userEvent.click(screen.getByRole("button", { name: `${NR_TOOLS.ROW_REMOVE} linear-token` }));
    expect(draft().overrides).toEqual({ hosts: { add_allowed: [] }, agent: {} });
    await userEvent.click(screen.getByRole("button", { name: `${NR_TOOLS.ROW_REMOVE} Billing` }));
    expect(draft().components).toBeUndefined();
  });

  it("removes an inline component added without a name, and only that one", async () => {
    const access: AccessFacts = {
      ...BASE,
      rows: [...BASE.rows, row({ id: "inline:0", kind: "custom", name: "" }), row({ id: "inline:1", kind: "custom", name: "Billing" })],
    };
    await show({ components: [{ inline: { hosts: ["api.example.com"] } }, { name: "Billing", inline: { hosts: ["b.example"] } }] }, access);
    const unnamed = (await screen.findAllByRole("button", { name: NR_TOOLS.ROW_REMOVE })).find((b) => b.getAttribute("aria-label")?.trim() === NR_TOOLS.ROW_REMOVE);
    await userEvent.click(unnamed!);
    expect(draft().components).toEqual([{ name: "Billing", inline: { hosts: ["b.example"] } }]);
  });

  describe("while the next preview is out, a row removes the component it was answered for", () => {
    const A = { name: "Alpha", inline: { hosts: ["a.example"] } };
    const B = { name: "Beta", inline: { hosts: ["b.example"] } };
    const C = { name: "Gamma", inline: { hosts: ["c.example"] } };
    const rows = (...names: string[]) => names.map((name, i) => row({ id: `inline:${i}`, kind: "custom", name }));
    const remove = (name: string) => userEvent.click(screen.getByRole("button", { name: `${NR_TOOLS.ROW_REMOVE} ${name}` }));

    it("removing the first and then the old second row removes the second, never the third", async () => {
      await show({ components: [A, B, C] }, { ...BASE, rows: [...BASE.rows, ...rows("Alpha", "Beta", "Gamma")] });
      await screen.findByRole("button", { name: `${NR_TOOLS.ROW_REMOVE} Alpha` });
      await remove("Alpha");
      await remove("Beta");
      expect(draft().components).toEqual([C]);
    });

    it("a second press on the same row removes nothing more", async () => {
      await show({ components: [A, B, C] }, { ...BASE, rows: [...BASE.rows, ...rows("Alpha", "Beta", "Gamma")] });
      await screen.findByRole("button", { name: `${NR_TOOLS.ROW_REMOVE} Alpha` });
      await remove("Alpha");
      await remove("Alpha");
      expect(draft().components).toEqual([B, C]);
    });

    it("a repeated press announces nothing", async () => {
      await show({ components: [A, B, C] }, { ...BASE, rows: [...BASE.rows, ...rows("Alpha", "Beta", "Gamma")] });
      await screen.findByRole("button", { name: `${NR_TOOLS.ROW_REMOVE} Alpha` });
      await remove("Alpha");
      await remove("Alpha");
      expect(h.announce.mock.calls.filter(([m]) => m === NR_TOOLS.COMPONENT_REMOVED)).toHaveLength(1);
    });

    it("a row whose component was edited since the answer removes nothing", async () => {
      await show({ components: [A, B] }, { ...BASE, rows: [...BASE.rows, ...rows("Alpha", "Beta")] });
      await screen.findByRole("button", { name: `${NR_TOOLS.ROW_REMOVE} Beta` });
      await userEvent.click(screen.getByRole("button", { name: "replace second" }));
      const edited = draft().components;
      await remove("Beta");
      expect(draft().components).toEqual(edited);
      expect(edited).toEqual([A, { ...B, inline: { hosts: ["edited.example"] } }]);
    });

    it("stored and inline components side by side: each row removes its own", async () => {
      const stored = { id: ORG_BILLING.id };
      const access: AccessFacts = { ...BASE, rows: [...BASE.rows, row({ id: ORG_BILLING.id, name: "Stored" }), row({ id: "inline:1", kind: "custom", name: "Beta" }), row({ id: "inline:2", kind: "custom", name: "Gamma" })] };
      await show({ components: [stored, B, C] }, access);
      await screen.findByRole("button", { name: `${NR_TOOLS.ROW_REMOVE} Stored` });
      await remove("Stored");
      await remove("Gamma");
      expect(draft().components).toEqual([B]);
    });
  });
});

describe("what a tool card brings", () => {
  const catalogue = (clis: Partial<Record<"kubectl" | "github", string[]>>) => {
    const rows = [ORG_KUBECTL, ORG_GITHUB].filter((r) => clis[r.cli?.kind as "kubectl" | "github"]);
    return COMPONENTS({
      org: [...MY_COMPONENTS.org, ...rows],
      clis: MY_COMPONENTS.clis.map((c) => (c.kind === "kubectl" || c.kind === "github") && clis[c.kind] ? { ...c, state: "available" as const, rows: clis[c.kind] ?? [] } : c),
    });
  };
  const dd = (title: string) => [...(screen.getByRole("heading", { name: title }).closest("li") as HTMLElement).querySelectorAll("dl dd")].map((n) => n.textContent);
  const dts = (title: string) => [...(screen.getByRole("heading", { name: title }).closest("li") as HTMLElement).querySelectorAll("dl dt")].map((n) => n.textContent);
  const details = (title: string) => userEvent.click(card(title).getByRole("button", { name: NR_TOOLS.DETAILS_FOR(title) }));

  describe("kubectl", () => {
    beforeEach(() => { h.kubectl = true; h.mine.mockResolvedValue(catalogue({ kubectl: [ORG_KUBECTL.id] })); });

    it("shows the cluster, its service account, no files, and the namespaces with read only and no exec", async () => {
      await show();
      await screen.findByRole("heading", { name: ORG_KUBECTL.name });
      expect(dts(ORG_KUBECTL.name)).toEqual([NR_TOOLS.NETWORK_TITLE, NR_TOOLS.CREDENTIALS_TITLE, NR_TOOLS.DETAIL_FILES, NR_TOOLS.DETAIL_OTHER]);
      expect(dd(ORG_KUBECTL.name)).toEqual([
        "api.staging.example.com",
        `${NR_TOOLS.KUBE_CREDENTIAL("staging")}${NR_TOOLS.DELIVERY_PROXY}`,
        NR_TOOLS.DETAIL_NONE,
        `${NR_TOOLS.KUBE_NAMESPACES("web, jobs")} · ${NR_TOOLS.KUBE_READ_ONLY} · ${NR_TOOLS.KUBE_NO_EXEC}`,
      ]);
    });

    it("says the cluster comes from your organisation, and the grid never forces a card wider than a phone", async () => {
      await show();
      const heading = await screen.findByRole("heading", { name: ORG_KUBECTL.name });
      expect(heading.closest("li")?.textContent).toContain(NR_TOOLS.FROM_ORG);
      const grid = screen.getByTestId("nr-tool-grid");
      expect(grid.className).toContain("minmax(min(320px,100%),1fr)");
      expect(grid.className).toContain("items-start");
    });

    it("says read and write for a row that may change things", async () => {
      const write = { ...ORG_KUBECTL, cli: { ...ORG_KUBECTL.cli!, access: "write", namespaces: ["sandbox"] } };
      h.mine.mockResolvedValue({ ...catalogue({ kubectl: [ORG_KUBECTL.id] }), org: [write] });
      await show();
      await screen.findByRole("heading", { name: ORG_KUBECTL.name });
      expect(dd(ORG_KUBECTL.name)[3]).toBe(`${NR_TOOLS.KUBE_NAMESPACES("sandbox")} · ${NR_TOOLS.KUBE_READ_WRITE} · ${NR_TOOLS.KUBE_NO_EXEC}`);
    });

    it("opens the whole breakdown from Details for the card, and Add and Remove still work", async () => {
      await show();
      await screen.findByRole("heading", { name: ORG_KUBECTL.name });
      await details(ORG_KUBECTL.name);
      const dialog = within(await screen.findByRole("dialog", { name: ORG_KUBECTL.name }));
      // Once as the host the run may reach, once as the cluster's address.
      expect(dialog.getAllByText("api.staging.example.com")).toHaveLength(2);
      expect(dialog.getByText(NR_TOOLS.KUBE_CREDENTIAL_NOTE)).toBeTruthy();
      expect(dialog.getByText(NR_TOOLS.KUBE_READ_DETAIL)).toBeTruthy();
      expect(dialog.getByText(NR_TOOLS.KUBE_EXEC_DETAIL)).toBeTruthy();
      expect(dialog.getByText("web, jobs")).toBeTruthy();
      await userEvent.click(dialog.getAllByRole("button", { name: "Close" })[0]);
      await userEvent.click(card(ORG_KUBECTL.name).getByRole("button", { name: `${NR_TOOLS.CARD_ADD} ${ORG_KUBECTL.name}` }));
      expect(draft().components).toEqual([{ id: ORG_KUBECTL.id }]);
      expect(card(ORG_KUBECTL.name).getByRole("button", { name: NR_TOOLS.DETAILS_FOR(ORG_KUBECTL.name) })).toBeTruthy();
    });

    it("gives each host in Details the server's verdict once the row is on the run, not a blanket Allowed", async () => {
      const access: AccessFacts = {
        ...BASE,
        hosts: [...BASE.hosts, { host: "api.staging.example.com", via: "injected", rows: [ORG_KUBECTL.id], verdict: "not_allowed", reason: "clamped", source: { kind: "policy" } }],
        rows: [...BASE.rows, row({ id: ORG_KUBECTL.id, kind: "cli", name: ORG_KUBECTL.name })],
      };
      await show({ components: [{ id: ORG_KUBECTL.id }] }, access);
      await screen.findByRole("heading", { name: ORG_KUBECTL.name });
      await details(ORG_KUBECTL.name);
      const dialog = within(await screen.findByRole("dialog", { name: ORG_KUBECTL.name }));
      expect(dialog.getByText(NR_TOOLS.NET_NOT_ALLOWED)).toBeTruthy();
      expect(dialog.queryByText(NR_TOOLS.NET_ALLOWED)).toBeNull();
    });

    it("is Coming soon, with no breakdown, while the server's switch is off or silent", async () => {
      h.kubectl = false;
      await show();
      await screen.findByRole("heading", { name: CLI_CARD.kubectl.name });
      expect(card(CLI_CARD.kubectl.name).getByText(NR_SHARED.COMING_SOON)).toBeTruthy();
      expect(document.querySelector(`dl[aria-label="${NR_TOOLS.BRINGS_FOR(CLI_CARD.kubectl.name)}"]`)).toBeNull();
      expect(screen.queryByRole("heading", { name: ORG_KUBECTL.name })).toBeNull();
    });

    it("shows no breakdown while the card is not granted", async () => {
      h.mine.mockResolvedValue(COMPONENTS({ clis: MY_COMPONENTS.clis.map((c) => (c.kind === "kubectl" ? { ...c, state: "not_granted" as const } : c)) }));
      await show();
      await screen.findByRole("heading", { name: CLI_CARD.kubectl.name });
      expect(card(CLI_CARD.kubectl.name).queryByRole("button")).toBeNull();
      expect(card(CLI_CARD.kubectl.name).getByText("Not turned on for you. Ask your admin.")).toBeTruthy();
      for (const title of [CLI_CARD.kubectl.name, CLI_CARD.github.name]) expect(dts(title)).toEqual([]);
    });
  });

  describe("GitHub CLI", () => {
    beforeEach(() => { h.mine.mockResolvedValue(catalogue({ github: [ORG_GITHUB.id] })); });

    it("acts on the run's GitHub repositories through the Wardyn GitHub App, reaching api.github.com", async () => {
      h.repos = ["acme/web-app", "acme/api"];
      await show();
      await screen.findByRole("heading", { name: ORG_GITHUB.name });
      expect(dd(ORG_GITHUB.name)).toEqual([
        "api.github.com",
        `${NR_TOOLS.GITHUB_APP}${NR_TOOLS.DELIVERY_PROXY}`,
        NR_TOOLS.DETAIL_NONE,
        NR_TOOLS.GITHUB_REPOS("acme/web-app, acme/api"),
      ]);
    });

    it("ends a long repository list with how many more, and Details lists them all", async () => {
      h.repos = ["acme/a", "acme/b", "acme/c"];
      await show();
      await screen.findByRole("heading", { name: ORG_GITHUB.name });
      expect(dd(ORG_GITHUB.name)[3]).toBe(`${NR_TOOLS.GITHUB_REPOS("acme/a, acme/b")} ${NR_TOOLS.MORE(1)}`);
      await details(ORG_GITHUB.name);
      const dialog = within(await screen.findByRole("dialog", { name: ORG_GITHUB.name }));
      for (const r of h.repos) expect(dialog.getByText(r)).toBeTruthy();
      expect(dialog.getByText(NR_TOOLS.DELIVERY_GITHUB)).toBeTruthy();
    });

    it("brings nothing without a GitHub repository, and offers no Details", async () => {
      await show();
      await screen.findByRole("heading", { name: ORG_GITHUB.name });
      expect(dd(ORG_GITHUB.name)).toEqual(Array(4).fill(NR_TOOLS.DETAIL_NONE));
      expect(card(ORG_GITHUB.name).queryByRole("button", { name: NR_TOOLS.DETAILS_FOR(ORG_GITHUB.name) })).toBeNull();
    });

    it("brings nothing once the server says the run has no GitHub repository, beside its own sentence", async () => {
      const access: AccessFacts = { ...BASE, rows: [...BASE.rows, row({ id: ORG_GITHUB.id, name: ORG_GITHUB.name, status: "unavailable", reason: "cli_no_github_repo" })] };
      await show({ components: [{ id: ORG_GITHUB.id }] }, access);
      await screen.findByRole("heading", { name: ORG_GITHUB.name });
      expect(dd(ORG_GITHUB.name)).toEqual(Array(4).fill(NR_TOOLS.DETAIL_NONE));
      expect(card(ORG_GITHUB.name).getByText("GitHub CLI works only with this run's GitHub repositories. Add one on the Files step.")).toBeTruthy();
      expect(card(ORG_GITHUB.name).queryByRole("button", { name: NR_TOOLS.DETAILS_FOR(ORG_GITHUB.name) })).toBeNull();
      expect(card(ORG_GITHUB.name).getByRole("button", { name: `${NR_TOOLS.CARD_REMOVE} ${ORG_GITHUB.name}` })).toBeTruthy();
    });
  });

  describe("remote MCP server", () => {
    it("shows its host, how its credential is delivered, and what kind of server it is", async () => {
      const linear = { ...ORG_LINEAR, secrets: [{ delivery: { mode: "header" as const, host: "mcp.linear.app" } }] };
      h.mine.mockResolvedValue({ ...COMPONENTS(), org: [linear] });
      await show();
      await screen.findByRole("heading", { name: "Linear" });
      expect(dd("Linear")).toEqual(["mcp.linear.app", NR_TOOLS.DELIVERY_PROXY, NR_TOOLS.DETAIL_NONE, `${NR_TOOLS.FROM_MCP} · ${NR_TOOLS.MCP_REMOTE}`]);
      await details("Linear");
      const dialog = within(await screen.findByRole("dialog", { name: "Linear" }));
      expect(dialog.getByText(NR_TOOLS.DETAIL_WHERE)).toBeTruthy();
      expect(dialog.getByText("https://mcp.linear.app/mcp")).toBeTruthy();
      expect(dialog.getByText(NR_TOOLS.DETAIL_MCP_BODY)).toBeTruthy();
    });

    it("shows no breakdown and no Details on a server that cannot be added", async () => {
      const envMcp = { ...ORG_LINEAR, secrets: [{ delivery: { mode: "env" as const, var: "LINEAR_TOKEN" } }] };
      h.mine.mockResolvedValue({ ...COMPONENTS({ resident_delivery_allowed: false }), org: [envMcp] });
      await show();
      expect(await card("Linear").findByText(CC.ERR.RESIDENT_OFF)).toBeTruthy();
      expect(dts("Linear")).toEqual([]);
      expect(card("Linear").queryByRole("button", { name: NR_TOOLS.DETAILS_FOR("Linear") })).toBeNull();
    });

    it("keeps its chips, with no breakdown, when it cannot be added", async () => {
      h.mine.mockResolvedValue(COMPONENTS({ resident_delivery_allowed: false }));
      await show();
      await screen.findByRole("heading", { name: ORG_POSTGRES.name });
      // An API row keeps today's chips; no card that cannot be added shows a breakdown.
      expect(card(ORG_POSTGRES.name).getByText(NR_TOOLS.DELIVERY_ENV)).toBeTruthy();
      expect(dts(ORG_POSTGRES.name)).toEqual([]);
    });
  });
});
