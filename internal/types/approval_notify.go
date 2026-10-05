// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "time"

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

// ApprovalEscalation is one pending approval's place on its notification schedule: the highest tier
// whose due time has passed, and the earliest due time still ahead (nil when no later tier exists).
type ApprovalEscalation struct {
	Tier   int16
	NextAt *time.Time
}

// ApprovalNotifyChannelStat is one channel's delivery history read from the outbox: when a send last
// succeeded, the class and time of the last failed attempt, and how many rows went dead in the last
// hour. LastError is a failure class such as "http_status:503", never text a server or URL wrote.
type ApprovalNotifyChannelStat struct {
	Channel        string
	LastSentAt     *time.Time
	LastError      string
	LastErrorAt    *time.Time
	FailedLastHour int
}
