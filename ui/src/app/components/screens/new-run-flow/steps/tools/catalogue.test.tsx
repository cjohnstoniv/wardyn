/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { setField } from "../../../../../../test/set-field";
import { MY_COMPONENTS, ORG_BILLING, ORG_LINEAR, ORG_POSTGRES, SAVED_SECRETS } from "../../../../../lib/new-run-fixtures/scenarios/catalogue";
import { SCENARIO_ACCESS } from "../../../../../lib/new-run-fixtures/scenarios/tools";
import type { MyComponents } from "../../../../../lib/types/components";
import type { AccessFacts, AccessRow } from "../../../../../lib/types/new-run-access";
import { ADD_ACCESS, CUSTOM_COMPONENT as CC } from "../../../../wardyn/copy/components";
import { NR_SHARED } from "../../../../wardyn/copy/new-run/shared";
import { NR_TOOLS } from "../../../../wardyn/copy/new-run/tools";
import type { RunIntent } from "../../draft/run-intent";
import { NewRunDraftProvider, useNewRunDispatch, useNewRunDraft } from "../../new-run-route";
import { CLI_CARD } from "./catalogue-pending";
import { AddTools } from "./catalogue";

const h = vi.hoisted(() => ({ mine: vi.fn(), names: vi.fn(), saveMine: vi.fn(), answered: true }));
vi.mock("../../../../../lib/api/components", () => ({ components: { mine: h.mine, saveMine: h.saveMine } }));
vi.mock("../../../../../lib/api/secrets", () => ({ secrets: { listSecrets: h.names } }));
vi.mock("../../checks/custom-component-dialog", () => ({
  CustomComponentDialog: (p: { context: string }) => <div role="dialog" aria-label="full editor">{p.context}</div>,
}));

const BASE = SCENARIO_ACCESS.member;
const COMPONENTS = (over: Partial<MyComponents> = {}): MyComponents => ({ ...MY_COMPONENTS, ...over });

function Seed({ intent, children }: { intent: RunIntent; children: React.ReactNode }) {
  const dispatch = useNewRunDispatch();
  const [seeded, setSeeded] = React.useState(false);
  React.useEffect(() => { dispatch({ type: "seed", origin: { kind: "import" }, intent }); setSeeded(true); }, [dispatch, intent]);
  return seeded ? <>{children}</> : null;
}
function Live({ access }: { access: AccessFacts | undefined }) {
  const { intent } = useNewRunDraft();
  return <AddTools access={access} answered={h.answered} intent={intent} />;
}
function Draft() {
  const { intent } = useNewRunDraft();
  return <pre data-testid="draft">{JSON.stringify({ components: intent.components, overrides: intent.overrides })}</pre>;
}
async function show(intent: RunIntent = {}, access: AccessFacts | undefined = BASE) {
  const view = render(
    <NewRunDraftProvider owner="alice"><Seed intent={intent}><Live access={access} /><Draft /></Seed></NewRunDraftProvider>,
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
    expect(draft().overrides).toBeUndefined();

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
});
