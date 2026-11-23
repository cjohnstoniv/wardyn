// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The Walls view's wire reasons. TestReasonDocsMatchReasonsGo and TestNoAdHocErrorBodyOrReasonLiteral read this
// file through reasonsGoFiles; docs/sdk.md's "The Walls view" table documents each value.
const (
	reasonWallsRateLimited     = "walls_rate_limited"     // too many Walls view reads from one caller (429, Retry-After)
	reasonWallsSnapshotGone    = "walls_snapshot_gone"    // a continuation page named an expired, foreign or rescoped topology snapshot (409)
	reasonWallsQueryInvalid    = "walls_query_invalid"    // a Walls view query parameter out of range or unknown (400)
	reasonWallsViewUnavailable = "walls_view_unavailable" // a held-back Walls view route, or a store that cannot read decision rows (501)
)
