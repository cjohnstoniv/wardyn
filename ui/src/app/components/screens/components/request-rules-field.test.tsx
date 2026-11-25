/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The request-rules field in each state the approved frames draw: empty, several rules, adding,
// a refused rule, the limit, no host to put a rule on, and the remove-only row while the switch is off.
import * as React from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HttpError } from "../../../lib/api/core";
import { aheadByHours } from "../../../lib/test-clock";
import type { Component, HostRule } from "../../../lib/types";
import { COMPONENTS_ADMIN as A } from "../../wardyn/copy/components-admin";
import { setField } from "../../../../test/set-field";
import { ComponentEditor } from "./component-editor";
import { REQUEST_RULES as T, RequestRulesField } from "./request-rules-field";

const h = vi.hoisted(() => ({ on: false as boolean | null, put: vi.fn() }));
vi.mock("../../../lib/api/components", () => ({ components: { put: (...a: unknown[]) => h.put(...a) } }));
vi.mock("../../../lib/api/model-providers", () => ({ modelProviders: { getModelProviders: vi.fn(async () => ({ providers: { providers: [] }, connected: {}, etag: null })) } }));
vi.mock("../../wardyn/model-access-context", () => ({
  useShellSetupStatus: () => ({ status: h.on === null ? null : { features: h.on ? { scheduled_runs: false, component_request_rules: true } : undefined }, refresh: () => {} }),
}));

beforeEach(() => {
  h.on = false;
  h.put.mockReset();
});
afterEach(cleanup);

const HOSTS = ["api.example.com", "uploads.example.com"];
const THREE: HostRule[] = [
  { host: "api.example.com", methods: ["GET", "HEAD"], paths: ["/v1/invoices"], effect: "allow" },
  { host: "api.example.com", methods: ["POST"], paths: ["/v1/invoices"], effect: "hold" },
  { host: "uploads.example.com", effect: "deny" },
];

let latest: HostRule[] = [];

function Harness({ initial = [], ...props }: { initial?: HostRule[] } & Partial<React.ComponentProps<typeof RequestRulesField>>) {
  const [rules, setRules] = React.useState(initial);
  return (
    <RequestRulesField
      on
      hadRules={initial.length > 0}
      hosts={HOSTS}
      idPrefix="rr"
      {...props}
      rules={rules}
      onChange={(next) => {
        latest = next;
        setRules(next);
      }}
    />
  );
}

const user = userEvent.setup({ pointerEventsCheck: 0 });
const card = (n: number) => screen.getByRole("group", { name: T.RULE(n) });

