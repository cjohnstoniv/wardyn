/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The credential delivery policy routes behind Governance -> Runner credentials. Both are securityOps;
// the PUT is held for a second person on a deployment that requires one (202, thrown as
// PendingChangeError by asJson) and takes the GET's ETag as If-Match, so a stale draft is a 412, not an overwrite.
import type { CredentialDeliveryPolicyDoc, DeliveryPolicy } from "../types/credential-delivery";
import { asJson, wfetch } from "./core";

/** The document with the ETag the response carried, which the next PUT sends back as If-Match. */
export type CredentialDeliverySnapshot = CredentialDeliveryPolicyDoc & { etag?: string | null };

export const credentialDelivery = {
  // GET /api/v1/governance/credential-delivery. A never-written policy is an empty `classes`. The rest of the
  // policy (`by_trust`) is kept as stored: the PUT replaces the whole document, so a field dropped here would be
  // erased by the next save.
  async get(): Promise<CredentialDeliverySnapshot> {
    const res = await wfetch("/governance/credential-delivery", { method: "GET" });
    const doc = await asJson<CredentialDeliveryPolicyDoc>(res);
    return { ...doc, policy: { ...doc.policy, classes: doc.policy?.classes ?? {} }, etag: res.headers.get("ETag") };
  },

  // PUT /api/v1/governance/credential-delivery replaces the whole document: 200 -> the stored doc,
  // 202 -> PendingChangeError, 400 -> HttpError carrying the server's sentence, 412 -> the same, for a stale
  // `etag` (the last GET's or PUT's; none sends no If-Match).
  async put(policy: DeliveryPolicy, etag?: string | null): Promise<CredentialDeliverySnapshot> {
    const res = await wfetch("/governance/credential-delivery", {
      method: "PUT", asPerson: true,
      headers: etag ? { "If-Match": etag } : {},
      body: JSON.stringify(policy),
    });
    const doc = await asJson<CredentialDeliveryPolicyDoc>(res);
    return { ...doc, etag: res.headers.get("ETag") };
  },

  // GET /api/v1/runners/settings -> whether runners are on (the note says the policy applies later when off).
  async runnersEnabled(): Promise<boolean> {
    return (await asJson<{ enabled: boolean }>(await wfetch("/runners/settings", { method: "GET" }))).enabled;
  },
};
