/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A Kubernetes runner's node-loss containment on run detail: the line a
// held run shows, and the security operator's node record form. The three labels the design names
// ("containment pending; egress bounded until T", "egress bound passed; termination unknown", "node
// name reused") are kept word for word, in sentence case; the sentences under them say the same plainly.

export const CONTAINMENT = {
  LOAD_FAILED: "Couldn't read this run's containment status, so a hold on it may not be shown.",
  STALE: "Couldn't refresh this run's containment status; what is shown may be out of date.",
  RETRY: "Retry",
  pendingTitle: (until: string) => `Containment pending; egress bounded until ${until}`,
  PENDING_BODY:
    "The runner deleted this run's pods but has no proof yet that they stopped. Its proxy can no longer renew its token, so its network access ends by then at the latest.",
  PASSED_TITLE: "Egress bound passed; termination unknown",
  PASSED_BODY:
    "This run's network access has ended, but the runner still cannot confirm that its pods stopped. It keeps the run held until the node reports back or a security operator records that the machine is off.",
  NAME_REUSED: "Node name reused",
  NAME_REUSED_BODY:
    'Another machine used this run\'s node name after the run was placed. Only a "Node powered off" record made by two people can release it.',
  NO_CAUSAL: "No causal association",
  NO_CAUSAL_BODY: "The runner could not tie this run to one machine, so any record for it needs two people.",

  FORM_TITLE: "Record the node's state",
  FORM_LEAD: "Release this run only after you have checked the machines yourself.",
  WARN_DELETED_NODE: "A deleted Node object is not confirmation that a machine is off.",
  WARN_PARTITIONED: "A machine cut off from the network may still be running the run's pods.",
  MACHINES_LABEL: "Machines the runner recorded",
  markedAt: (when: string) => `Pods marked for deletion: at or before ${when}`,
  pod: (component: string, uid: string) => `${component} pod ${uid}`,
  node: (name: string) => `Node name ${name}`,
  NODE_NONE: "Never placed on a node",
  holder: (label: string, uid: string) => `${label}: ${uid}`,
  HOLDER_CAUSAL: "Machine it was placed on",
  HOLDER_POST_BIND: "Holder after placement",
  HOLDER_BEFORE: "Holder before placement",
  HOLDER_CLOSING: "Holder after deletion",
  machine: (machineId: string, systemUuid: string) => `machine ${machineId}, system ${systemUuid}`,
  REUSES_LABEL: "Name reuses the runner saw",
  reuse: (name: string, uid: string, machineId: string, systemUuid: string) =>
    `${name}: ${uid} (machine ${machineId}, system ${systemUuid})`,

  ATTEST_LABEL: "What you confirmed",
  ATTEST_POWERED_OFF: "Node powered off",
  ATTEST_POWERED_OFF_HINT:
    "Every machine that held this node name, from when the run was placed until now, is powered off.",
  ATTEST_NO_PRIOR: "No prior holder",
  ATTEST_NO_PRIOR_HINT:
    "Your platform's inventory shows that no machine other than the one above held this node name since the run was placed.",
  SOURCE_LABEL: "Where you confirmed it",
  SOURCE_PROVIDER: "Provider console",
  SOURCE_CONTROLLER: "Management controller",
  SOURCE_OTHER: "Other",
  SOURCE_ID_LABEL: "Identifier there",
  SOURCE_ID_HINT: "The instance, host or ticket id. It is kept in the audit row.",
  TWO_PEOPLE:
    "This record needs two people. After you send it, a second security operator approves it on Governance, Changes.",
  SUBMIT: "Send record",
  SUBMITTING: "Sending…",
  SENT: "Record sent to the runner.",
  HELD: "Waiting for a second person. They approve it on Governance, Changes.",
  SEND_FAILED: "Couldn't send the record",

  RECORDS_TITLE: "Node records",
  STATE_SENT: "Sent, waiting for the runner",
  STATE_ACCEPTED: "Accepted by the runner",
  stateRefused: (reason: string) => `Refused by the runner: ${reason}`,
} as const;
