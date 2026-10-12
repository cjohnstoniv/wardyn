/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The credential delivery policy routes behind Governance -> Runner credentials. Both are securityOps;
// the PUT is held for a second person on a deployment that requires one (202, thrown as
// PendingChangeError by asJson) and takes no If-Match.
import type { CredentialDeliveryPolicyDoc, DeliveryPolicy } from "../types/credential-delivery";
import { asJson, wfetch } from "./core";

export const credentialDelivery = {
  // GET /api/v1/governance/credential-delivery. A never-written policy is an empty `classes`.
  async get(): Promise<CredentialDeliveryPolicyDoc> {
    const doc = await asJson<CredentialDeliveryPolicyDoc>(await wfetch("/governance/credential-delivery", { method: "GET" }));
    return { ...doc, policy: { classes: doc.policy?.classes ?? {} } };
  },

  // PUT /api/v1/governance/credential-delivery replaces the whole document: 200 -> the stored doc,
  // 202 -> PendingChangeError, 400 -> HttpError carrying the server's sentence.
  async put(policy: DeliveryPolicy): Promise<CredentialDeliveryPolicyDoc> {
    return asJson<CredentialDeliveryPolicyDoc>(
      await wfetch("/governance/credential-delivery", { method: "PUT", body: JSON.stringify(policy) }),
    );
  },

  // GET /api/v1/runners/settings -> whether runners are on (the note says the policy applies later when off).
  async runnersEnabled(): Promise<boolean> {
    return (await asJson<{ enabled: boolean }>(await wfetch("/runners/settings", { method: "GET" }))).enabled;
  },
};
