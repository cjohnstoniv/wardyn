/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { ComponentSaved, HostRule } from "../../../../lib/types";
import { REQUEST_RULES as R } from "../../components/request-rules-field";
import { setField } from "../../../../../test/set-field";
import { CustomComponentDialog, type CustomComponentDialogProps } from "./custom-component-dialog";
import { aheadByHours } from "../../../../lib/test-clock";

const rulesOn = vi.hoisted(() => ({ on: false }));
vi.mock("../../../wardyn/model-access-context", () => ({
  useShellSetupStatus: () => ({ status: { features: rulesOn.on ? { scheduled_runs: false, component_request_rules: true } : undefined }, refresh: () => {} }),
}));

// The real components client over a stubbed fetch: the routes, verbs and bodies
// below are what goes on the wire.
const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

let fetchMock: ReturnType<typeof vi.fn>;
beforeEach(() => {
  rulesOn.on = false;
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => vi.unstubAllGlobals());

const calls = () =>
  fetchMock.mock.calls.map(([path, init]) => ({
    path: String(path),
    method: String(init?.method),
    body: init?.body ? JSON.parse(String(init.body)) : undefined,
  }));

const SAVED: ComponentSaved = {
  id: "11111111-1111-1111-1111-111111111111",
  owner: "me",
  name: "Acme",
  definition: { hosts: ["api.acme.test"] },
  version: 1,
  created_at: aheadByHours(-24),
  updated_at: aheadByHours(-24),
  requirements: [],
};

function open(over: Partial<CustomComponentDialogProps> = {}) {
  const props: CustomComponentDialogProps = {
    context: "run",
    residentAllowed: true,
    secretsPath: "/secrets",
    onClose: vi.fn(),
    onAttach: vi.fn(),
    onSaved: vi.fn(),
    ...over,
  };
  const user = userEvent.setup({ pointerEventsCheck: 0 });
  render(
    <MemoryRouter>
      <CustomComponentDialog {...props} />
    </MemoryRouter>,
  );
  return { props, user };
}

const type = async (_user: ReturnType<typeof userEvent.setup>, label: RegExp | string, text: string) => {
  setField(screen.getByLabelText(label), text);
};

const setFieldValue = (label: string, text: string) => setField(screen.getByLabelText(label), text);

describe("CustomComponentDialog: this run only", () => {
  it("adds an inline definition to the run and sends nothing to the server", async () => {
    const { props, user } = open();
    await type(user, /^Hosts it reaches/, "api.acme.test");
    await user.click(screen.getByRole("button", { name: "Add a secret" }));
    await type(user, /^Stored secret name/, "acme-key");
    await user.click(screen.getByRole("button", { name: "Add to this run" }));

    expect(props.onAttach).toHaveBeenCalledWith({
      inline: {
        hosts: ["api.acme.test"],
        secrets: [{ secret_name: "acme-key", delivery: { mode: "header", host: "api.acme.test" } }],
      },
    });
    expect(props.onClose).toHaveBeenCalled();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("labels the inline component when it has a name", async () => {
    const { props, user } = open();
    await type(user, /^Name/, "Acme API");
    await type(user, /^Hosts it reaches/, "api.acme.test");
    await user.click(screen.getByRole("button", { name: "Add to this run" }));
    expect(props.onAttach).toHaveBeenCalledWith({ inline: { hosts: ["api.acme.test"] }, name: "Acme API" });
  });

  it("offers no plain-HTTP control, and no shared choice, for a header secret (D6)", async () => {
    const { user } = open();
    await user.click(screen.getByRole("button", { name: "Add a secret" }));
    expect(screen.queryByText(/plain http|without tls|unencrypted/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
    expect(screen.queryByText(/shared|provided by your admin/i)).not.toBeInTheDocument();
  });

  it("a refused add names the field, focuses it and drops nothing the person typed", async () => {
    const { props, user } = open();
    await type(user, /^Name/, "Acme API");
    await type(user, /^Hosts it reaches/, "10.0.0.5\napi.acme.test");
    await user.click(screen.getByRole("button", { name: "Add a secret" }));
    await type(user, /^Stored secret name/, "acme-key");
    await user.click(screen.getByRole("button", { name: "Environment variable" }));
    await type(user, /^Variable name/, "PATH");
    await user.click(screen.getByRole("button", { name: "Add to this run" }));

    // Both fields are named, each beside its own control.
    const hosts = screen.getByLabelText(/^Hosts it reaches/);
    expect(hosts).toHaveAttribute("aria-invalid", "true");
    expect(document.getElementById(hosts.getAttribute("aria-describedby")!.split(" ").find((i) => i.endsWith("-error"))!)).toHaveTextContent(
      /"10\.0\.0\.5" must be a DNS name, not an IP address/,
    );
    const variable = screen.getByLabelText(/^Variable name/);
    expect(variable).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText(/"PATH" is reserved/)).toBeInTheDocument();

    // Nothing was sent or closed, and every typed value is still there.
    expect(props.onAttach).not.toHaveBeenCalled();
    expect(props.onClose).not.toHaveBeenCalled();
    expect(screen.getByLabelText(/^Name/)).toHaveValue("Acme API");
    expect(hosts).toHaveValue("10.0.0.5\napi.acme.test");
    expect(screen.getByLabelText(/^Stored secret name/)).toHaveValue("acme-key");
    expect(variable).toHaveValue("PATH");
    await waitFor(() => expect(hosts).toHaveFocus());
  });
});

describe("CustomComponentDialog: env and file delivery switch", () => {
  it("hides both when the deployment refuses them", async () => {
    const { user } = open({ residentAllowed: false });
    await user.click(screen.getByRole("button", { name: "Add a secret" }));
    expect(screen.getByRole("button", { name: "Request header" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Environment variable" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "File" })).not.toBeInTheDocument();
  });

  it("offers all three when it allows them", async () => {
    const { user } = open();
    await user.click(screen.getByRole("button", { name: "Add a secret" }));
    for (const n of ["Request header", "Environment variable", "File"]) {
      expect(screen.getByRole("button", { name: n })).toBeInTheDocument();
    }
  });
});

describe("CustomComponentDialog: save to reuse", () => {
  const fill = async (user: ReturnType<typeof userEvent.setup>) => {
    await user.click(screen.getByRole("button", { name: "Save to reuse" }));
    await type(user, /^Name/, "Acme");
    await type(user, /^Hosts it reaches/, "api.acme.test");
    await user.click(screen.getByRole("button", { name: "Add a secret" }));
    await type(user, /^Stored secret name/, "acme-key");
  };

  it("POSTs the definition, attaches the saved row by id and closes when nothing is missing", async () => {
    fetchMock.mockResolvedValue(json(201, { ...SAVED, requirements: [{ kind: "secret", name: "acme-key", status: "present" }] }));
    const { props, user } = open();
    await fill(user);
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(props.onClose).toHaveBeenCalled());
    expect(calls()).toEqual([
      {
        path: "/api/v1/me/components",
        method: "POST",
        body: {
          name: "Acme",
          definition: {
            hosts: ["api.acme.test"],
            secrets: [{ secret_name: "acme-key", delivery: { mode: "header", host: "api.acme.test" } }],
          },
        },
      },
    ]);
    expect(props.onAttach).toHaveBeenCalledWith({ id: SAVED.id });
    expect(props.onSaved).toHaveBeenCalledWith(expect.objectContaining({ id: SAVED.id }));
  });

  it("its fields are read-only while the save is in flight, so nothing typed then is lost when it closes", async () => {
    let answer: ((r: Response) => void) | undefined;
    fetchMock.mockReturnValue(new Promise<Response>((done) => (answer = done)));
    const { props, user } = open();
    await fill(user);
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByLabelText(/^Name/)).toBeDisabled();
    expect(screen.getByLabelText(/^Hosts it reaches/)).toBeDisabled();
    expect(screen.getByLabelText(/^Stored secret name/)).toBeDisabled();
    expect(screen.getByRole("button", { name: "Save to reuse" })).toBeDisabled();
    await act(async () => answer?.(json(201, SAVED)));
    await waitFor(() => expect(props.onClose).toHaveBeenCalled());
  });

  it("renders the answer's missing secrets with a link to add them (D18)", async () => {
    fetchMock.mockResolvedValue(
      json(201, {
        ...SAVED,
        requirements: [
          { kind: "secret", name: "acme-key", status: "missing", fix: "add_secret" },
          { kind: "secret", name: "other-key", status: "present" },
        ],
      }),
    );
    const { props, user } = open({ secretsPath: "/admin/secrets" });
    await fill(user);
    await user.click(screen.getByRole("button", { name: "Save" }));

    const needs = await screen.findByTestId("custom-component-needs");
    expect(screen.getByText("Needs input before a run can launch")).toBeInTheDocument();
    expect(within(needs).getByText("Secret acme-key isn't stored yet.")).toBeInTheDocument();
    expect(within(needs).queryByText(/other-key/)).not.toBeInTheDocument();
    expect(within(needs).getByRole("link", { name: "Add it on the Secrets page" })).toHaveAttribute("href", "/admin/secrets");
    // It stays open until the person is done, and the row is already on the run.
    expect(props.onClose).not.toHaveBeenCalled();
    expect(props.onAttach).toHaveBeenCalledWith({ id: SAVED.id });
    await user.click(screen.getByRole("button", { name: "Done" }));
    expect(props.onClose).toHaveBeenCalled();
  });

  it("a server refusal names the field it is about and keeps the whole form", async () => {
    fetchMock.mockResolvedValue(
      json(422, {
        error: "invalid component: definition.secrets[0].secret_name: \"acme-key\" is managed by Wardyn",
        reason: "component_definition_invalid",
      }),
    );
    const { props, user } = open();
    await fill(user);
    await user.click(screen.getByRole("button", { name: "Save" }));

    const field = screen.getByLabelText(/^Stored secret name/);
    await waitFor(() => expect(field).toHaveAttribute("aria-invalid", "true"));
    expect(screen.getByText(/"acme-key" is managed by Wardyn/)).toBeInTheDocument();
    expect(field).toHaveValue("acme-key");
    expect(screen.getByLabelText(/^Name/)).toHaveValue("Acme");
    expect(screen.getByLabelText(/^Hosts it reaches/)).toHaveValue("api.acme.test");
    expect(props.onClose).not.toHaveBeenCalled();
    expect(props.onAttach).not.toHaveBeenCalled();
  });

  it("a name already in use belongs to the name field", async () => {
    fetchMock.mockResolvedValue(json(409, { error: "you already have a component with that name", reason: "component_name_conflict" }));
    const { user } = open();
    await fill(user);
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.getByLabelText(/^Name/)).toHaveAttribute("aria-invalid", "true"));
    expect(screen.getByText("you already have a component with that name")).toBeInTheDocument();
  });

  it("a saved component must be named before anything is sent", async () => {
    const { user } = open();
    await user.click(screen.getByRole("button", { name: "Save to reuse" }));
    await type(user, /^Hosts it reaches/, "api.acme.test");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByLabelText(/^Name/)).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText("Give it a name.")).toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe("CustomComponentDialog: editing a saved component", () => {
  it("PUTs to its own id and does not attach it to a run", async () => {
    fetchMock.mockResolvedValue(json(200, { ...SAVED, name: "Acme v2", version: 2 }));
    const { props, user } = open({ context: "saved", component: SAVED, onAttach: undefined });
    expect(screen.getByLabelText(/^Name/)).toHaveValue("Acme");
    expect(screen.getByLabelText(/^Hosts it reaches/)).toHaveValue("api.acme.test");
    await type(user, /^Name/, "Acme v2");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(props.onClose).toHaveBeenCalled());
    expect(calls()).toEqual([
      { path: `/api/v1/me/components/${SAVED.id}`, method: "PUT", body: { name: "Acme v2", definition: { hosts: ["api.acme.test"] }, expected_version: 1 } },
    ]);
  });

  it("keeps a saved MCP server's address and transport when it is renamed", async () => {
    const mcp = { ...SAVED, definition: { hosts: ["mcp.acme.test"], mcp: { url: "https://mcp.acme.test/mcp", transport: "http" as const } } };
    fetchMock.mockResolvedValue(json(200, { ...mcp, name: "Acme v2", version: 2 }));
    const { props, user } = open({ context: "saved", component: mcp, onAttach: undefined });
    await type(user, /^Name/, "Acme v2");
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(props.onClose).toHaveBeenCalled());
    expect(calls()[0].body).toEqual({ name: "Acme v2", definition: { hosts: ["mcp.acme.test"], mcp: { url: "https://mcp.acme.test/mcp", transport: "http" } }, expected_version: 1 });
  });

  it("keeps the form open, as typed, when the component was saved elsewhere since it opened", async () => {
    const msg = "Someone saved this component since you opened it. Open it again to see their change, then make your edit again.";
    fetchMock.mockResolvedValue(json(409, { error: msg, reason: "component_version_conflict" }));
    const { props, user } = open({ context: "saved", component: SAVED, onAttach: undefined });
    await type(user, /^Name/, "Acme v2");
    await user.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByText(msg)).toBeInTheDocument();
    expect(screen.getByLabelText(/^Name/)).toHaveValue("Acme v2");
    expect(props.onClose).not.toHaveBeenCalled();
    expect(props.onSaved).not.toHaveBeenCalled();
  });

  it("has no this-run-only choice: it always saves", () => {
    open({ context: "saved", component: SAVED });
    expect(screen.queryByRole("button", { name: "For this run only" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();
  });
});

// The member case of the request rules (R9 to R12): the same field as the organisation's editor, in the person's dialog.
const RULES: HostRule[] = [{ host: "api.acme.test", methods: ["GET", "HEAD"], paths: ["/v1/invoices"], effect: "allow" }];
const WITH_RULES: ComponentSaved = { ...SAVED, definition: { hosts: ["api.acme.test", "cdn.acme.test"], request_rules: RULES } };
const ruleGroup = () => screen.getByRole("group", { name: R.LABEL });
const refuse = (status: number, error: string, reason: string) => json(status, { error, reason });

describe("CustomComponentDialog: request rules", () => {
  it("R9 left: a new component for this run has the field after the hosts and before the secrets, and the rule goes inline", async () => {
    rulesOn.on = true;
    const { props, user } = open();
    const hosts = screen.getByLabelText(/^Hosts it reaches/);
    expect(hosts.compareDocumentPosition(ruleGroup()) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(ruleGroup().compareDocumentPosition(screen.getByRole("group", { name: "Secrets" })) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByRole("button", { name: "For this run only" })).toBeInTheDocument();
    expect(screen.getByText(R.NO_HOST)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: R.ADD })).toBeDisabled();

    await type(user, /^Hosts it reaches/, "api.acme.test\n*.cdn.acme.test");
    expect(screen.getByText(R.EMPTY)).toBeInTheDocument();
    expect(screen.getByText("0 of 64 rules")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: R.ADD }));
    await user.click(screen.getByRole("combobox", { name: R.HOST_OF(1) }));
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual(["api.acme.test"]);
    await user.click(screen.getByRole("option", { name: "api.acme.test" }));
    setFieldValue(R.METHODS_OF(1), "post");
    await user.click(screen.getByRole("button", { name: "Add to this run" }));
    expect(props.onAttach).toHaveBeenCalledWith({ inline: { hosts: ["api.acme.test", "*.cdn.acme.test"], request_rules: [{ host: "api.acme.test", methods: ["POST"], effect: "allow" }] } });
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("R9 right: a saved component's refusal names the rule, shows the server's sentence under the field, focuses the input and keeps everything typed", async () => {
    rulesOn.on = true;
    fetchMock.mockResolvedValue(refuse(422, 'invalid component: definition.request_rules[0].paths[0]: "v1/invoices" must start with /', "component_definition_invalid"));
    const { props, user } = open({ context: "saved", component: WITH_RULES, onAttach: undefined });
    setFieldValue(R.PATHS_OF(1), "v1/invoices");
    await type(user, /^Name/, "Acme v2");
    await user.click(screen.getByRole("button", { name: "Save" }));

    const sentence = await screen.findByText('request_rules[0].paths[0]: "v1/invoices" must start with /');
    expect(ruleGroup().contains(sentence)).toBe(true);
    expect(within(screen.getByRole("group", { name: R.RULE(1) })).getByText(R.REFUSED)).toBeInTheDocument();
    const paths = screen.getByLabelText(R.PATHS_OF(1));
    expect(paths).toHaveAttribute("aria-invalid", "true");
    expect(paths.getAttribute("aria-describedby")).toBe(sentence.id);
    await waitFor(() => expect(paths).toHaveFocus());
    expect(paths).toHaveValue("v1/invoices");
    expect(screen.getByLabelText(/^Name/)).toHaveValue("Acme v2");
    expect(props.onClose).not.toHaveBeenCalled();
  });

  it("R11: the off and unread sentences show under the field, nothing typed is lost, and the summary still says the save was refused", async () => {
    rulesOn.on = true;
    const off = "This component carries request rules, and request rules are off on this deployment. An admin turns them on with features.component_request_rules.enabled.";
    const unread = "This component carries request rules, and whether request rules are on could not be read. Try again in a moment.";
    for (const sentence of [off, unread]) {
      fetchMock.mockResolvedValue(refuse(422, sentence, "component_request_rules_off"));
      const { user } = open({ context: "saved", component: WITH_RULES, onAttach: undefined });
      await user.click(within(within(screen.getByRole("group", { name: R.RULE(1) })).getByRole("group", { name: R.EFFECT_OF(1) })).getByRole("button", { name: R.DENY }));
      await user.click(screen.getByRole("button", { name: "Save" }));
      const shown = await screen.findByText(sentence);
      expect(ruleGroup().contains(shown)).toBe(true);
      expect(screen.getAllByText(sentence)).toHaveLength(1);
      expect(screen.getByText(/can't be saved as written/i)).toBeInTheDocument();
      expect(screen.queryByText(R.REFUSED)).not.toBeInTheDocument();
      expect(screen.getAllByTestId("request-rule")).toHaveLength(1);
      cleanup();
    }
  });

  it("R12: a version conflict stays in the summary, not under the field", async () => {
    rulesOn.on = true;
    const msg = "Someone saved this component since you opened it. Open it again to see their change, then make your edit again.";
    fetchMock.mockResolvedValue(refuse(409, msg, "component_version_conflict"));
    open({ context: "saved", component: WITH_RULES, onAttach: undefined });
    await screen.getByRole("button", { name: "Save" }).click();
    const shown = await screen.findByText(msg);
    expect(ruleGroup().contains(shown)).toBe(false);
    expect(screen.queryByText(R.REFUSED)).not.toBeInTheDocument();
  });

  it("R10 left: off, a saved component with rules shows the remove-only row, and Remove rules then Save sends no rules", async () => {
    fetchMock.mockResolvedValue(json(200, { ...WITH_RULES, version: 2 }));
    const { props, user } = open({ context: "saved", component: WITH_RULES, onAttach: undefined });
    expect(screen.getByText("1 rule")).toBeInTheDocument();
    expect(screen.getByText(R.OFF_CHIP)).toBeInTheDocument();
    expect(screen.getByText(R.OFF_SENTENCE)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: R.ADD })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: R.REMOVE_ALL }));
    expect(screen.getByText(R.REMOVED)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(props.onClose).toHaveBeenCalled());
    expect(calls()).toEqual([
      { path: `/api/v1/me/components/${SAVED.id}`, method: "PUT", body: { name: "Acme", definition: { hosts: ["api.acme.test", "cdn.acme.test"] }, expected_version: 1 } },
    ]);
  });

  it("off, a save with the rules untouched sends them back as stored", async () => {
    fetchMock.mockResolvedValue(json(200, { ...WITH_RULES, version: 2 }));
    const { props, user } = open({ context: "saved", component: WITH_RULES, onAttach: undefined });
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(props.onClose).toHaveBeenCalled());
    expect(calls()[0].body.definition.request_rules).toEqual(RULES);
  });

  it("R10 right: off, a component with no rules shows no field, and a new one for this run has none", () => {
    open({ context: "saved", component: SAVED, onAttach: undefined });
    expect(screen.queryByRole("group", { name: R.LABEL })).not.toBeInTheDocument();
    cleanup();
    open();
    expect(screen.queryByRole("group", { name: R.LABEL })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add a setting" })).toBeInTheDocument();
  });
});
