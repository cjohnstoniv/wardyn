/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The four narrowing keys on a git_pat grant's scope (types.GitPATScope on the
// Go side). GrantSpec.scope stays free-form on the wire type; this is the one
// place the console reads the four keys out of it, for the policy editor and
// the run's Policy tab alike.
//
// Omission defaults mirror GitPATScope.Normalize: repos absent = every
// repository the token reaches, an EMPTY list = none; access write; api false;
// forge generic.
import type { GrantSpec } from "./types";

export type PATAxis = "forge" | "repos" | "access" | "api";

export interface PATScopeView {
  host: string;
  secretName: string;
  /** undefined = every repository; [] = none. */
  repos: string[] | undefined;
  /** The raw wire value: "" when absent. */
  access: string;
  api: boolean;
  /** The raw wire value: "" when absent (generic). */
  forge: string;
}

export function readPATScope(scope: Record<string, unknown> | undefined): PATScopeView {
  const s = scope ?? {};
  const str = (v: unknown) => (typeof v === "string" ? v : "");
  return {
    host: str(s.host),
    secretName: str(s.secret_name),
    repos: Array.isArray(s.repos) ? s.repos.map(String) : undefined,
    access: str(s.access),
    api: s.api === true,
    forge: str(s.forge),
  };
}

/** The git_pat grants of a spec with their index in eligible_grants (the index
 *  a server refusal names). A non-list or a non-object entry reads as absent. */
export function gitPATGrants(grants: unknown): { index: number; grant: GrantSpec }[] {
  if (!Array.isArray(grants)) return [];
  const out: { index: number; grant: GrantSpec }[] = [];
  grants.forEach((g, index) => {
    if (g && typeof g === "object" && (g as GrantSpec).kind === "git_pat") out.push({ index, grant: g as GrantSpec });
  });
  return out;
}

export interface PATAxisError {
  index: number;
  axis: PATAxis;
  /** The server's sentence without its "eligible_grants[n]: " prefix. */
  sentence: string;
}

// The refusals that name one axis of one grant (internal/api/git_pat_scope.go
// and the strict scope decode, and the governance ceiling check):
//   eligible_grants[0]: git_pat scope invalid: git_pat scope access "x" is not one of read, write
//   eligible_grants[0]: git_pat scope invalid: git_pat scope repos entry "x" is malformed (...)
//   eligible_grants[0]: git_pat scope invalid: git_pat scope api: true is not available on the generic forge (...)
//   eligible_grants[0]: git_pat scope api: true on forge bitbucket_server needs ...
// A refusal that names no single axis (an unknown key, two grants for one host,
// an Azure DevOps host) matches nothing and stays in the editor's error banner.
const GRANT_INDEX = /eligible_grants\[(\d+)\]:\s*/;
const AXIS_NAMED = /git_pat (?:scope )?(repos|access|api|forge)\b/;

export function patAxisError(message: string | null | undefined): PATAxisError | null {
  if (!message) return null;
  const at = GRANT_INDEX.exec(message);
  if (!at) return null;
  const sentence = message.slice(at.index + at[0].length);
  const axis = AXIS_NAMED.exec(sentence);
  if (!axis) return null;
  return { index: Number(at[1]), axis: axis[1] as PATAxis, sentence };
}
