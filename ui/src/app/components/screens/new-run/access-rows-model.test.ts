/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import type { ComponentFact } from "../../../lib/types/components";
import { ACCESS_ROWS as T } from "../../wardyn/copy/components";
import { accessIssues, accessRow, accessRows, overlayFacts, rowDomId, rowIdFromDomId } from "./access-rows-model";

const custom = (over: Partial<ComponentFact> = {}): ComponentFact => ({
  kind: "custom", id: "c1", name: "Billing API", reason: "self", status: "ready", requirements: [], ...over,
});
const git = (over: Partial<ComponentFact> = {}): ComponentFact => ({
  kind: "git_provider", provider: "github", id: "git_provider:github:app", reason: "workspace", status: "unknown", requirements: [], lane: "app", ...over,
});
const missing = { id: "secret:tok", kind: "secret", label: "Secret: tok", required_by: "an env_secret grant (TOK)", status: "missing", fix: { action: "add_secret", secret_name: "tok" } } as const;

describe("overlayFacts", () => {
  it("is empty with neither read", () => {
    expect(overlayFacts(undefined, undefined)).toEqual([]);
  });
  it("is the preview's facts when preflight has not answered", () => {
    const a = custom(), b = custom({ id: "c2" });
    expect(overlayFacts([a, b], undefined)).toEqual([a, b]);
  });
  it("is preflight's facts when the preview has not answered", () => {
    const a = custom();
    expect(overlayFacts(undefined, [a])).toEqual([a]);
  });
  it("replaces a preview fact with preflight's answer for the same id, in the preview's order", () => {
    const pa = custom({ id: "a" }), pb = custom({ id: "b", status: "unknown" });
    const fb = custom({ id: "b", status: "needs_input" });
    expect(overlayFacts([pa, pb], [fb]).map((f) => [f.id, f.status])).toEqual([["a", "ready"], ["b", "needs_input"]]);
  });
  it("appends a fact only preflight carries, after the preview's", () => {
    expect(overlayFacts([custom({ id: "a" })], [custom({ id: "z" })]).map((f) => f.id)).toEqual(["a", "z"]);
  });
  it("never lists one id twice", () => {
    expect(overlayFacts([custom()], [custom()]).map((f) => f.id)).toEqual(["c1"]);
  });
  it("leaves a preview fact preflight dropped in place", () => {
    expect(overlayFacts([custom({ id: "a" }), custom({ id: "b" })], [custom({ id: "b" })]).map((f) => f.id)).toEqual(["a", "b"]);
  });
});

describe("accessRow — what names it and why it is there", () => {
  it("names a GitHub row GitHub and an Azure DevOps row Azure DevOps", () => {
    expect(accessRow(git()).title).toBe(T.TITLE.github);
    expect(accessRow(git({ provider: "azure_devops" })).title).toBe(T.TITLE.azure_devops);
  });
  it("names a custom row by its name", () => {
    expect(accessRow(custom({ name: "  Billing API " })).title).toBe("Billing API");
  });
  it("gives an unnamed custom row a plain name", () => {
    expect(accessRow(custom({ name: undefined })).title).toBe(T.TITLE.custom_unnamed);
    expect(accessRow(custom({ name: "   " })).title).toBe(T.TITLE.custom_unnamed);
  });
  it.each(["org", "self", "inline", "workspace"] as const)("says why a %s row is there", (reason) => {
    expect(accessRow(custom({ reason })).reason).toBe(T.REASON[reason]);
  });
});

