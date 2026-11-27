/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { CHANGES } from "../../../../lib/governance-copy";
import { ASSISTANT as T } from "../../../wardyn/copy/assistant";
import { hostListError } from "../../new-run-flow/checks/custom-component-form-model";
import { SUMMARY } from "../../run-detail/policy-tab-copy";
import { domainEntryError, PartCard, parsePartText, valueLines, type PartCardProps } from "./part-card";

afterEach(cleanup);

function card(over: Partial<PartCardProps> = {}) {
  const handlers = { onAccept: vi.fn(), onDiscard: vi.fn(), onEdit: vi.fn() };
  render(<PartCard section="allowed_domains" value={["github.com"]} status="proposed" edited={false} fromBuildFile={false} {...handlers} {...over} />);
  return handlers;
}

describe("PartCard", () => {
  it("names the part in the Summary's words and offers Accept and Discard while it is only proposed", async () => {
    const h = card();
    expect(screen.getByText(SUMMARY.allowedHosts)).toBeInTheDocument();
    expect(screen.getByText(T.PART_PROPOSED)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: T.PART_ACCEPT }));
    expect(h.onAccept).toHaveBeenCalledTimes(1);
    await userEvent.click(screen.getByRole("button", { name: T.PART_DISCARD }));
    expect(h.onDiscard).toHaveBeenCalledTimes(1);
  });

  it("shows Accepted, and Edited by you once the person changed the value", () => {
    card({ status: "accepted" });
    expect(screen.getByText(T.PART_ACCEPTED)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: T.PART_ACCEPT })).toBeNull();
    cleanup();
    card({ status: "accepted", edited: true });
    expect(screen.getByText(T.PART_EDITED)).toBeInTheDocument();
    expect(screen.queryByText(T.PART_ACCEPTED)).toBeNull();
  });

  it("marks a part as from a build file only when the server's part says so", () => {
    card({ value: ["Dockerfile builds on Node 20, from a build file"], fromBuildFile: false });
    expect(screen.queryByText(T.PART_FROM_BUILD_FILE)).toBeNull();
    cleanup();
    card({ fromBuildFile: true });
    expect(screen.getByText(T.PART_FROM_BUILD_FILE)).toBeInTheDocument();
  });

  it("edits an accepted part as the field's own value and hands the parsed value back", async () => {
    const h = card({ status: "accepted" });
    await userEvent.click(screen.getByRole("button", { name: CHANGES.OP.update }));
    const box = screen.getByRole("textbox");
    await userEvent.clear(box);
    await userEvent.click(box);
    await userEvent.paste('["github.com", "registry.npmjs.org"]');
    await userEvent.click(screen.getByRole("button", { name: T.PART_EDIT_DONE }));
    expect(h.onEdit).toHaveBeenCalledWith(["github.com", "registry.npmjs.org"]);
  });

  it("refuses text that is not JSON and hands nothing back", async () => {
    const h = card({ status: "accepted" });
    await userEvent.click(screen.getByRole("button", { name: CHANGES.OP.update }));
    const box = screen.getByRole("textbox");
    await userEvent.clear(box);
    await userEvent.type(box, "not json");
    await userEvent.click(screen.getByRole("button", { name: T.PART_EDIT_DONE }));
    expect(screen.getByRole("alert")).toHaveTextContent(T.PART_EDIT_INVALID);
    expect(h.onEdit).not.toHaveBeenCalled();
  });
});

describe("domainEntryError", () => {
  // internal/egress/proxy/policy.go ValidDomainEntry: a host, "*." and a host, an IP address, each with an optional port.
  it("accepts what the proxy's domain check accepts, literal IPs included", () => {
    for (const ok of ["github.com", "*.github.com", "github.com:443", "*.github.com:443", "10.0.0.5", "10.0.0.5:443", "fd00::1", "[fd00::1]:443", "XN--nxasmq6b.example"]) {
      expect(domainEntryError(ok), ok).toBeNull();
    }
  });

  it("refuses what it refuses", () => {
    for (const bad of ["", "https://x.example/path", "a b.example", "*", "a.*.example", "github.com:0", "github.com:65536", "github.com:x", "010.0.0.5", "300.0.0.5", "134744072", "0x7f.0.0.1", "0x.1.2.3", "0xc0.0.2.1", "[fd00::1]", "*.fd00::1", "ünï.example", "*.10.0.0.5"]) {
      expect(domainEntryError(bad), bad).toBe(T.DOMAIN_ENTRY_BAD(bad));
    }
  });
});

