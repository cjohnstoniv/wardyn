/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A Kubernetes runner's node-loss containment as the console reads it (internal/types/run_containment.go,
// the tuple from internal/runner/runner.go). run-containment.wire.test.ts holds each interface to its Go
// struct's json tags.

export type ContainmentPendingKind = "agent_only" | "pending_delete";
export type NodeRecordAttestation = "node_powered_off" | "no_prior_holder";
export type NodeRecordSource = "provider_console" | "management_controller" | "other";
export type NodeRecordState = "sent" | "accepted" | "refused";

/** One observation of the Node holding a name. */
export interface NodeSample {
  name: string;
  uid: string;
  machine_id: string;
  system_uuid: string;
  internal_ip?: string;
}

/** One pod's part of the tuple: an absent value was never recorded. */
export interface NodeTuplePod {
  component: string;
  uid: string;
  node_name?: string;
  causal_uid?: string;
  post_bind?: NodeSample;
  before_holder_uid?: string;
  closing_holder_uid?: string;
}

/** What a node record must name: the run's pods and nodes as the runner recorded them, and every reuse it saw. */
export interface NodeTuple {
  run_id: string;
  pods: NodeTuplePod[];
  reuses?: NodeSample[];
}

/** GET /runs/{id}/containment's pending report. `tuple` is sent to security operators only. */
export interface RunContainmentPending {
  kind: ContainmentPendingKind;
  since: string;
  bound_until: string;
  bound_passed: boolean;
  name_reused: boolean;
  no_causal_association: boolean;
  two_people: boolean;
  tuple?: NodeTuple;
}

/** One node record and what the runner answered. */
export interface RunNodeRecord {
  id: string;
  attestation: NodeRecordAttestation;
  source: NodeRecordSource;
  source_id: string;
  state: NodeRecordState;
  reason?: string;
  created_at: string;
  decided_at?: string;
}

/** GET /runs/{id}/containment. */
export interface RunContainment {
  pending?: RunContainmentPending;
  records: RunNodeRecord[];
}

/** POST /runs/{id}/node-record: `tuple` is the one the form showed. */
export interface RunNodeRecordRequest {
  attestation: NodeRecordAttestation;
  source: NodeRecordSource;
  source_id: string;
  tuple: NodeTuple;
}

/** The refusals and run-ending reason this surface adds to the shared reason set (internal/api/reasons.go). */
export type RunContainmentReason =
  | "run_containment_pending"
  | "relay_ip_fenced"
  | "node_record_invalid"
  | "node_record_refused"
  | "node_record_human_only";
