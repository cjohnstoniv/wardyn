// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
)

// FitChecker is an OPTIONAL Runner capability: ask the substrate whether one run of this size
// can fit the room it is allowed, before anything is created. Only the Kubernetes substrate
// has such a limit (the runs namespace's ResourceQuota objects, and the nodes a placement
// allows); a substrate without one does not implement it and is skipped.
//
// It reads and reserves nothing: the scheduler and the quota admission stay authoritative,
// and two concurrent callers both see the same room.
type FitChecker interface {
	CheckFit(ctx context.Context, res Resources) (Fit, error)
}

// ErrFitUnsupported is FitChecker's answer from a router with no substrate that has a fit to check.
var ErrFitUnsupported = errors.New("runner: no substrate checks whether a run fits")

// ReadState is how one read of the cluster ended. Empty, forbidden and unavailable are three
// different answers with three different remedies, so none may stand in for another.
type ReadState string

const (
	// ReadOK: the list was read. An empty list is still ReadOK, with nothing in it.
	ReadOK ReadState = "ok"
	// ReadForbidden: the apiserver refused the list (RBAC).
	ReadForbidden ReadState = "forbidden"
	// ReadUnavailable: the list failed for any other reason.
	ReadUnavailable ReadState = "unavailable"
	// ReadSkipped: the read is switched off (the node read, which is opt-in).
	ReadSkipped ReadState = "skipped"
)

// Fit is a FitChecker's answer for one run.
type Fit struct {
	// Quotas is the ResourceQuota list's read state; Quota holds the quotas that apply to at
	// least one of the run's two pods, by name.
	Quotas ReadState
	Quota  []QuotaFit
	// Nodes is the node list's read state; NodeShortfall is set only when Nodes is ReadOK and
	// some pod of the run is larger than every node its placement allows.
	Nodes         ReadState
	NodeShortfall *NodeShortfall
}

// QuotaFit is one applicable quota and what the run asks of each quantity it limits.
type QuotaFit struct {
	Name string
	Axes []QuotaAxis
}

// QuotaAxis is one hard limit of a quota that the run touches: the quota's own key
// ("requests.cpu", "limits.memory", "pods", ...), what both pods of the run add to it, what is
// left under it now, and its hard value. CPU is in millicores, memory and storage in bytes,
// pods in a count. Left is read from the quota's status, which lags the cluster by the quota
// controller's resync, and is floored at zero.
type QuotaAxis struct {
	Key  string
	Need int64
	Left int64
	Hard int64
}

// NodeShortfall is the largest pod request no allowed node can hold: a pod's CPU in
// millicores and memory in bytes.
type NodeShortfall struct {
	CPUMillis   int64
	MemoryBytes int64
}
