// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// AuditRetentionPolicy is the persisted retention policy (audit_partition_meta). A decrease, including
// 0 -> finite, is pending until its date; days 0 is forever.
type AuditRetentionPolicy struct {
	// Days is the stored policy; EffectiveDays is what the drop function applies now (the pending value
	// once its date has passed).
	Days          int `json:"days"`
	EffectiveDays int `json:"effective_days"`
	// PendingDays and PendingEffectiveAt are set together, for a decrease that has not taken effect.
	PendingDays        *int       `json:"pending_days,omitempty"`
	PendingEffectiveAt *time.Time `json:"pending_effective_at,omitempty"`
}

// AuditRetentionPartition is one partition of the audit log as audit_retention_partitions reports it.
type AuditRetentionPartition struct {
	Name string `json:"name"`
	// Lo is absent for the legacy partition (it starts at MINVALUE).
	Lo    *time.Time `json:"lo,omitempty"`
	Hi    *time.Time `json:"hi,omitempty"`
	Rows  int64      `json:"rows"`
	State string     `json:"state"` // closed | open | future
	// Eligible is true when audit_retention_drop would take this partition now; otherwise Refusal is the
	// reason it would refuse with.
	Eligible bool   `json:"eligible"`
	Refusal  string `json:"refusal,omitempty"`
}

// AuditRetentionStatus is what GET /audit/retention reports.
type AuditRetentionStatus struct {
	Policy AuditRetentionPolicy `json:"policy"`
	// Cutover is when the log was converted to partitions; everything before it is in the legacy partition.
	Cutover    time.Time                 `json:"cutover"`
	Partitions []AuditRetentionPartition `json:"partitions"`
	// MonthsAhead is how many months past the current one already have a partition.
	MonthsAhead int `json:"months_ahead"`
}

// AuditRetentionDrop is a completed drop.
type AuditRetentionDrop struct {
	Partition string `json:"partition"`
	Rows      int64  `json:"rows"`
	SeqLo     int64  `json:"seq_lo"`
	SeqHi     int64  `json:"seq_hi"`
	Digest    string `json:"digest"`
	// EventSeq is the seq of the chained audit.retention.partition_dropped event.
	EventSeq int64 `json:"event_seq"`
}

// AuditRetentionPolicyChange is audit_retention_set_policy's answer. Outcome is applied, pending,
// cancelled or unchanged.
type AuditRetentionPolicyChange struct {
	Outcome string `json:"outcome"`
	AuditRetentionPolicy
}
