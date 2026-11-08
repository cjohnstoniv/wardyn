// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auditreceipt"
)

// AuditBundle is a signed-receipt or checkpoint-export page: the bundle `wardyn audit verify` reads, plus
// where to continue. NextAfterSeq is set only when the answer ended before the data did; pass it as AfterSeq
// to fetch the next page.
type AuditBundle struct {
	auditreceipt.Bundle
	NextAfterSeq int64 `json:"next_after_seq,omitempty"`
}

// AuditReceiptQuery names what a receipt is for: the given events, or every event of RunID (from AfterSeq,
// at most Limit of them, the server's cap when 0). Exactly one of EventIDs and RunID is set; AfterSeq and
// Limit page a run's receipt and belong with RunID.
type AuditReceiptQuery struct {
	EventIDs []uuid.UUID
	RunID    uuid.UUID
	AfterSeq int64
	Limit    int
}

// AuditReceipt fetches a signed receipt that the events are in the audit log unaltered. Security tier only.
// GET /api/v1/audit/receipt. A refusal is an *APIError whose Reason is audit_receipt_pending (409: newer than
// the last signed checkpoint; the returned header's Retry-After says when to ask again, and the header is
// returned with an error too), audit_receipt_unchained (409: in no checkpoint, ever), audit_receipt_too_large
// (413: ask for fewer events), audit_receipt_not_found (404) or audit_receipt_signing_off (503).
func (c *Client) AuditReceipt(ctx context.Context, q AuditReceiptQuery) (AuditBundle, http.Header, error) {
	byRun := q.RunID != uuid.Nil
	if byRun == (len(q.EventIDs) > 0) {
		return AuditBundle{}, nil, errors.New("client: a receipt is for events or for one run, not both and not neither")
	}
	v := url.Values{}
	for _, id := range q.EventIDs {
		v.Add("event", id.String())
	}
	if byRun {
		v.Set("run_id", q.RunID.String())
	}
	if q.AfterSeq > 0 {
		v.Set("after_seq", strconv.FormatInt(q.AfterSeq, 10))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	var out AuditBundle
	var h http.Header
	err := c.do(ctx, http.MethodGet, "/api/v1/audit/receipt?"+v.Encode(), nil, &out, &h)
	return out, h, err
}

// AuditCheckpoints fetches signed checkpoints covering rows above afterSeq, oldest first, at most limit (the
// server's cap when 0): the signatures and links, with no row and no inclusion proof. Security tier only.
// GET /api/v1/audit/checkpoints.
func (c *Client) AuditCheckpoints(ctx context.Context, afterSeq int64, limit int) (AuditBundle, error) {
	v := url.Values{}
	if afterSeq > 0 {
		v.Set("after_seq", strconv.FormatInt(afterSeq, 10))
	}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	var out AuditBundle
	err := c.do(ctx, http.MethodGet, "/api/v1/audit/checkpoints?"+v.Encode(), nil, &out)
	return out, err
}

// AuditReceiptKeys fetches the public keys that sign audit checkpoints, current and previous, with the
// retired ones marked. Save the file where the control plane cannot change it: a receipt is only as good as
// the keys it is checked against. Security tier only. GET /api/v1/audit/receipt-keys.
func (c *Client) AuditReceiptKeys(ctx context.Context) (auditreceipt.Keys, error) {
	var out auditreceipt.Keys
	err := c.do(ctx, http.MethodGet, "/api/v1/audit/receipt-keys", nil, &out)
	return out, err
}