describe("RequestRulesField: switch on", () => {
  it("With no rules it says so, offers Add a rule and counts 0 of 64", () => {
    render(<Harness />);
    expect(screen.getByRole("group", { name: T.LABEL })).toBeTruthy();
    expect(screen.getByText(T.HINT)).toBeTruthy();
    expect(screen.getByText(T.FORMAT_HINT)).toBeTruthy();
    expect(screen.getByText(T.EMPTY)).toBeTruthy();
    expect((screen.getByRole("button", { name: T.ADD }) as HTMLButtonElement).disabled).toBe(false);
    expect(screen.getByText("0 of 64 rules")).toBeTruthy();
    expect(screen.queryByTestId("request-rule")).toBeNull();
  });

  it("Shows each rule under a numbered heading, with its effect pressed and Hold explained on the Hold rule alone", () => {
    render(<Harness initial={THREE} />);
    expect(screen.getAllByTestId("request-rule")).toHaveLength(3);
    const one = within(card(1));
    expect(one.getByRole("combobox", { name: T.HOST_OF(1) }).textContent).toContain("api.example.com");
    expect((one.getByLabelText(T.METHODS_OF(1)) as HTMLInputElement).value).toBe("GET HEAD");
    expect((one.getByLabelText(T.PATHS_OF(1)) as HTMLInputElement).value).toBe("/v1/invoices");
    const effect = (n: number, label: string) => within(within(card(n)).getByRole("group", { name: T.EFFECT_OF(n) })).getByRole("button", { name: label });
    expect(effect(1, T.ALLOW).getAttribute("aria-pressed")).toBe("true");
    expect(effect(1, T.HOLD).getAttribute("aria-pressed")).toBe("false");
    expect(effect(2, T.HOLD).getAttribute("aria-pressed")).toBe("true");
    expect(effect(3, T.DENY).getAttribute("aria-pressed")).toBe("true");
    expect(screen.getAllByText(T.HOLD_HINT)).toHaveLength(1);
    expect(within(card(2)).getByText(T.HOLD_HINT)).toBeTruthy();
    expect(screen.getByText("3 of 64 rules")).toBeTruthy();
    expect(screen.queryByText(T.EMPTY)).toBeNull();
  });

  it("changing the effect shows the Hold line for a Hold rule and sends the new effect", async () => {
    render(<Harness initial={[THREE[0]]} />);
    expect(screen.queryByText(T.HOLD_HINT)).toBeNull();
    await user.click(within(within(card(1)).getByRole("group", { name: T.EFFECT_OF(1) })).getByRole("button", { name: T.HOLD }));
    expect(screen.getByText(T.HOLD_HINT)).toBeTruthy();
    expect(latest).toEqual([{ ...THREE[0], effect: "hold" }]);
  });

  it("uppercases methods as typed, keeps a typed space, and sends both lists split on spaces", () => {
    render(<Harness initial={[{ host: "api.example.com", effect: "allow" }]} />);
    const methods = screen.getByLabelText(T.METHODS_OF(1)) as HTMLInputElement;
    setField(methods, "get head ");
    expect(methods.value).toBe("GET HEAD ");
    setField(screen.getByLabelText(T.PATHS_OF(1)), "/v1  /v2/a ");
    expect(latest).toEqual([{ host: "api.example.com", methods: ["GET", "HEAD"], paths: ["/v1", "/v2/a"], effect: "allow" }]);
    setField(methods, "");
    setField(screen.getByLabelText(T.PATHS_OF(1)), "");
    expect(latest).toEqual([{ host: "api.example.com", effect: "allow" }]);
  });

  it("Add a rule appends a rule with Allow chosen and no host yet, and the menu lists only the hosts it was given", async () => {
    render(<Harness initial={[THREE[0]]} />);
    await user.click(screen.getByRole("button", { name: T.ADD }));
    expect(screen.getAllByTestId("request-rule")).toHaveLength(2);
    const trigger = within(card(2)).getByRole("combobox", { name: T.HOST_OF(2) });
    expect(trigger.textContent).toContain(T.HOST_PLACEHOLDER);
    expect(within(within(card(2)).getByRole("group", { name: T.EFFECT_OF(2) })).getByRole("button", { name: T.ALLOW }).getAttribute("aria-pressed")).toBe("true");
    await user.click(trigger);
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual(HOSTS);
    await user.click(screen.getByRole("option", { name: "uploads.example.com" }));
    expect(latest[1]).toEqual({ host: "uploads.example.com", effect: "allow" });
    expect(screen.getByText("2 of 64 rules")).toBeTruthy();
  });

  it("moves a rule up or down, with the first Up and the last Down disabled, and removes one", async () => {
    render(<Harness initial={THREE} />);
    const btn = (n: number, name: string) => within(card(n)).getByRole("button", { name }) as HTMLButtonElement;
    expect(btn(1, T.UP(1)).disabled).toBe(true);
    expect(btn(3, T.DOWN(3)).disabled).toBe(true);
    expect(btn(2, T.UP(2)).disabled).toBe(false);
    await user.click(btn(1, T.DOWN(1)));
    expect(latest.map((r) => r.effect)).toEqual(["hold", "allow", "deny"]);
    expect((within(card(1)).getByLabelText(T.METHODS_OF(1)) as HTMLInputElement).value).toBe("POST");
    await user.click(btn(3, T.UP(3)));
    expect(latest.map((r) => r.effect)).toEqual(["hold", "deny", "allow"]);
    await user.click(btn(2, T.REMOVE(2)));
    expect(latest.map((r) => r.effect)).toEqual(["hold", "allow"]);
    expect(screen.getByText("2 of 64 rules")).toBeTruthy();
  });

  it("Marks the rule a sentence names with an icon and text, and sets only its named input invalid, tied to the sentence", () => {
    const sentence = 'invalid component: definition.request_rules[1].paths[0]: path "/v1/../admin" is not in normal form';
    render(<Harness initial={THREE} refusal={sentence} failing={{ index: 1, part: "paths" }} />);
    expect(within(card(2)).getByText(T.REFUSED)).toBeTruthy();
    expect(screen.getAllByText(T.REFUSED)).toHaveLength(1);
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toBe(sentence);
    const paths = within(card(2)).getByLabelText(T.PATHS_OF(2));
    expect(paths.getAttribute("aria-invalid")).toBe("true");
    expect(paths.getAttribute("aria-describedby")).toBe(alert.id);
    expect(within(card(2)).getByLabelText(T.METHODS_OF(2)).getAttribute("aria-invalid")).toBeNull();
    expect(within(card(1)).getByLabelText(T.PATHS_OF(1)).getAttribute("aria-invalid")).toBeNull();
  });

  it("A refused host marks the Host menu, which still shows the host that was taken out of Hosts", () => {
    const rules: HostRule[] = [THREE[0], { host: "uploads.example.com", methods: ["PUT"], paths: ["/v1/files"], effect: "allow" }];
    render(<Harness initial={rules} hosts={["api.example.com"]} refusal="request_rules[1].host: not one of the hosts" failing={{ index: 1, part: "host" }} />);
    const host = within(card(2)).getByRole("combobox", { name: T.HOST_OF(2) });
    expect(host.textContent).toContain("uploads.example.com");
    expect(host.getAttribute("aria-invalid")).toBe("true");
    expect(host.getAttribute("aria-describedby")).toBe(screen.getByRole("alert").id);
  });

  it("an edit takes the refused mark off the rules, and the sentence stays until the next save", async () => {
    const sentence = "request_rules[1].paths[0]: bad";
    render(<Harness initial={THREE} refusal={sentence} failing={{ index: 1, part: "paths" }} />);
    expect(screen.getByText(T.REFUSED)).toBeTruthy();
    await user.click(screen.getByRole("button", { name: T.DOWN(2) }));
    expect(screen.queryByText(T.REFUSED)).toBeNull();
    expect(document.querySelectorAll("[aria-invalid]")).toHaveLength(0);
    expect(screen.getByRole("alert").textContent).toBe(sentence);
  });

  it("a sentence about the rules as a whole shows under the field and marks no rule", () => {
    render(<Harness initial={THREE} refusal="This component carries request rules, and request rules are off on this deployment." failing={{}} />);
    expect(screen.getByRole("alert").textContent).toContain("request rules are off");
    expect(screen.queryByText(T.REFUSED)).toBeNull();
    expect(document.querySelectorAll("[aria-invalid]")).toHaveLength(0);
  });

  it("At 64 rules Add a rule is disabled and says to remove one; one removed re-enables it", async () => {
    const many: HostRule[] = Array.from({ length: 64 }, (_, i) => ({ host: "api.example.com", paths: [`/p${i}`], effect: "allow" as const }));
    render(<Harness initial={many} />);
    expect((screen.getByRole("button", { name: T.ADD }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(T.AT_LIMIT)).toBeTruthy();
    expect(screen.getByText(T.RULE(64))).toBeTruthy();
    await user.click(screen.getByRole("button", { name: T.REMOVE(64) }));
    expect((screen.getByRole("button", { name: T.ADD }) as HTMLButtonElement).disabled).toBe(false);
    expect(screen.getByText("63 of 64 rules")).toBeTruthy();
  });

  it("With no host to put a rule on, Add a rule is disabled and says why, and no empty line is shown", () => {
    render(<Harness hosts={[]} />);
    expect((screen.getByRole("button", { name: T.ADD }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(T.NO_HOST)).toBeTruthy();
    expect(screen.queryByText(T.EMPTY)).toBeNull();
    expect(screen.queryByText(/of 64 rules/)).toBeNull();
  });

  it("is disabled as a whole while a save is in flight", () => {
    render(<Harness initial={THREE} disabled />);
    expect((screen.getByRole("group", { name: T.LABEL }) as HTMLFieldSetElement).disabled).toBe(true);
  });
});

describe("RequestRulesField: switch off", () => {
  it("A component with rules shows the count, the Off chip, the sentence and Remove rules, and no add or edit control", () => {
    render(<Harness on={false} initial={THREE} />);
    expect(screen.getByRole("group", { name: T.LABEL })).toBeTruthy();
    expect(screen.getByText("3 rules")).toBeTruthy();
    expect(screen.getByText(T.OFF_CHIP)).toBeTruthy();
    expect(screen.getByText(T.OFF_SENTENCE)).toBeTruthy();
    expect(screen.getByRole("button", { name: T.REMOVE_ALL })).toBeTruthy();
    expect(screen.queryByRole("button", { name: T.ADD })).toBeNull();
    expect(screen.queryByTestId("request-rule")).toBeNull();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(screen.getAllByRole("button")).toHaveLength(1);
  });

  it("says 1 rule for one", () => {
    render(<Harness on={false} initial={[THREE[0]]} />);
    expect(screen.getByText("1 rule")).toBeTruthy();
  });

  it("Remove rules empties the rules and says to save", async () => {
    render(<Harness on={false} initial={THREE} />);
    await user.click(screen.getByRole("button", { name: T.REMOVE_ALL }));
    expect(latest).toEqual([]);
    expect(screen.getByText(T.REMOVED)).toBeTruthy();
    expect(screen.queryByRole("button", { name: T.REMOVE_ALL })).toBeNull();
    expect(screen.queryByText(T.OFF_SENTENCE)).toBeNull();
  });

  it("With no rules, stored or drafted, the field is not shown at all", () => {
    const { container } = render(<Harness on={false} />);
    expect(container.textContent).toBe("");
  });
});

const STORED: Component = {
  id: "33333333-3333-4333-8333-333333333333",
  name: "Billing API",
  version: 4,
  created_at: aheadByHours(-24),
  updated_at: aheadByHours(-24),
  definition: { hosts: ["api.example.com", "uploads.example.com", "*.cdn.example.com", "other.example.com:8443"], request_rules: THREE },
};
const BARE: Component = { ...STORED, definition: { hosts: STORED.definition.hosts } };

function renderEditor(component: Component | null) {
  render(<ComponentEditor component={component} onClose={vi.fn()} onSaved={vi.fn()} />);
}
const save = () => user.click(screen.getByRole("button", { name: A.SAVE }));

describe("ComponentEditor: request rules", () => {
  it("The field sits directly after Hosts and before Secrets, and the Host menu offers the exact hosts only", async () => {
    h.on = true;
    renderEditor(BARE);
    const hosts = screen.getByLabelText(A.HOSTS);
    const rules = screen.getByRole("group", { name: T.LABEL });
    const secrets = screen.getByRole("group", { name: A.SECRETS_LABEL });
    expect(hosts.compareDocumentPosition(rules) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(rules.compareDocumentPosition(secrets) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByText(T.EMPTY)).toBeTruthy();
    await user.click(screen.getByRole("button", { name: T.ADD }));
    await user.click(screen.getByRole("combobox", { name: T.HOST_OF(1) }));
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual(["api.example.com", "uploads.example.com"]);
  });

  it("the field sits before the token audience of a remote tool server", async () => {
    h.on = true;
    renderEditor({ ...BARE, definition: { hosts: ["mcp.example.com"], mcp: { transport: "http", url: "https://mcp.example.com/mcp" } } });
    await act(async () => {}); // the audience field reads the providers once
    const rules = screen.getByRole("group", { name: T.LABEL });
    expect(rules.compareDocumentPosition(screen.getByLabelText(A.AUDIENCE)) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("an untouched save sends the stored rules back with the version it opened", async () => {
    h.on = true;
    h.put.mockResolvedValue({});
    renderEditor(STORED);
    await save();
    await waitFor(() => expect(h.put).toHaveBeenCalledTimes(1));
    expect(h.put.mock.calls[0][1]).toMatchObject({ expected_version: 4, definition: { request_rules: THREE } });
  });

  it("a new rule, an edit and a reorder are sent as the form stands", async () => {
    h.on = true;
    h.put.mockResolvedValue({});
    renderEditor(BARE);
    await user.click(screen.getByRole("button", { name: T.ADD }));
    await user.click(screen.getByRole("combobox", { name: T.HOST_OF(1) }));
    await user.click(await screen.findByRole("option", { name: "uploads.example.com" }));
    setField(screen.getByLabelText(T.METHODS_OF(1)), "put");
    setField(screen.getByLabelText(T.PATHS_OF(1)), "/v1/files");
    await user.click(screen.getByRole("button", { name: T.ADD }));
    await user.click(screen.getByRole("combobox", { name: T.HOST_OF(2) }));
    await user.click(await screen.findByRole("option", { name: "api.example.com" }));
    await user.click(within(within(card(2)).getByRole("group", { name: T.EFFECT_OF(2) })).getByRole("button", { name: T.DENY }));
    await user.click(screen.getByRole("button", { name: T.UP(2) }));
    await save();
    await waitFor(() => expect(h.put).toHaveBeenCalledTimes(1));
    expect(h.put.mock.calls[0][1].definition.request_rules).toEqual([
      { host: "api.example.com", effect: "deny" },
      { host: "uploads.example.com", methods: ["PUT"], paths: ["/v1/files"], effect: "allow" },
    ]);
  });

  it("removing every rule one by one sends none", async () => {
    h.on = true;
    h.put.mockResolvedValue({});
    renderEditor(STORED);
    for (let n = 3; n >= 1; n--) await user.click(screen.getByRole("button", { name: T.REMOVE(n) }));
    expect(screen.getByText(T.EMPTY)).toBeTruthy();
    await save();
    await waitFor(() => expect(h.put).toHaveBeenCalledTimes(1));
    expect(h.put.mock.calls[0][1].definition).not.toHaveProperty("request_rules");
  });

  it("A refusal naming a rule shows under the field, marks that rule, and leaves the alert line empty and everything typed", async () => {
    h.on = true;
    const sentence = 'invalid component: definition.request_rules[1].paths[0]: path "/v1/../admin" is not in normal form: it has an empty, "." or ".." segment';
    h.put.mockRejectedValue(new HttpError(422, sentence));
    renderEditor(STORED);
    setField(screen.getByLabelText(T.PATHS_OF(2)), "/v1/../admin");
    await save();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(sentence);
    expect(screen.getByRole("group", { name: T.LABEL }).contains(alert)).toBe(true);
    expect(within(card(2)).getByText(T.REFUSED)).toBeTruthy();
    const paths = screen.getByLabelText(T.PATHS_OF(2)) as HTMLInputElement;
    expect(paths.value).toBe("/v1/../admin");
    expect(paths.getAttribute("aria-invalid")).toBe("true");
    expect(paths.getAttribute("aria-describedby")).toBe(alert.id);
    expect(screen.getAllByRole("alert")).toHaveLength(1);
  });

  it("A rule left on a host taken out of Hosts is marked on its Host menu", async () => {
    h.on = true;
    h.put.mockRejectedValue(new HttpError(422, 'invalid component: definition.request_rules[2].host: "uploads.example.com" is not one of the component\'s hosts named exactly, with no port or port 443'));
    renderEditor(STORED);
    setField(screen.getByLabelText(A.HOSTS), "api.example.com");
    await save();
    await screen.findByRole("alert");
    expect(within(card(3)).getByText(T.REFUSED)).toBeTruthy();
    expect(screen.getByRole("combobox", { name: T.HOST_OF(3) }).getAttribute("aria-invalid")).toBe("true");
  });

  it("A version conflict stays in the existing alert line at the foot, outside the field", async () => {
    h.on = true;
    const stale = "Someone saved this component since you opened it. Open it again to see their change, then make your edit again.";
    h.put.mockRejectedValue(new HttpError(409, stale));
    renderEditor(STORED);
    await save();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(stale);
    expect(screen.getByRole("group", { name: T.LABEL }).contains(alert)).toBe(false);
    expect(screen.queryByText(T.REFUSED)).toBeNull();
  });

  it("The switch turned off after the page loaded refuses a changed rule under the field, keeping every edit", async () => {
    h.on = true;
    const off = "This component carries request rules, and request rules are off on this deployment. An admin turns them on with features.component_request_rules.enabled.";
    h.put.mockRejectedValue(new HttpError(422, off));
    renderEditor(STORED);
    await user.click(within(within(card(1)).getByRole("group", { name: T.EFFECT_OF(1) })).getByRole("button", { name: T.DENY }));
    await save();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(off);
    expect(screen.getByRole("group", { name: T.LABEL }).contains(alert)).toBe(true);
    expect(screen.queryByText(T.REFUSED)).toBeNull();
    expect(screen.getAllByTestId("request-rule")).toHaveLength(3);
  });

  it("Off, a component with rules shows the remove-only row, and Remove rules then Save sends none", async () => {
    h.put.mockResolvedValue({});
    renderEditor(STORED);
    expect(screen.getByText("3 rules")).toBeTruthy();
    expect(screen.getByText(T.OFF_SENTENCE)).toBeTruthy();
    expect(screen.queryByRole("button", { name: T.ADD })).toBeNull();
    await user.click(screen.getByRole("button", { name: T.REMOVE_ALL }));
    expect(screen.getByText(T.REMOVED)).toBeTruthy();
    await save();
    await waitFor(() => expect(h.put).toHaveBeenCalledTimes(1));
    expect(h.put.mock.calls[0][1].definition).not.toHaveProperty("request_rules");
    expect(h.put.mock.calls[0][1].definition.hosts).toEqual(STORED.definition.hosts);
  });

  it("off, a save with the rules untouched sends them back as stored", async () => {
    h.put.mockResolvedValue({});
    renderEditor(STORED);
    await save();
    await waitFor(() => expect(h.put).toHaveBeenCalledTimes(1));
    expect(h.put.mock.calls[0][1].definition.request_rules).toEqual(THREE);
  });

  it("off, a refusal about a rule that was stored shows under the remove-only row, since no rule input exists to carry it", async () => {
    const sentence = 'invalid component: definition.request_rules[0].host: "api.example.com" is not one of the component\'s hosts named exactly, with no port or port 443';
    h.put.mockRejectedValue(new HttpError(422, sentence));
    renderEditor(STORED);
    setField(screen.getByLabelText(A.HOSTS), "uploads.example.com");
    await save();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe(sentence);
    expect(screen.getByRole("group", { name: T.LABEL }).contains(alert)).toBe(true);
    expect(screen.getAllByRole("alert")).toHaveLength(1);
    expect(screen.getByText("3 rules")).toBeTruthy();
    expect(screen.getByRole("button", { name: T.REMOVE_ALL })).toBeTruthy();
  });

  it("until the setup status has answered the field is not shown, and a save sends the stored rules unchanged", async () => {
    h.on = null;
    h.put.mockResolvedValue({});
    renderEditor(STORED);
    expect(screen.queryByRole("group", { name: T.LABEL })).toBeNull();
    expect(screen.queryByText(T.OFF_SENTENCE)).toBeNull();
    await save();
    await waitFor(() => expect(h.put).toHaveBeenCalledTimes(1));
    expect(h.put.mock.calls[0][1].definition.request_rules).toEqual(THREE);
  });

  it("off, a component with no rules shows no rules field and the shipped editor", () => {
    renderEditor(BARE);
    expect(screen.queryByRole("group", { name: T.LABEL })).toBeNull();
    expect(screen.getByRole("group", { name: A.CONFIG_LABEL })).toBeTruthy();
    expect(screen.getByRole("group", { name: A.SECRETS_LABEL })).toBeTruthy();
  });
});
