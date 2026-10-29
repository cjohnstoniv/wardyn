/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The caller strings a port's `callers` holds, and how the picker writes them. The server's grammar is in
// internal/api/policy_ui_callers.go: entries are user:, group: or service:, lower-case, unique and at most 32.

import { NR_RUN } from "../../../../wardyn/copy/new-run/run";

/** The server's maxUIAppCallers. */
export const MAX_CALLERS = 32;

export type CallerKind = "user" | "group" | "service";

export const callerKind = (caller: string): CallerKind | undefined => {
  const kind = caller.slice(0, caller.indexOf(":"));
  return kind === "user" || kind === "group" || kind === "service" ? kind : undefined;
};

export const callerKindLabel = (kind: CallerKind | undefined): string | undefined =>
  kind === "user" ? NR_RUN.CALLER_PERSON : kind === "group" ? NR_RUN.CALLER_GROUP : kind === "service" ? NR_RUN.CALLER_SERVICE : undefined;

/** What a chip shows for a caller: the value without its prefix. */
export const callerValue = (caller: string): string => (callerKind(caller) ? caller.slice(caller.indexOf(":") + 1) : caller);

// A full address: something, an @, a host with a dot. The server takes any lower-case `sub` or email; the picker offers email only.
const EMAIL = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

/** `user:<email>`, lower-cased, or undefined for text that is not a whole address. */
export function personCaller(text: string): string | undefined {
  const email = text.trim().toLowerCase();
  return EMAIL.test(email) ? `user:${email}` : undefined;
}

/** `user:<sub or email>` for a directory person, lower-cased as the server stores it; undefined for a value with a space or control character. */
export function userCaller(value: string): string | undefined {
  const sub = value.trim().toLowerCase();
  return sub && !/[\s\x00-\x1f\x7f]/.test(sub) ? `user:${sub}` : undefined;
}

// The server's CanonicalGroupSubject: printable ASCII, lower-cased, trimmed. A name outside it cannot be a caller.
const PRINTABLE = /^[\x20-\x7e]+$/;

/** `group:<value>` as the server stores it, or undefined for a value it would refuse. */
export function groupCaller(value: string): string | undefined {
  const name = value.trim().toLowerCase();
  return name && PRINTABLE.test(name) ? `group:${name}` : undefined;
}

// serviceNameRE in internal/api/service_identities.go.
const SERVICE_NAME = /^[a-z][a-z0-9-]{2,62}$/;

/** `service:<name>`, or undefined for a name the server would refuse. */
export const serviceCaller = (name: string): string | undefined => (SERVICE_NAME.test(name) ? `service:${name}` : undefined);
