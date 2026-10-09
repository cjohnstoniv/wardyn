/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import {
  blankSecret,
  blankSetting,
  hostError,
  placeServerError,
  refIndexForFact,
  secretField,
  settingField,
  validateDraft,
  type ComponentDraft,
} from "./custom-component-form-model";

const base = (over: Partial<ComponentDraft> = {}): ComponentDraft => ({
  name: "Acme",
  hosts: "api.acme.test",
  secrets: [],
  settings: [],
  ...over,
});
const OPTS = { named: true, residentAllowed: true };

describe("hostError: the rules a person's hosts must satisfy", () => {
  it.each([
    "api.example.com",
    "*.example.com",
    "api.example.com:8443",
    "*.example.com:443",
    "xn--bcher-kva.example",
  ])("accepts %s", (h) => expect(hostError(h)).toBeNull());

  it.each([
    ["10.0.0.1", /IP address/],
    ["127.1", /IP address/],
    ["2130706433", /IP address/],
    ["0x7f.1", /IP address/],
    ["0177.0.0.1", /IP address/],
    ["10.0.0.1:8080", /IP address/],
    ["[::1]", /IP address/],
    ["[::1]:443", /IP address/],
    ["2001:db8::1", /IP address/],
    ["::ffff:10.0.0.1", /IP address/],
    ["API.Example.com", /Write "API.Example.com" as "api.example.com"/],
    ["api.example.com.", /Write "api.example.com." as "api.example.com"/],
    ["https://api.example.com", /bare host, not a URL/],
    ["api.example.com/v1", /bare host, not a URL/],
    ["api.example.com:99999", /port must be a number/],
    ["api.example.com:abc", /port must be a number/],
    ["a.*.example.com", /only allowed as a leading/],
    ["*", /only allowed as a leading/],
    ["bücher.example", /ASCII letters/],
  ])("refuses %s", (h, why) => expect(hostError(h)).toMatch(why));
});

