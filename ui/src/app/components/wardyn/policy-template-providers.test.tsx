/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";

vi.mock("../../lib/api/runs", () => ({
  runs: { gradePolicy: vi.fn().mockResolvedValue({ overall_risk: "medium", risk_assessment: [] }) },
}));

import { POLICY_TEMPLATES, PolicyPanel, minimalSpec, policyTemplates } from "./policy-panel";
import { POLICY_TEMPLATE_COPY as C } from "./copy/policy-templates";
import { seedAllowedDomains } from "../screens/new-run/wizard-types";
import { buildSpec, impliedEgressHosts } from "../screens/new-run/wizard-spec";
import { initialWizardState } from "../screens/new-run/wizard-types";
import type { SetupModelProvider } from "../../lib/types";

const prov = (kind: string, host: string, extra: Partial<SetupModelProvider> = {}): SetupModelProvider => ({
  id: kind,
  kind,
  host,
  harnesses: ["claude-code"],
  ...extra,
});
const BEDROCK = prov("bedrock_sso", "bedrock-runtime.us-east-1.amazonaws.com");
const byId = (ps: SetupModelProvider[] | undefined, id: string) => policyTemplates(ps).find((t) => t.id === id)!;
const hosts = (ps: SetupModelProvider[] | undefined, id: string) => byId(ps, id).spec.allowed_domains;

describe("provider-aware policy templates", () => {
  it.each([undefined, [], [{ ...BEDROCK, disabled: true }]])("no counted provider keeps today's templates (%#)", (ps) => {
    expect(policyTemplates(ps as SetupModelProvider[] | undefined)).toEqual(POLICY_TEMPLATES);
    expect(hosts(ps as SetupModelProvider[] | undefined, "minimal")).toEqual(["api.anthropic.com"]);
    const grants = byId(ps as SetupModelProvider[] | undefined, "model-provider").spec.eligible_grants!;
    expect(grants).toHaveLength(1);
    expect(grants[0].kind).toBe("api_key");
    expect(byId(undefined, "model-provider").hint).toBe(C.MODEL_PROVIDER_HINT_KEY);
  });

  it("Bedrock SSO gets its regional host and no api_key grant, in Minimal and Model provider only", () => {
    expect(hosts([BEDROCK], "minimal")).toEqual([BEDROCK.host]);
    expect(hosts([BEDROCK], "model-provider")).toEqual([BEDROCK.host]);
    expect(byId([BEDROCK], "model-provider").spec.eligible_grants).toEqual([]);
    expect(byId([BEDROCK], "model-provider").hint).toBe(C.MODEL_PROVIDER_HINT_SSO);
    expect(minimalSpec([BEDROCK]).allowed_domains).toEqual([BEDROCK.host]);
    const reg = hosts([BEDROCK], "registries")!;
    expect(reg).toContain(BEDROCK.host);
    expect(reg).not.toContain("api.anthropic.com");
    expect(reg).toContain("pypi.org");
  });

  it.each([
    ["anthropic_api_key", "api.anthropic.com", "x-api-key"],
    ["openai_api_key", "api.openai.com", "Authorization"],
  ])("%s keeps its vendor host and key grant", (kind, host, header) => {
    const ps = [prov(kind, host)];
    expect(hosts(ps, "model-provider")).toEqual([host]);
    const g = byId(ps, "model-provider").spec.eligible_grants!;
    expect(g).toHaveLength(1);
    expect(g[0].scope).toMatchObject({ host, header });
    expect(byId(ps, "model-provider").hint).toBe(C.MODEL_PROVIDER_HINT_KEY);
  });

  it.each(["anthropic_subscription", "bedrock_bearer", "custom_endpoint"])("%s is host only, proxy hint", (kind) => {
    const ps = [prov(kind, "gw.example.test")];
    expect(hosts(ps, "model-provider")).toEqual(["gw.example.test"]);
    expect(byId(ps, "model-provider").spec.eligible_grants).toEqual([]);
    expect(byId(ps, "model-provider").hint).toBe(C.MODEL_PROVIDER_HINT_PROXY);
  });

  it("two providers: every host, one grant per key kind, plural hints", () => {
    const ps = [prov("anthropic_api_key", "api.anthropic.com"), BEDROCK];
    expect(hosts(ps, "minimal")).toEqual(["api.anthropic.com", BEDROCK.host]);
    expect(byId(ps, "model-provider").spec.eligible_grants).toHaveLength(1);
    expect(byId(ps, "model-provider").hint).toBe(C.MODEL_PROVIDER_HINT_MANY(2));
    expect(byId(ps, "minimal").hint).toBe(C.MINIMAL_HINT_MANY);
  });

  it("CI and Allow-all are unchanged", () => {
    for (const id of ["ci", "allow-all"]) {
      expect(byId([BEDROCK], id)).toEqual(byId(undefined, id));
    }
  });

  it("a Bedrock-only install no longer seeds the wizard with api.anthropic.com", () => {
    expect(seedAllowedDomains()).toEqual(["api.anthropic.com"]);
    expect(initialWizardState("CC1", undefined, [BEDROCK]).allowedDomains).toEqual([BEDROCK.host]);
    const st = { ...initialWizardState("CC1", undefined, [BEDROCK]), llmSecretName: "k" };
    expect(impliedEgressHosts(st, [BEDROCK]).map((h) => h.host)).toEqual([BEDROCK.host]);
    expect(buildSpec(st, [], [BEDROCK]).inline_policy.allowed_domains).not.toContain("api.anthropic.com");
  });

  it("the panel shows the caption only when a provider counts", () => {
    const noop = () => {};
    const { rerender } = render(<PolicyPanel instance="policies" value="{}" onChange={noop} />);
    expect(screen.queryByText(/Model hosts follow/)).toBeNull();
    rerender(<PolicyPanel instance="policies" value="{}" onChange={noop} modelProviders={[BEDROCK]} />);
    expect(screen.getByText(C.FROM_PROVIDERS(["Amazon Bedrock"]))).toBeTruthy();
  });
});
