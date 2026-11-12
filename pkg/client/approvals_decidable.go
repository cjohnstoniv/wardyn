// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ListApprovalsDecidable returns the PENDING approvals the caller may decide now (GET
// /api/v1/approvals?decidable_by=me), newest first: a security admin's are every pending approval the
// deployment's switches let them decide; anyone else's are those on runs they created plus those their unit's
// deciders rule names them for. A row served to a decider who is not the run's creator carries RequestedBy.
// The server returns at most 500 rows; truncated=true means more were decidable.
func (c *Client) ListApprovalsDecidable(ctx context.Context) (aps []types.ApprovalRequest, truncated bool, err error) {
	var hdr http.Header
	err = c.do(ctx, http.MethodGet, "/api/v1/approvals?decidable_by=me", nil, &aps, &hdr)
	return aps, hdr.Get("X-Wardyn-Truncated") == "true", err
}
