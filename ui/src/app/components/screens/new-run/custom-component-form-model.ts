/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The custom-component form as data (#1914): what a person's definition must
// satisfy before it is offered to the server, and where a server refusal
// belongs on the form. The server's Validate (internal/types/component.go) is
// the authority; this mirrors the rules a person can trip so the sentence lands
// on the field before the round trip, and so a refusal the server alone can
// make still finds its field. Pure: the dialog, the saved-components card and
// the Add list all read from here.
//
// A person's row differs from an organisation's: hosts are DNS names (A2), a
// header goes over TLS only (D6, no plain-HTTP control), and a secret is
// always the person's own. The form never produces `shared` or `plain_http`.
import type { ComponentDeliveryMode, ComponentRef } from "../../../lib/types";
import { CUSTOM_COMPONENT as T } from "../../wardyn/copy/components";
import {
  headerHostChoices,
  hostLines,
  type ComponentDraft,
  type SecretDraft,
} from "../components/component-editor-model";

export {
  blankSecret,
  blankSetting,
  draftOf,
  headerHostChoices,
  hostLines,
  requestOf,
  type ComponentDraft,
  type SecretDraft,
  type SettingDraft,
} from "../components/component-editor-model";

/** Field path to the sentence about it. "form" is a refusal no field owns. */
export type FormErrors = Record<string, string>;

export const secretField = (s: { key: number }, field: string): string => `secrets.${s.key}.${field}`;
export const settingField = (s: { key: number }, field: "name" | "value"): string => `settings.${s.key}.${field}`;

// Mirrors of the server's limits and patterns; the file is internal/types/component.go.
const MAX_HOSTS = 32;
const MAX_SECRETS = 8;
const MAX_CONFIG = 32;
const MAX_NAME_RUNES = 64;
const MAX_ENV_NAME = 128;
const MAX_FORMAT = 512;
const MAX_CONFIG_VALUE = 4096;
const DELIVERY_FIELD = { header: "host", env: "var", file: "file" } as const;
const SECRET_NAME_RE = /^[a-z0-9]([a-z0-9._-]{0,126}[a-z0-9])?$/;
const ENV_NAME_RE = /^[A-Z_][A-Z0-9_]*$/;
const FILE_RE = /^[a-z0-9][a-z0-9_.-]{0,62}$/;
const HEADER_NAME_RE = /^[A-Za-z0-9!#$%&'*+\-.^_`|~]{1,128}$/;
const RESERVED_ENV = new Set([
  "PATH", "HOME", "SHELL", "BASH_ENV", "ENV", "PROMPT_COMMAND", "NODE_OPTIONS", "NODE_PATH",
  "PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP", "PERL5OPT", "PERL5LIB", "RUBYOPT", "RUBYLIB",
  "GIT_EXEC_PATH", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_PROXY_COMMAND", "CLAUDE_CONFIG_DIR",
  "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR",
  "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "GIT_SSL_CAINFO", "GIT_SSL_NO_VERIFY",
]);
const RESERVED_ENV_PREFIXES = ["LD_", "GIT_CONFIG_", "CLAUDE_CODE_"];

// A character a line break or other control would be (types.printable).
const CONTROL_RE = /[\u0000-\u001f\u007f]/;

const runes = (s: string): number => Array.from(s).length;

/** Why `name` cannot be an environment variable or config key, or null. */
export function envNameError(name: string): string | null {
  if (name.length > MAX_ENV_NAME) return T.ERR.ENV_TOO_LONG(MAX_ENV_NAME);
  if (!ENV_NAME_RE.test(name)) return T.ERR.ENV_SHAPE(name);
  if (name.startsWith("WARDYN_")) return T.ERR.ENV_WARDYN(name);
  if (RESERVED_ENV.has(name) || RESERVED_ENV_PREFIXES.some((p) => name.startsWith(p))) return T.ERR.ENV_RESERVED(name);
  return null;
}

// A resolver may read a name whose last label is a number as an address
// (127.1, 2130706433, 0x7f.1): decimal, or hex after "0x".
function numericLabel(s: string): boolean {
  if (/^0x/i.test(s)) return /^0x[0-9a-f]*$/i.test(s);
  return /^[0-9]+$/.test(s);
}

/** Splits a host entry the way the proxy reads it: lower case, no trailing dot, an optional valid port. */
function parseHost(entry: string): { host: string; port: number; wildcard: boolean } | null {
  let d = entry.trim().toLowerCase().replace(/\.+$/, "");
  let port = 0;
  const m = /^(.*):(\d+)$/.exec(d);
  if (m && !m[1].includes(":")) {
    const n = Number(m[2]);
    if (n >= 1 && n <= 65535) {
      d = m[1];
      port = n;
    }
  }
  const wildcard = d.startsWith("*.");
  const host = wildcard ? d.slice(2) : d;
  return host ? { host, port, wildcard } : null;
}

const canonicalHost = (p: { host: string; port: number; wildcard: boolean }): string =>
  `${p.wildcard ? "*." : ""}${p.host}${p.port ? `:${p.port}` : ""}`;

// A colon inside the host that is not a port: an address in IPv6 form, bracketed or not.
const isIPv6Shape = (h: string): boolean => /[[\]%]/.test(h) || (/^[0-9a-f:.]+$/.test(h) && (h.match(/:/g) ?? []).length >= 2);

/** The sentence for one host line a person may not write, or null. */
export function hostError(entry: string): string | null {
  const p = parseHost(entry);
  if (!p) return T.ERR.HOST_EMPTY;
  if (/[/\s]/.test(entry.trim())) return T.ERR.HOST_URL(entry);
  if (p.host.includes("*")) return T.ERR.HOST_WILDCARD(entry);
  if (/[:[\]%]/.test(p.host)) return isIPv6Shape(p.host) ? T.ERR.HOST_ADDRESS(entry) : T.ERR.HOST_PORT(entry);
  if (!/^[a-z0-9.-]+$/.test(p.host)) return T.ERR.HOST_CHARSET(entry);
  if (numericLabel(p.host.split(".").pop() ?? "")) return T.ERR.HOST_ADDRESS(entry);
  const canon = canonicalHost(p);
  return entry.trim() === canon ? null : T.ERR.HOST_SPELLING(entry, canon);
}

function deliveryTarget(s: SecretDraft, choice: string): string {
  if (s.mode === "env") return `env ${s.var.trim()}`;
  if (s.mode === "file") return `file ${s.file.trim()}`;
  return `header ${choice}`;
}

/** The sentence about one secret row's delivery field, and the field it is about. */
function secretErrors(s: SecretDraft, choices: string[], residentAllowed: boolean, out: FormErrors): void {
  const name = s.secret_name.trim();
  if (!name) out[secretField(s, "secret_name")] = T.ERR.SECRET_NAME_EMPTY;
  else if (!SECRET_NAME_RE.test(name)) out[secretField(s, "secret_name")] = T.ERR.SECRET_NAME_SHAPE(name);
  const mode: ComponentDeliveryMode = s.mode;
  if (mode !== "header" && !residentAllowed) out[secretField(s, "mode")] = T.ERR.RESIDENT_OFF;
  if (mode === "header") {
    const host = s.host || (choices.length === 1 ? choices[0] : "");
    if (!host || !choices.includes(host)) out[secretField(s, "host")] = T.ERR.HEADER_HOST;
    const header = s.header.trim();
    if (header && !HEADER_NAME_RE.test(header)) out[secretField(s, "header")] = T.ERR.HEADER_NAME(header);
    const format = s.format.trim();
    if (format) {
      if ((format.match(/%s/g) ?? []).length !== 1 || (format.match(/%/g) ?? []).length !== 1) {
        out[secretField(s, "format")] = T.ERR.FORMAT_VERB(format);
      } else if (format.length > MAX_FORMAT || CONTROL_RE.test(format)) {
        out[secretField(s, "format")] = T.ERR.FORMAT_TEXT(MAX_FORMAT);
      }
    }
  } else if (mode === "env") {
    const err = envNameError(s.var.trim());
    if (err) out[secretField(s, "var")] = s.var.trim() ? err : T.ERR.VAR_EMPTY;
  } else if (!FILE_RE.test(s.file.trim())) {
    out[secretField(s, "file")] = T.ERR.FILE_SHAPE(s.file.trim());
  }
}

/**
 * Every rule the form can check before sending. `named` is true when the
 * definition is saved (a saved component needs a name; one for this run only may
 * go unnamed).
 */
export function validateDraft(d: ComponentDraft, opts: { named: boolean; residentAllowed: boolean }): FormErrors {
  const out: FormErrors = {};
  const name = d.name.trim();
  if (opts.named && !name) out.name = T.ERR.NAME_EMPTY;
  else if (runes(name) > MAX_NAME_RUNES || CONTROL_RE.test(name)) out.name = T.ERR.NAME_SHAPE(MAX_NAME_RUNES);

  const hosts = hostLines(d.hosts);
  const hostErr = hosts.flatMap((h, i) => {
    const e = hostError(h) ?? (hosts.indexOf(h) < i ? T.ERR.HOST_TWICE(h) : null);
    return e ? [e] : [];
  });
  if (hosts.length > MAX_HOSTS) out.hosts = T.ERR.HOSTS_COUNT(hosts.length, MAX_HOSTS);
  else if (hostErr.length > 0) out.hosts = hostErr[0];

  if (d.secrets.length > MAX_SECRETS) out.secrets = T.ERR.SECRETS_COUNT(d.secrets.length, MAX_SECRETS);
  const choices = headerHostChoices(hosts);
  const seen = new Map<string, number>();
  d.secrets.forEach((s, i) => {
    secretErrors(s, choices, opts.residentAllowed, out);
    const choice = s.host || (choices.length === 1 ? choices[0] : "");
    const target = deliveryTarget(s, choice);
    const first = seen.get(target);
    // Two deliveries into one place overwrite each other: the later row is the one named.
    const where = secretField(s, DELIVERY_FIELD[s.mode]);
    if (first !== undefined && !out[where]) out[where] = T.ERR.SAME_PLACE(first + 1);
    if (first === undefined) seen.set(target, i);
  });

  const filled = d.settings.filter((s) => s.name.trim());
  if (filled.length > MAX_CONFIG) out.settings = T.ERR.CONFIG_COUNT(filled.length, MAX_CONFIG);
  const keys = new Set<string>();
  for (const s of filled) {
    const key = s.name.trim();
    const err = envNameError(key);
    if (err) out[settingField(s, "name")] = err;
    else if (keys.has(key)) out[settingField(s, "name")] = T.ERR.SETTING_TWICE(key);
    keys.add(key);
    if (s.value.length > MAX_CONFIG_VALUE || CONTROL_RE.test(s.value)) out[settingField(s, "value")] = T.ERR.SETTING_VALUE(MAX_CONFIG_VALUE);
    if (d.secrets.some((x) => x.mode === "env" && x.var.trim() === key)) out[settingField(s, "name")] = T.ERR.SETTING_IS_VAR(key);
  }
  return out;
}

// The part of a secret row the server's path names, by the last segment it gives.
const SERVER_SECRET_FIELD: Record<string, string> = {
  secret_name: "secret_name", shared: "secret_name", mode: "mode", host: "host", plain_http: "host",
  header: "header", format: "format", var: "var", file: "file",
};

/**
 * Puts a server refusal on the field it names. The server's sentence leads with
 * the path ("invalid component: definition.secrets[1].delivery.var: ..."), so
 * the field is the row at that index. Anything it does not name is the form's.
 */
export function placeServerError(message: string, d: ComponentDraft): FormErrors {
  const text = message.replace(/^invalid component:\s*/, "").replace(/^definition\./, "");
  const secret = /^secrets\[(\d+)\]\.(?:delivery\.)?([a-z_]+)/.exec(text);
  if (secret) {
    const row = d.secrets[Number(secret[1])];
    const field = SERVER_SECRET_FIELD[secret[2]] ?? "secret_name";
    if (row) return { [secretField(row, field)]: text };
  }
  if (/^hosts\b/.test(text)) return { hosts: text };
  if (/^name\b/.test(text)) return { name: text };
  const config = /^config\[([^\]]+)\]/.exec(text);
  if (config) {
    const row = d.settings.find((s) => s.name.trim() === config[1]);
    if (row) return { [settingField(row, "value")]: text };
  }
  if (/^config\b/.test(text)) return { settings: text };
  if (/^secrets\b/.test(text)) return { secrets: text };
  return { form: message };
}

/** The first field with a sentence, in the order the form shows them: for focus. */
export function firstErrorField(errors: FormErrors, d: ComponentDraft): string | undefined {
  const order = [
    "name",
    "hosts",
    "secrets",
    ...d.secrets.flatMap((s) => ["secret_name", "mode", "host", "header", "format", "var", "file"].map((f) => secretField(s, f))),
    "settings",
    ...d.settings.flatMap((s) => [settingField(s, "name"), settingField(s, "value")]),
    "form",
  ];
  return order.find((f) => errors[f]);
}

/** The attachment index a row's fact id stands for in the run's component list, or -1. */
export function refIndexForFact(factId: string, refs: readonly ComponentRef[]): number {
  const inline = /^inline:(\d+)$/.exec(factId);
  if (inline) {
    const at = Number(inline[1]);
    return refs[at]?.inline ? at : -1;
  }
  return refs.findIndex((r) => r.id === factId);
}

/** Whether `c`, a saved row, is already attached to the run. */
export const isAttached = (refs: readonly ComponentRef[], id: string): boolean => refs.some((r) => r.id === id);
