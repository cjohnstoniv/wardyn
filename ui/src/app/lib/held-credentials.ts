/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Which credentials a run held that Wardyn cannot take back when the run is
// killed (#1487). Read from the run's own credential.mint success rows, joined
// on data.grant_id to its grants for the kind and host, plus its env_secret
// grants (a whole-run secret is resolved into the sandbox environment at launch
// and leaves no mint row). Only the KIND and the HOST ever leave this file: a
// grant's scope, a mint row's jti and any value never reach the DOM.
//
// "readable: false" is a first-class answer, not an empty list: when the grants
// or the mint rows could not be read, or a mint names a grant this run no longer
// lists, saying nothing would read as "held nothing".
import type { AuditEvent, CredentialGrant } from "./types";

export type HeldCredential =
  | { kind: "github_token" }
  | { kind: "git_pat" | "ssh_key"; host: string }
  | { kind: "env_secret" };

export type HeldCredentials = { readable: true; items: HeldCredential[] } | { readable: false };

export function heldCredentials(
  grants: CredentialGrant[] | undefined,
  mintRows: AuditEvent[] | undefined,
): HeldCredentials {
  if (!grants || !mintRows) return { readable: false };
  const byID = new Map(grants.map((g) => [g.id, g]));
  const seen = new Set<string>();
  const items: HeldCredential[] = [];
  const add = (c: HeldCredential) => {
    const key = c.kind === "git_pat" || c.kind === "ssh_key" ? `${c.kind}@${c.host}` : c.kind;
    if (seen.has(key)) return;
    seen.add(key);
    items.push(c);
  };
  for (const g of grants) if (g.audience === "env_secret") add({ kind: "env_secret" });
  for (const row of mintRows) {
    if (row.action !== "credential.mint" || row.outcome !== "success") continue;
    const id = row.data?.grant_id;
    const grant = typeof id === "string" ? byID.get(id) : undefined;
    if (!grant) return { readable: false };
    if (grant.audience === "github_token") add({ kind: "github_token" });
    // No host, no line: wording that names no host would be invented.
    else if ((grant.audience === "git_pat" || grant.audience === "ssh_key") && grant.host) {
      add({ kind: grant.audience, host: grant.host });
    }
  }
  return { readable: true, items };
}
