// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The receipt, checkpoint and key routes' wire: the query names the server reads, the field names it writes
// and the pending refusal's retry header.
func TestAuditReceipt_WireNames(t *testing.T) {
	ev1, ev2, run := uuid.New(), uuid.New(), uuid.New()
	var gotPath string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query()
		switch r.URL.Path {
		case "/api/v1/audit/receipt":
			if r.URL.Query().Get("run_id") == "" && len(r.URL.Query()["event"]) == 0 {
				t.Errorf("a receipt request names neither event nor run_id")
			}
			// As the API does: after_seq and limit page a run's receipt only.
			if r.URL.Query().Get("run_id") == "" && (r.URL.Query().Has("after_seq") || r.URL.Query().Has("limit")) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"after_seq and limit page a run's receipt: use them with run_id","reason":"audit_receipt_request_invalid"}`))
				return
			}
			if r.URL.Query().Get("event") == ev1.String() && r.URL.Query()["event"][1] == uuid.Nil.String() {
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"newer than the last checkpoint","reason":"audit_receipt_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"format":"wardyn-audit-receipt/2","next_after_seq":812,"checkpoints":[{"statement":"{}","signature":"AAEC",` +
				`"seqs":[5,9],"row_hashes":["aa","bb"],"events":[{"index":1,"row":{"seq":9,"id":"` + ev2.String() + `","time_us":1,"run_id":null,` +
				`"actor_type":"system","actor":"wardynd","action":"run.start","target":"t","outcome":"success","source_ip":"","data":"{}",` +
				`"prev_hash":"aa","row_hash":"bb"},"redacted_fields":["workspace_mounts"]}]}]}`))
		case "/api/v1/audit/checkpoints":
			_, _ = w.Write([]byte(`{"format":"wardyn-audit-checkpoints/1","next_after_seq":40,"checkpoints":[{"statement":"{}","signature":"AAEC"}]}`))
		case "/api/v1/audit/receipt-keys":
			_, _ = w.Write([]byte(`{"format":"wardyn-audit-keys/1","keys":[{"kid":"k1","public_key":"AAEC","public_key_pem":"pem","fingerprint":"f","created_at":"2026-11-07T10:00:00Z","retired":true}]}`))
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := &client.Client{BaseURL: srv.URL, Token: "t"}
	ctx := context.Background()

	// Events: one event parameter each, and no paging.
	b, _, err := c.AuditReceipt(ctx, client.AuditReceiptQuery{EventIDs: []uuid.UUID{ev1, ev2}})
	if err != nil {
		t.Fatalf("AuditReceipt: %v", err)
	}
	if gotPath != "/api/v1/audit/receipt" || len(gotQuery["event"]) != 2 || gotQuery.Has("after_seq") || gotQuery.Has("limit") || gotQuery.Get("run_id") != "" {
		t.Errorf("request = %s ? %v", gotPath, gotQuery)
	}
	if b.Format != "wardyn-audit-receipt/2" || b.NextAfterSeq != 812 || len(b.Checkpoints) != 1 {
		t.Fatalf("bundle = %+v", b)
	}
	cp := b.Checkpoints[0]
	if string(cp.Signature) != "\x00\x01\x02" || len(cp.Seqs) != 2 || cp.Seqs[1] != 9 || len(cp.RowHashes) != 2 || len(cp.Events) != 1 {
		t.Fatalf("checkpoint = %+v", cp)
	}
	if e := cp.Events[0]; e.Index != 1 || e.Row.Seq != 9 || e.Row.ID != ev2 || e.Row.Action != "run.start" || e.Row.Data == nil || *e.Row.RowHash != "bb" || len(e.RedactedFields) != 1 {
		t.Fatalf("event = %+v", e)
	}

	if _, _, err := c.AuditReceipt(ctx, client.AuditReceiptQuery{RunID: run}); err != nil {
		t.Fatalf("AuditReceipt by run: %v", err)
	}
	if gotQuery.Get("run_id") != run.String() || len(gotQuery["event"]) != 0 || gotQuery.Get("after_seq") != "" {
		t.Errorf("run request query = %v", gotQuery)
	}

	// A run's receipt pages: after_seq and limit named as the server reads them.
	if _, _, err := c.AuditReceipt(ctx, client.AuditReceiptQuery{RunID: run, AfterSeq: 4, Limit: 7}); err != nil {
		t.Fatalf("AuditReceipt by run, paged: %v", err)
	}
	if gotQuery.Get("run_id") != run.String() || len(gotQuery["event"]) != 0 || gotQuery.Get("after_seq") != "4" || gotQuery.Get("limit") != "7" {
		t.Errorf("paged run request query = %v", gotQuery)
	}

	// Events with paging: the server refuses the pair, and the APIError names the reason.
	for _, q := range []client.AuditReceiptQuery{{EventIDs: []uuid.UUID{ev2}, AfterSeq: 4}, {EventIDs: []uuid.UUID{ev2}, Limit: 7}} {
		_, _, err := c.AuditReceipt(ctx, q)
		var refused *client.APIError
		if !errors.As(err, &refused) || refused.Status != http.StatusBadRequest || refused.Reason != "audit_receipt_request_invalid" {
			t.Errorf("events with paging %+v: err = %v; want a 400 audit_receipt_request_invalid", q, err)
		}
	}

	// A request that names both, or neither, is refused before it is sent.
	if _, _, err := c.AuditReceipt(ctx, client.AuditReceiptQuery{}); err == nil {
		t.Errorf("a receipt request naming nothing was sent")
	}
	if _, _, err := c.AuditReceipt(ctx, client.AuditReceiptQuery{RunID: run, EventIDs: []uuid.UUID{ev1}}); err == nil {
		t.Errorf("a receipt request naming an event and a run was sent")
	}

	// Pending: the APIError names the reason, and the Retry-After header comes back with it.
	_, h, err := c.AuditReceipt(ctx, client.AuditReceiptQuery{EventIDs: []uuid.UUID{ev1, uuid.Nil}})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Reason != "audit_receipt_pending" || h.Get("Retry-After") != "60" {
		t.Errorf("pending: err = %v, Retry-After %q; want a 409 audit_receipt_pending with the header", err, h.Get("Retry-After"))
	}

	page, err := c.AuditCheckpoints(ctx, 12, 500)
	if err != nil {
		t.Fatalf("AuditCheckpoints: %v", err)
	}
	if gotQuery.Get("after_seq") != "12" || gotQuery.Get("limit") != "500" || page.Format != "wardyn-audit-checkpoints/1" || page.NextAfterSeq != 40 || len(page.Checkpoints) != 1 {
		t.Errorf("checkpoints page = %+v, query %v", page, gotQuery)
	}

	keys, err := c.AuditReceiptKeys(ctx)
	if err != nil {
		t.Fatalf("AuditReceiptKeys: %v", err)
	}
	if keys.Format != "wardyn-audit-keys/1" || len(keys.Keys) != 1 || keys.Keys[0].Kid != "k1" || !keys.Keys[0].Retired || len(keys.Keys[0].PublicKey) != 3 || keys.Keys[0].PublicKeyPEM != "pem" {
		t.Errorf("keys = %+v", keys)
	}
}
