// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

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
