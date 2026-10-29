/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Workloads screen, the run header's workload chip and Admin -> Runners -> Workload bindings. Every string is
// the approved mock's (r16 T3-a and T3-b), character for character; workloads.test.ts freezes them. The few
// sentences the mock does not draw are marked "not in the mock". Lazy-only like the other screen copy: never
// re-exported through copy.ts.

export const WORKLOADS = {
  NAV: "Workloads",
  TITLE: "Workloads",
  LIST_ARIA: "Workloads",
  COL_NAME: "Name",
  COL_OWNER: "Owner",
  COL_SOURCE: "Source",
  COL_RUN: "Run",
  COL_STATE: "State",
  COL_UPDATED: "Updated",
  OWNER_YOU: "You",
  NONE: "—",
  SOURCE_API: "Wardyn CLI or API",
  /** Followed by the namespace, in mono. */
  SOURCE_K8S_PREFIX: "Kubernetes: ",
  /** Followed by the command, in mono, and EMPTY_END. */
  EMPTY_LEAD: "No workloads yet. Apply one with",
  EMPTY_COMMAND: "wardyn apply -f workload.yaml",
  EMPTY_END: ".",
  // The state badges.
  RUNNING: "Running",
  UNHEALTHY: "Unhealthy",
  QUEUED: "Queued",
  ENDED: "Ended",
  REFUSED: "Refused",
  // A custom resource's status in its cluster: a namespace nothing binds stores no workload, so no row wears it.
  NOT_BOUND: "Not bound",
  BINDING_INACTIVE: "Binding inactive",
  // The detail drawer.
  GENERATION: "Generation",
  CURRENT_RUN: "Current run",
  STARTED: (ago: string) => `started ${ago}`,
  LAST_REFUSAL: "Last refusal",
  /** "Generation 3 · <sentence> · the previous run kept running." The code follows in small print. */
  REFUSAL_GENERATION: (n: number) => `Generation ${n}`,
  PREVIOUS_KEPT: "the previous run kept running.",
  SPEC: "Spec",
  RESTART: "Restart",
  DELETE: "Delete",
  /** Followed by the namespace, in mono, and MANAGED_TAIL. */
  MANAGED_LEAD: "Managed by Kubernetes namespace",
  MANAGED_TAIL: ". Change it in Git.",
  DELETE_HIDDEN: "Delete is hidden: this workload is managed by Kubernetes.",
  // Not in the mock: the toasts and the delete confirmation around the two actions.
  RESTARTED: (name: string) => `${name} restarted.`,
  DELETED: (name: string) => `${name} deleted.`,
  DELETE_TITLE: (name: string) => `Delete ${name}?`,
  DELETE_BODY: "Its run ends and the workload is removed.",
} as const;

/** The run header's chip (r16 T3-b callout 1): neutral, linking to the workload's drawer. */
export const WORKLOAD_CHIP = (name: string) => `Workload: ${name}`;

export const WORKLOAD_BINDINGS = {
  TAB: "Workload bindings",
  TABS_LABEL: "Workload bindings",
  BIND: "Bind namespace",
  COL_POOL: "Pool",
  COL_NAMESPACE: "Namespace",
  COL_SERVICE: "Service identity",
  COL_SPONSOR: "Sponsor",
  COL_CREATED: "Created",
  DELETE: "Delete",
  // The Bind namespace dialog.
  DIALOG_TITLE: "Bind namespace",
  POOL: "Pool",
  NAMESPACE: "Namespace",
  SERVICE: "Service identity",
  HELP: "Anyone who can create a WardynWorkload in this namespace will launch runs as this service identity, in this pool.",
  CANCEL: "Cancel",
  CONFIRM: "Bind namespace",
  // The delete dialog. The title is the mock's own (proposed there); the body is the design's.
  DELETE_TITLE: (namespace: string) => `Delete the binding for ${namespace}?`,
  DELETE_BODY: "Runs already started keep running.",
  DELETE_CONFIRM: "Delete",
  // Not in the mock.
  EMPTY: "No namespaces are bound yet.",
  POOL_PLACEHOLDER: "Choose a pool",
  SERVICE_PLACEHOLDER: "Choose a service identity",
  NO_POOLS: "There is no organization pool to bind a namespace to.",
  BOUND: (namespace: string) => `${namespace} bound.`,
  UNBOUND: (namespace: string) => `The binding for ${namespace} was deleted.`,
} as const;

const plural = (n: number, unit: string) => `${n} ${unit}${n === 1 ? "" : "s"}`;

/** The list's and the drawer's stamp: "2 min ago", "1 h ago", "yesterday", "9 d ago". */
export function agoShort(iso: string, now: number): string {
  const min = Math.max(0, Math.round((now - Date.parse(iso)) / 60_000));
  if (min < 1) return "just now";
  if (min < 60) return `${min} min ago`;
  const h = Math.round(min / 60);
  if (h < 24) return `${h} h ago`;
  const d = Math.round(h / 24);
  return d === 1 ? "yesterday" : `${d} d ago`;
}

/** The bindings table's stamp: "12 days ago", "2 months ago". */
export function agoLong(iso: string, now: number): string {
  const min = Math.max(0, Math.round((now - Date.parse(iso)) / 60_000));
  if (min < 1) return "just now";
  if (min < 60) return `${plural(min, "minute")} ago`;
  const h = Math.round(min / 60);
  if (h < 24) return `${plural(h, "hour")} ago`;
  const d = Math.round(h / 24);
  return d < 45 ? `${plural(d, "day")} ago` : `${plural(Math.round(d / 30), "month")} ago`;
}
