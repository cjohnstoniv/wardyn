/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin editor's draft and its conversion to the wire. The server refuses a
// delivery that carries another mode's fields, so requestOf writes only the
// fields the chosen mode uses, and `shared` only on a header delivery.
import type { Component, ComponentDeliveryMode, ComponentRequest, ComponentSecret } from "../../../lib/types";

export interface SecretDraft {
  // Stable React key; never sent.
  key: number;
  secret_name: string;
  shared: boolean;
  mode: ComponentDeliveryMode;
  host: string;
  header: string;
  format: string;
  plain_http: boolean;
  var: string;
  file: string;
}

export interface SettingDraft {
  key: number;
  name: string;
  value: string;
}

export interface ComponentDraft {
  name: string;
  // One host per line.
  hosts: string;
  secrets: SecretDraft[];
  settings: SettingDraft[];
}

let nextKey = 1;

export const blankSecret = (): SecretDraft => ({
  key: nextKey++,
  secret_name: "",
  shared: false,
  mode: "header",
  host: "",
  header: "",
  format: "",
  plain_http: false,
  var: "",
  file: "",
});

export const blankSetting = (): SettingDraft => ({ key: nextKey++, name: "", value: "" });

export function draftOf(c: Component | null): ComponentDraft {
  if (!c) return { name: "", hosts: "", secrets: [], settings: [] };
  return {
    name: c.name,
    hosts: c.definition.hosts.join("\n"),
    secrets: (c.definition.secrets ?? []).map((s) => ({
      ...blankSecret(),
      secret_name: s.secret_name,
      shared: !!s.shared,
      mode: s.delivery.mode,
      host: s.delivery.host ?? "",
      header: s.delivery.header ?? "",
      format: s.delivery.format ?? "",
      plain_http: !!s.delivery.plain_http,
      var: s.delivery.var ?? "",
      file: s.delivery.file ?? "",
    })),
    settings: Object.entries(c.definition.config ?? {}).map(([name, value]) => ({ ...blankSetting(), name, value })),
  };
}

export const hostLines = (text: string): string[] =>
  text
    .split("\n")
    .map((h) => h.trim())
    .filter(Boolean);

// The hosts a header delivery may name: bare, or on :443 (types.validHeaderHost),
// and never a wildcard. Offered without the port.
export function headerHostChoices(hosts: string[]): string[] {
  const out: string[] = [];
  for (const h of hosts) {
    const bare = h.endsWith(":443") ? h.slice(0, -4) : h;
    if (!bare || bare.includes(":") || bare.startsWith("*") || out.includes(bare)) continue;
    out.push(bare);
  }
  return out;
}

function secretOf(s: SecretDraft, choices: string[]): ComponentSecret {
  const secret: ComponentSecret = { secret_name: s.secret_name.trim(), delivery: { mode: s.mode } };
  if (s.mode === "env") {
    secret.delivery.var = s.var.trim();
  } else if (s.mode === "file") {
    secret.delivery.file = s.file.trim();
  } else {
    secret.delivery.host = s.host || (choices.length === 1 ? choices[0] : "");
    if (s.header.trim()) secret.delivery.header = s.header.trim();
    if (s.format.trim()) secret.delivery.format = s.format.trim();
    if (s.plain_http) secret.delivery.plain_http = true;
    if (s.shared) secret.shared = true;
  }
  return secret;
}

export function requestOf(d: ComponentDraft): ComponentRequest {
  const hosts = hostLines(d.hosts);
  const choices = headerHostChoices(hosts);
  const config = Object.fromEntries(d.settings.filter((s) => s.name.trim()).map((s) => [s.name.trim(), s.value]));
  return {
    name: d.name.trim(),
    definition: {
      hosts,
      ...(d.secrets.length > 0 && { secrets: d.secrets.map((s) => secretOf(s, choices)) }),
      ...(Object.keys(config).length > 0 && { config }),
    },
  };
}

// crypto.randomUUID exists only in secure contexts (HTTPS or localhost), and the console
// may be reached without either; getRandomValues does not have that limit.
export function newUuid(): string {
  if (typeof crypto.randomUUID === "function") return crypto.randomUUID();
  const b = crypto.getRandomValues(new Uint8Array(16));
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  const h = Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}
