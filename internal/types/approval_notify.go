// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// ApprovalNotifyState is the lifecycle of one approval_notifications outbox row.
type ApprovalNotifyState string

const (
	NotifyPending   ApprovalNotifyState = "pending"
	NotifySending   ApprovalNotifyState = "sending"
	NotifySent      ApprovalNotifyState = "sent"
	NotifyCancelled ApprovalNotifyState = "cancelled"
	NotifyDead      ApprovalNotifyState = "dead"
)

// ApprovalNotifyStates is the closed set, compared with the approval_notifications.state CHECK by
// internal/db's closedEnumChecks.
var ApprovalNotifyStates = []ApprovalNotifyState{
	NotifyPending, NotifySending, NotifySent, NotifyCancelled, NotifyDead,
}