describe("accessRow — status, tone and the D14 block", () => {
  it.each([
    ["ready", "success", false],
    ["unknown", "neutral", false],
    ["unavailable", "warning", false],
    ["needs_input", "danger", true],
  ] as const)("a custom %s row is %s tone, blocking=%s", (status, tone, blocking) => {
    const row = accessRow(custom({ status }));
    expect(row.tone).toBe(tone);
    expect(row.blocking).toBe(blocking);
    expect(row.statusLabel).toBe(T.STATUS[status]);
  });
  it("a custom refused row blocks, in error tone", () => {
    const row = accessRow(custom({ status: "refused" as ComponentFact["status"] }));
    expect(row).toMatchObject({ blocking: true, tone: "danger", statusLabel: T.STATUS.refused });
    expect(row.issueText).toBe(T.ISSUE_REFUSED("Billing API"));
  });
  it("a custom needs_input row's issue sentence names it", () => {
    expect(accessRow(custom({ status: "needs_input" })).issueText).toBe(T.ISSUE_NEEDS_INPUT("Billing API"));
  });
  it("a Git provider that needs input is a warning, not a block", () => {
    const row = accessRow(git({ status: "needs_input" }));
    expect(row).toMatchObject({ blocking: false, tone: "warning", issueText: null });
  });
  it("a Git provider that is refused still does not block (only custom rows do)", () => {
    expect(accessRow(git({ status: "refused" as ComponentFact["status"] })).blocking).toBe(false);
  });
  it("a ready or unknown row carries no issue sentence", () => {
    expect(accessRow(custom()).issueText).toBeNull();
    expect(accessRow(custom({ status: "unknown" })).issueText).toBeNull();
  });
  it("an unavailable row says what that means for its kind", () => {
    expect(accessRow(custom({ status: "unavailable" })).statusNote).toBe(T.UNAVAILABLE.custom);
    expect(accessRow(git({ status: "unavailable" })).statusNote).toBe(T.UNAVAILABLE.git_provider);
    expect(accessRow(custom()).statusNote).toBeNull();
  });
  it("a status the client does not know reads as not checked", () => {
    expect(accessRow(custom({ status: "later" as ComponentFact["status"] })).statusLabel).toBe(T.STATUS.unknown);
  });
});

describe("accessRow — disclosures", () => {
  it("has none for a plain row", () => {
    expect(accessRow(custom()).disclosures).toEqual([]);
  });
  it("says a self-defined run reaches destinations the person added", () => {
    expect(accessRow(custom({ self_defined: true })).disclosures).toEqual([T.DISCLOSURE.SELF_DEFINED]);
  });
  it("says Wardyn decrypts the connection only when the server says it does", () => {
    expect(accessRow(custom({ tls_intercept: true })).disclosures).toEqual([T.DISCLOSURE.HEADER]);
    expect(accessRow(custom({ secrets: [{ delivery: "header", shared: false }] })).disclosures).toEqual([]);
  });
  it.each(["env", "file"] as const)("says a %s secret is inside the sandbox for the whole run", (delivery) => {
    expect(accessRow(custom({ secrets: [{ delivery, shared: false }] })).disclosures).toEqual([T.DISCLOSURE.RESIDENT]);
  });
  it("says it once for several resident secrets", () => {
    const secrets = [{ delivery: "env" as const, shared: false }, { delivery: "file" as const, shared: false }];
    expect(accessRow(custom({ secrets })).disclosures).toEqual([T.DISCLOSURE.RESIDENT]);
  });
  it("prints a cap sentence only when the server set a cap", () => {
    expect(accessRow(custom({ self_defined: true })).disclosures).not.toContain(T.DISCLOSURE.CAP_L1);
    expect(accessRow(custom({ autonomy_cap: "L1" })).disclosures).toEqual([T.DISCLOSURE.CAP_L1]);
    expect(accessRow(custom({ autonomy_cap: "L0" })).disclosures).toEqual([T.DISCLOSURE.CAP_L0]);
  });
  it("prints neither cap sentence for any other level", () => {
    expect(accessRow(custom({ autonomy_cap: "L2" as ComponentFact["autonomy_cap"] })).disclosures).toEqual([]);
  });
  it("says high risk and the strongest sandbox when the server does", () => {
    expect(accessRow(custom({ high_risk: true, vault_floor: true })).disclosures).toEqual([T.DISCLOSURE.HIGH_RISK, T.DISCLOSURE.VAULT_FLOOR]);
  });
  it("lists them in the design's order", () => {
    const row = accessRow(custom({
      self_defined: true, tls_intercept: true, secrets: [{ delivery: "env", shared: false }],
      autonomy_cap: "L1", high_risk: true, vault_floor: true,
    }));
    expect(row.disclosures).toEqual([
      T.DISCLOSURE.SELF_DEFINED, T.DISCLOSURE.HEADER, T.DISCLOSURE.RESIDENT, T.DISCLOSURE.CAP_L1, T.DISCLOSURE.HIGH_RISK, T.DISCLOSURE.VAULT_FLOOR,
    ]);
  });
});

