/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { CHANGES } from "../../../../lib/governance-copy";
import { ASSISTANT as T } from "../../../wardyn/copy/assistant";
import { hostError, hostListError } from "../../new-run-flow/checks/custom-component-form-model";
import { SUMMARY } from "../../run-detail/policy-tab-copy";
import { PartCard, parsePartText, valueLines, type PartCardProps } from "./part-card";

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

describe("parsePartText", () => {
  it("judges each host of a host list with Guided setup's own host check", () => {
    const bad = "https://x.example/path";
    expect(hostError(bad)).not.toBeNull();
    expect(parsePartText("allowed_domains", JSON.stringify(["github.com", bad]))).toEqual({ error: hostError(bad) });
    expect(parsePartText("allowed_domains", '["github.com"]')).toEqual({ value: ["github.com"] });
  });

  it("refuses a host listed twice with Guided setup's own sentence, however long the list", () => {
    const twice = hostListError(["github.com", "github.com"]);
    expect(twice).toBeTruthy();
    expect(parsePartText("allowed_domains", '["github.com","github.com"]')).toEqual({ error: twice });
    expect(parsePartText("denied_domains", '["a.example","b.example","a.example"]')).toEqual({ error: hostListError(["a.example", "b.example", "a.example"]) });
    const many = Array.from({ length: 60 }, (_, i) => `h${i}.example`);
    expect(parsePartText("allowed_domains", JSON.stringify(many))).toEqual({ value: many });
    expect(parsePartText("allowed_domains", JSON.stringify([...many, "h3.example"]))).toEqual({ error: hostListError([...many, "h3.example"]) });
  });

  it("judges the host of each rule of host_rules, which may name a host more than once", () => {
    const bad = "https://x.example/path";
    const rules = (...hosts: string[]) => JSON.stringify([{ rules: hosts.map((host) => ({ host, effect: "allow" })) }]);
    expect(parsePartText("host_rules", rules("github.com", bad))).toEqual({ error: hostError(bad) });
    expect(parsePartText("host_rules", rules("github.com", "github.com"))).toEqual({ value: JSON.parse(rules("github.com", "github.com")) });
    expect(parsePartText("host_rules", '{"rules":1}')).toEqual({ error: T.PART_EDIT_INVALID });
  });

  it("takes an IPv4 host in a rule as the server does, and refuses a wildcard or a badly written address", () => {
    const rules = (host: string) => JSON.stringify([{ rules: [{ host, effect: "deny" }] }]);
    expect(parsePartText("host_rules", rules("10.0.0.5"))).toEqual({ value: JSON.parse(rules("10.0.0.5")) });
    expect(parsePartText("host_rules", rules("*.github.com"))).toEqual({ error: T.RULE_HOST_WILDCARD("*.github.com") });
    expect(parsePartText("host_rules", rules("010.0.0.5"))).toHaveProperty("error");
    expect(parsePartText("host_rules", rules("300.0.0.5"))).toHaveProperty("error");
    expect(parsePartText("host_rules", rules("10.0.0.5:80"))).toHaveProperty("error");
    // A host list is still a DNS-name list: an address is refused there.
    expect(parsePartText("allowed_domains", '["10.0.0.5"]')).toHaveProperty("error");
  });

  it("wants a list of strings for a host list, and any JSON for another field", () => {
    expect(parsePartText("denied_domains", '{"a":1}')).toEqual({ error: T.PART_EDIT_INVALID });
    expect(parsePartText("workspace_repos", '[{"url":"x"}]')).toEqual({ value: [{ url: "x" }] });
  });
});

describe("valueLines", () => {
  it("reads a value as the lines the card shows", () => {
    expect(valueLines({ a: ["x", "y"], b: 1 })).toEqual(["a: x, y", "b: 1"]);
  });
});