describe("validateDraft", () => {
  it("passes a complete definition", () => {
    const secret = { ...blankSecret(), secret_name: "acme-key", mode: "header" as const };
    expect(validateDraft(base({ secrets: [secret] }), OPTS)).toEqual({});
  });

  it("a saved component needs a name; one for this run may go unnamed", () => {
    expect(validateDraft(base({ name: " " }), OPTS).name).toMatch(/Give it a name/);
    expect(validateDraft(base({ name: "" }), { ...OPTS, named: false })).toEqual({});
  });

  it("names the host line it refuses, and counts a repeat", () => {
    expect(validateDraft(base({ hosts: "10.0.0.1" }), OPTS).hosts).toMatch(/10\.0\.0\.1.*IP address/);
    expect(validateDraft(base({ hosts: "a.test\na.test" }), OPTS).hosts).toMatch(/listed twice/);
  });

  describe("header delivery", () => {
    const header = (over: object) => ({ ...blankSecret(), secret_name: "k", mode: "header" as const, ...over });

    it("needs one of the entered hosts, bare", () => {
      const s = header({ host: "elsewhere.test" });
      expect(validateDraft(base({ secrets: [s] }), OPTS)[secretField(s, "host")]).toMatch(/Pick the host/);
    });

    it("offers a :443 host bare and a port-qualified host never", () => {
      const s = header({});
      expect(validateDraft(base({ hosts: "api.acme.test:443", secrets: [s] }), OPTS)).toEqual({});
      const only8443 = validateDraft(base({ hosts: "api.acme.test:8443", secrets: [s] }), OPTS);
      expect(only8443[secretField(s, "host")]).toMatch(/Pick the host/);
    });

    it("checks the header name and the value template", () => {
      const s = header({ header: "Bad Name", format: "Bearer" });
      const e = validateDraft(base({ secrets: [s] }), OPTS);
      expect(e[secretField(s, "header")]).toMatch(/isn't a valid header name/);
      expect(e[secretField(s, "format")]).toMatch(/%s exactly once/);
    });
  });

  describe("environment variable and file delivery", () => {
    it.each(["PATH", "HOME", "NODE_OPTIONS", "LD_PRELOAD", "GIT_CONFIG_COUNT", "CLAUDE_CODE_X", "HTTPS_PROXY"])(
      "refuses the reserved variable %s",
      (v) => {
        const s = { ...blankSecret(), secret_name: "k", mode: "env" as const, var: v };
        expect(validateDraft(base({ secrets: [s] }), OPTS)[secretField(s, "var")]).toMatch(/reserved/);
      },
    );

    it("refuses WARDYN_*, a lower-case name and an empty one, each with its own sentence", () => {
      const at = (v: string) => {
        const s = { ...blankSecret(), secret_name: "k", mode: "env" as const, var: v };
        return validateDraft(base({ secrets: [s] }), OPTS)[secretField(s, "var")];
      };
      expect(at("WARDYN_X")).toMatch(/WARDYN_\* configures the sandbox/);
      expect(at("lower")).toMatch(/upper-case letters/);
      expect(at("")).toMatch(/Name the variable/);
    });

    it("checks a file name, not a path", () => {
      const s = { ...blankSecret(), secret_name: "k", mode: "file" as const, file: "../etc/passwd" };
      expect(validateDraft(base({ secrets: [s] }), OPTS)[secretField(s, "file")]).toMatch(/file name/);
      const ok = { ...s, file: "token.txt" };
      expect(validateDraft(base({ secrets: [ok] }), OPTS)).toEqual({});
    });

    it("refuses both when the deployment turned them off, naming the row", () => {
      const s = { ...blankSecret(), secret_name: "k", mode: "env" as const, var: "TOKEN" };
      const e = validateDraft(base({ secrets: [s] }), { ...OPTS, residentAllowed: false });
      expect(e[secretField(s, "mode")]).toMatch(/turned off/);
    });

    it("refuses two deliveries to one place, naming the later row", () => {
      const a = { ...blankSecret(), secret_name: "a", mode: "env" as const, var: "TOKEN" };
      const b = { ...blankSecret(), secret_name: "b", mode: "env" as const, var: "TOKEN" };
      const e = validateDraft(base({ secrets: [a, b] }), OPTS);
      expect(e[secretField(a, "var")]).toBeUndefined();
      expect(e[secretField(b, "var")]).toMatch(/same place as secret 1/);
    });
  });

  it("refuses a secret name the store would not hold", () => {
    const s = { ...blankSecret(), secret_name: "Not Valid", mode: "env" as const, var: "T" };
    expect(validateDraft(base({ secrets: [s] }), OPTS)[secretField(s, "secret_name")]).toMatch(/isn't a valid secret name/);
  });

  it("settings: a reserved or repeated name, and a name that is also a delivered variable", () => {
    const reserved = { ...blankSetting(), name: "PATH", value: "x" };
    const a = { ...blankSetting(), name: "REGION", value: "x" };
    const b = { ...blankSetting(), name: "REGION", value: "y" };
    const env = { ...blankSecret(), secret_name: "k", mode: "env" as const, var: "TOKEN" };
    const clash = { ...blankSetting(), name: "TOKEN", value: "z" };
    const e = validateDraft(base({ settings: [reserved, a, b, clash], secrets: [env] }), OPTS);
    expect(e[settingField(reserved, "name")]).toMatch(/reserved/);
    expect(e[settingField(a, "name")]).toBeUndefined();
    expect(e[settingField(b, "name")]).toMatch(/listed twice/);
    expect(e[settingField(clash, "name")]).toMatch(/also the variable/);
  });
});

describe("placeServerError: a refusal lands on the field it names", () => {
  const s0 = { ...blankSecret(), secret_name: "a" };
  const s1 = { ...blankSecret(), secret_name: "b" };
  const cfg = { ...blankSetting(), name: "REGION", value: "x" };
  const d = base({ secrets: [s0, s1], settings: [cfg] });

  it.each([
    ["invalid component: definition.secrets[1].delivery.var: \"PATH\" is reserved", secretField(s1, "var")],
    ["invalid component: definition.secrets[0].delivery.host: \"x\" must be one host", secretField(s0, "host")],
    ["invalid component: definition.secrets[0].secret_name: \"a\" is managed by Wardyn", secretField(s0, "secret_name")],
    ["invalid component: definition.hosts[2]: \"x\" must be a DNS name", "hosts"],
    ["invalid component: name: must be 1 to 64 characters", "name"],
    ["invalid component: definition.config[REGION]: value exceeds 4096 bytes", settingField(cfg, "value")],
    ["invalid component: definition.config: 40 keys exceeds the 32-key limit", "settings"],
    ["invalid component: definition.secrets: 9 entries exceeds the 8-secret limit", "secrets"],
  ])("%s", (msg, field) => {
    const placed = placeServerError(msg, d);
    expect(Object.keys(placed)).toEqual([field]);
    expect(placed[field]).not.toMatch(/^invalid component: /);
  });

  it("falls back to the form for a sentence that names no field", () => {
    expect(placeServerError("too many saved components (max 32); remove one first", d)).toEqual({
      form: "too many saved components (max 32); remove one first",
    });
  });
});

describe("refIndexForFact", () => {
  const refs = [{ id: "u1" }, { inline: { hosts: [] } }, { id: "u2" }];
  it.each([
    ["u1", 0],
    ["u2", 2],
    ["inline:1", 1],
    ["inline:0", -1], // position 0 is a stored component, not an inline one
    ["inline:7", -1],
    ["git_provider:github:app", -1],
  ])("%s -> %i", (fact, at) => expect(refIndexForFact(fact, refs)).toBe(at));
});
