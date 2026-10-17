// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// AuditRetention reports the audit log's effective and pending retention policy, the cutover, every
// partition with its row count, state and whether a drop would take it now (and, if not, why), and how
// many months ahead already have a partition. Security tier only.
// GET /api/v1/audit/retention.
func (c *Client) AuditRetention(ctx context.Context) (types.AuditRetentionStatus, error) {
	var out types.AuditRetentionStatus
	err := c.do(ctx, http.MethodGet, "/api/v1/audit/retention", nil, &out)
	return out, err
}

// DropAuditPartition drops the oldest closed audit partition past the retention window, through the
// database function that checks digest (the one ExportAuditPartition's footer carries, which the operator
// has verified), records a chained audit.retention.partition_dropped event and an anchor, and only then
// drops. A refusal is a 409 APIError whose Reason is one of audit_retention_not_oldest,
// audit_retention_not_closed, audit_retention_inside_window, audit_retention_live_run or
// audit_retention_digest_mismatch; an unknown partition is 404. Security tier only.
// POST /api/v1/audit/retention/drop.
func (c *Client) DropAuditPartition(ctx context.Context, partition, digest string) (types.AuditRetentionDrop, error) {
	var out types.AuditRetentionDrop
	err := c.do(ctx, http.MethodPost, "/api/v1/audit/retention/drop",
		map[string]string{"partition": partition, "digest": digest}, &out)
	return out, err
}

// ExportAuditPartition streams one closed audit partition as NDJSON: a manifest line, one line per
// row in seq order, and a footer line carrying the partition's digest. raw selects the stored,
// hash-covered form an operator can re-hash independently; otherwise the readable form. The caller
// MUST Close the returned reader, and must treat a body that ends without the footer line as an
// incomplete export. Security tier only: any other caller gets an empty body.
// GET /api/v1/audit/export?partition=<name>[&form=raw]. Returns 404/409 APIErrors for an unknown or
// still-open partition.
func (c *Client) ExportAuditPartition(ctx context.Context, partition string, raw bool) (io.ReadCloser, error) {
	q := url.Values{"partition": {partition}}
	if raw {
		q.Set("form", "raw")
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/audit/export?"+q.Encode(), nil, false)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := c.streamClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return nil, NewAPIError(resp.StatusCode, body)
	}
	return resp.Body, nil
}