describe("accessRow — what the open row shows", () => {
  it("carries requirements, hosts, secrets, settings, organisation and repositories as sent", () => {
    const row = accessRow(custom({
      requirements: [missing], hosts: ["api.example"], secrets: [{ delivery: "header", shared: true }],
      config_keys: ["REGION"], org: "https://dev.azure.com/contoso", repos: ["https://dev.azure.com/contoso/p/_git/r"],
    }));
    expect(row).toMatchObject({
      requirements: [missing], hosts: ["api.example"], secrets: [{ delivery: "header", shared: true }],
      configKeys: ["REGION"], org: "https://dev.azure.com/contoso", repos: ["https://dev.azure.com/contoso/p/_git/r"],
    });
  });
  it("defaults every list to empty when the fact omits it", () => {
    const row = accessRow({ kind: "custom", id: "x", reason: "inline", status: "ready" } as ComponentFact);
    expect(row).toMatchObject({ requirements: [], hosts: [], secrets: [], configKeys: [], repos: [] });
  });
  it.each(["app", "pat", "ssh", "entra", "direct", "none"] as const)("says what the %s lane means", (lane) => {
    expect(accessRow(git({ lane })).laneNote).toBe(T.LANE[lane]);
  });
  it("says nothing of a lane the fact does not name", () => {
    expect(accessRow(git({ lane: undefined })).laneNote).toBeNull();
  });
  it("never puts a secret's name on a row", () => {
    const row = accessRow(custom({ secrets: [{ delivery: "header", shared: true }] }));
    expect(JSON.stringify(row.secrets)).not.toMatch(/secret_name|name/);
  });
});

describe("row ids and the issues that hold Launch", () => {
  it("round-trips a fact id through its DOM id, whatever characters it has", () => {
    for (const id of ["c1", "inline:0", "git_provider:azure_devops:entra:https://dev.azure.com/contoso"]) {
      expect(rowIdFromDomId(rowDomId(id))).toBe(id);
    }
  });
  it("reads no row id from any other DOM id", () => {
    expect(rowIdFromDomId("nr-title")).toBeUndefined();
    expect(rowIdFromDomId("")).toBeUndefined();
  });
  it("lists a blocking row as an inline Access issue that focuses the row", () => {
    const rows = accessRows([custom({ status: "needs_input", id: "a" })], undefined);
    expect(accessIssues(rows)).toEqual([
      { panel: "access", focus: rowDomId("a"), text: T.ISSUE_NEEDS_INPUT("Billing API"), inline: true },
    ]);
  });
  it("lists no issue for rows that do not block", () => {
    const rows = accessRows([custom(), git({ status: "needs_input" }), custom({ id: "u", status: "unavailable" })], undefined);
    expect(accessIssues(rows)).toEqual([]);
  });
  it("lists blocking rows in row order", () => {
    const rows = accessRows([custom({ id: "a", name: "A", status: "needs_input" }), custom({ id: "b", name: "B" }), custom({ id: "c", name: "C", status: "needs_input" })], undefined);
    expect(accessIssues(rows).map((i) => i.text)).toEqual([T.ISSUE_NEEDS_INPUT("A"), T.ISSUE_NEEDS_INPUT("C")]);
  });
  it("blocks on preflight's fresher answer, not the preview's stale one", () => {
    const rows = accessRows([custom({ status: "ready" })], [custom({ status: "needs_input" })]);
    expect(accessIssues(rows)).toHaveLength(1);
    expect(accessIssues(accessRows([custom({ status: "needs_input" })], [custom({ status: "ready" })]))).toEqual([]);
  });
});

describe("accessRows — a component the server refused has no fact", () => {
  const inline = { inline: { hosts: ["api.openai.com"] }, name: "Bad one" };
  it("adds no row when no read refused anything", () => {
    expect(accessRows(undefined, undefined, [inline], null)).toEqual([]);
  });
  it("builds a blocking Refused row from the ref, carrying the server's sentence", () => {
    const [row] = accessRows(undefined, undefined, [inline], "serves a model");
    expect(row).toMatchObject({
      id: "inline:0", title: "Bad one", status: "refused", blocking: true, hosts: ["api.openai.com"],
      statusNote: "serves a model", issueText: T.ISSUE_REFUSED("Bad one"),
    });
  });
  it("leaves a ref that has a fact to the fact", () => {
    const rows = accessRows([custom({ id: "inline:0", name: "Bad one" })], undefined, [inline, { inline: { hosts: ["x.example"] }, name: "Other" }], "no");
    expect(rows.map((r) => [r.id, r.status])).toEqual([["inline:0", "ready"], ["inline:1", "refused"]]);
  });
  it("names a stored ref by its id and gives it no reason line", () => {
    const [row] = accessRows(undefined, undefined, [{ id: "6f0c1d2e-0000-4000-8000-0000000c0301" }], "denied");
    expect(row).toMatchObject({ id: "6f0c1d2e-0000-4000-8000-0000000c0301", title: T.TITLE.custom_unnamed, reason: "", blocking: true });
  });
});