describe("parsePartText", () => {
  it("judges each entry of a domain list with the proxy's own rules, and takes a literal IP", () => {
    const bad = "https://x.example/path";
    expect(parsePartText("allowed_domains", JSON.stringify(["github.com", bad]))).toEqual({ error: T.DOMAIN_ENTRY_BAD(bad) });
    expect(parsePartText("allowed_domains", '["github.com","10.0.0.5","*.example.com:443"]')).toEqual({ value: ["github.com", "10.0.0.5", "*.example.com:443"] });
    expect(parsePartText("denied_domains", '["10.0.0.5"]')).toEqual({ value: ["10.0.0.5"] });
    // The server trims an entry before it judges it.
    expect(domainEntryError("  github.com ")).toBeNull();
    expect(domainEntryError(" 0x.1.2.3")).toBe(T.DOMAIN_ENTRY_BAD(" 0x.1.2.3"));
    expect(parsePartText("allowed_domains", '[" github.com"]')).toEqual({ value: [" github.com"] });
  });

  it("refuses a host listed twice with Guided setup's own sentence, however long the list", () => {
    const twice = hostListError(["github.com", "github.com"], domainEntryError);
    expect(twice).toBeTruthy();
    expect(parsePartText("allowed_domains", '["github.com","github.com"]')).toEqual({ error: twice });
    expect(parsePartText("denied_domains", '["a.example","b.example","a.example"]')).toEqual({ error: hostListError(["a.example", "b.example", "a.example"]) });
    const many = Array.from({ length: 60 }, (_, i) => `h${i}.example`);
    expect(parsePartText("allowed_domains", JSON.stringify(many))).toEqual({ value: many });
    expect(parsePartText("allowed_domains", JSON.stringify([...many, "h3.example"]))).toEqual({ error: hostListError([...many, "h3.example"]) });
  });

  it("judges the host of each rule of host_rules as the server does, and allows the same host in two rules", () => {
    const rules = (...hosts: string[]) => JSON.stringify([{ rules: hosts.map((host) => ({ host, effect: "allow" })) }]);
    for (const ok of ["github.com", "10.0.0.5", "my_host.internal"]) expect(parsePartText("host_rules", rules(ok)), ok).toEqual({ value: JSON.parse(rules(ok)) });
    expect(parsePartText("host_rules", rules("github.com", "github.com"))).toEqual({ value: JSON.parse(rules("github.com", "github.com")) });
    expect(parsePartText("host_rules", rules("*.github.com"))).toEqual({ error: T.RULE_HOST_WILDCARD("*.github.com") });
    // internal/types/host_rules.go validHostRule: no port, path, trailing dot, upper case; an IPv4 is written in full.
    // A host every label of which a resolver reads as a number is an address, and only the standard spelling is taken.
    for (const bad of ["0xc0.0.2.1", "0x.1.2.3", "0x7f.1", "0X7F.0.0.1", "1.2.3", "0x"]) {
      expect(parsePartText("host_rules", rules("ok.example", bad)), bad).toEqual({ error: T.RULE_HOST_BAD(bad) });
    }
    expect(parsePartText("host_rules", rules("0xc0.example"))).toHaveProperty("value");
    for (const bad of ["github.com:443", "10.0.0.5:80", "https://x.example/path", "github.com.", ".github.com", "GitHub.com", "010.0.0.5", "300.0.0.5", "", "a b", "[::1]", "ünï.example"]) {
      expect(parsePartText("host_rules", rules("ok.example", bad)), bad).toEqual({ error: T.RULE_HOST_BAD(bad) });
    }
    expect(parsePartText("host_rules", '{"rules":1}')).toEqual({ error: T.PART_EDIT_LIST });
  });

  it("takes null for allowed_domains, which the server writes for no list, and for no other field", () => {
    expect(parsePartText("allowed_domains", "null")).toEqual({ value: null });
    expect(parsePartText("denied_domains", "null")).toEqual({ error: T.PART_EDIT_LIST });
    expect(parsePartText("ui_apps", "null")).toEqual({ error: T.PART_EDIT_LIST });
  });

  it("wants the kind of value a known field holds, and any JSON for another", () => {
    expect(parsePartText("denied_domains", '{"a":1}')).toEqual({ error: T.PART_EDIT_LIST });
    expect(parsePartText("ui_apps", "{}")).toEqual({ error: T.PART_EDIT_LIST });
    expect(parsePartText("eligible_grants", '"x"')).toEqual({ error: T.PART_EDIT_LIST });
    expect(parsePartText("resources", "[]")).toEqual({ error: T.PART_EDIT_OBJECT });
    expect(parsePartText("llm_inspection", "null")).toEqual({ error: T.PART_EDIT_OBJECT });
    expect(parsePartText("allow_all_egress", "[]")).toEqual({ error: T.PART_EDIT_VALUE });
    expect(parsePartText("max_holds", '{"a":1}')).toEqual({ error: T.PART_EDIT_VALUE });
    expect(parsePartText("ui_apps", "[]")).toEqual({ value: [] });
    expect(parsePartText("allow_all_egress", "true")).toEqual({ value: true });
    expect(parsePartText("workspace_repos", '[{"url":"x"}]')).toEqual({ value: [{ url: "x" }] });
    expect(parsePartText("something_else", '{"a":1}')).toEqual({ value: { a: 1 } });
  });
});

describe("valueLines", () => {
  it("reads a value as the lines the card shows", () => {
    expect(valueLines({ a: ["x", "y"], b: 1 })).toEqual(["a: x, y", "b: 1"]);
  });
});
