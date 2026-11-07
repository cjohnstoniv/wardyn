// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auditreceipt"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// receiptFake is a store holding a chain of audit rows and the signed checkpoints over them.
type receiptFake struct {
	pagerFake
	rows  []store.AuditPartitionRow
	cps   []store.AuditCheckpoint
	lists map[int64][]store.CheckpointRow // by checkpoint seq_lo
	reads atomic.Int32
}

func (f *receiptFake) AuditChainHead(context.Context) (int64, string, error) { return 0, "", nil }
func (f *receiptFake) AuditCheckpointRange(context.Context, int64, string, int) ([]store.CheckpointRow, int64, error) {
	return nil, 0, nil
}
func (f *receiptFake) InsertAuditCheckpoint(context.Context, store.AuditCheckpoint) (store.AuditCheckpoint, error) {
	return store.AuditCheckpoint{}, nil
}
func (f *receiptFake) LatestAuditCheckpoint(context.Context) (store.AuditCheckpoint, bool, error) {
	return store.AuditCheckpoint{}, false, nil
}
func (f *receiptFake) AuditCheckpoints(_ context.Context, after int64, limit int) ([]store.AuditCheckpoint, error) {
	f.reads.Add(1)
	var out []store.AuditCheckpoint
	for _, c := range f.cps {
		if c.SeqHi > after && len(out) < limit {
			out = append(out, c)
		}
	}
	return out, nil
}
func (f *receiptFake) AuditCheckpointRowHashes(_ context.Context, lo, _ int64) ([]store.CheckpointRow, error) {
	f.reads.Add(1)
	return f.lists[lo], nil
}
func (f *receiptFake) AuditReceiptRows(_ context.Context, ids []uuid.UUID, runID *uuid.UUID, after int64, limit int) ([]store.AuditPartitionRow, error) {
	f.reads.Add(1)
	var out []store.AuditPartitionRow
	for _, r := range f.rows {
		match := false
		for _, id := range ids {
			match = match || r.ID == id
		}
		if len(ids) == 0 {
			match = runID != nil && r.RunID != nil && *r.RunID == *runID
		}
		if match && r.Seq > after && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

// receiptWorld is nine chained rows in three signed checkpoints (1-3, 4-6, 7-9), run rows at seqs 2, 5 and 8;
// seq 5 is a run.policy.resolve whose workspace mount source is host detail.
type receiptWorld struct {
	fake *receiptFake
	pub  auditreceipt.PublicKey
	run  uuid.UUID
	ids  []uuid.UUID // by seq-1
}

func newReceiptWorld(t *testing.T, lastCheckpoint int) *receiptWorld {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{5}, ed25519.SeedSize))
	pk := priv.Public().(ed25519.PublicKey)
	w := &receiptWorld{
		fake: &receiptFake{lists: map[int64][]store.CheckpointRow{}},
		pub:  auditreceipt.PublicKey{Kid: auditreceipt.KeyID(pk), PublicKey: pk, CreatedAt: time.Unix(0, 0).UTC()},
		run:  uuid.New(),
	}
	str := func(s string) *string { return &s }
	var prev, prevStatement string
	var batch []auditreceipt.Row
	flush := func() {
		if len(batch) == 0 {
			return
		}
		seqs, hashes := make([]int64, len(batch)), make([]string, len(batch))
		list := make([]store.CheckpointRow, len(batch))
		for i, r := range batch {
			seqs[i], hashes[i], list[i] = r.Seq, *r.RowHash, store.CheckpointRow{Seq: r.Seq, RowHash: *r.RowHash}
		}
		chain := ""
		if prevStatement != "" {
			chain = auditreceipt.StatementHash(prevStatement)
		}
		st := auditreceipt.Statement{Format: auditreceipt.CheckpointFormat, Kid: w.pub.Kid, SeqLo: seqs[0], SeqHi: seqs[len(seqs)-1],
			Rows: int64(len(batch)), HeadHash: hashes[len(hashes)-1], Digest: auditreceipt.Fold(chain, hashes),
			SeqDigest: auditreceipt.SeqDigest(seqs, hashes), Prev: chain, SignedAt: "2026-11-07T10:00:00Z"}
		text := string(st.Bytes())
		w.fake.cps = append(w.fake.cps, store.AuditCheckpoint{SeqLo: st.SeqLo, SeqHi: st.SeqHi, Kid: st.Kid, Statement: text,
			Signature: ed25519.Sign(priv, []byte(text))})
		w.fake.lists[st.SeqLo] = list
		prevStatement, batch = text, nil
	}
	for seq := int64(1); seq <= 9; seq++ {
		r := auditreceipt.Row{Seq: seq, ID: uuid.New(), TimeUS: 1700000000000000 + seq, ActorType: "system", Actor: "wardynd",
			Action: "egress.allow", Target: str("t"), Outcome: "success", SourceIP: str(""), Data: str(fmt.Sprintf(`{"n": %d}`, seq))}
		if seq == 2 || seq == 5 || seq == 8 {
			run := w.run
			r.RunID = &run
		}
		if seq == 5 {
			r.Action = "run.policy.resolve"
			r.Data = str(`{"allowed_domains": [], "workspace_mounts": [{"source": "/home/alice/src", "target": "/work"}]}`)
		}
		if prev != "" {
			r.PrevHash = str(prev)
		}
		r.RowHash = str(auditreceipt.RowHash(r))
		prev = *r.RowHash
		w.ids = append(w.ids, r.ID)
		w.fake.rows = append(w.fake.rows, store.AuditPartitionRow{Seq: r.Seq, RecordedUS: 1, TimeUS: r.TimeUS, ID: r.ID, RunID: r.RunID,
			ActorType: r.ActorType, Actor: r.Actor, Action: r.Action, Target: *r.Target, Outcome: r.Outcome, SourceIP: *r.SourceIP,
			Data: r.Data, PrevHash: r.PrevHash, RowHash: r.RowHash, FoldHash: *r.RowHash})
		batch = append(batch, r)
		if seq%3 == 0 && seq/3 <= int64(lastCheckpoint) {
			flush()
		}
	}
	return w
}

func (w *receiptWorld) server(t *testing.T) (*Server, *harness) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, w.fake)
	cfg.OIDC = &oidc.Authenticator{}
	// Both fields live in Config's embedded AuditReceiptConfig (the interval is the one field this lane expects added to it).
	cfg.AuditReceiptKeys = func() []auditreceipt.PublicKey { return []auditreceipt.PublicKey{w.pub} }
	cfg.AuditCheckpointInterval = 30 * time.Second
	return New(cfg), h
}

func (w *receiptWorld) keys() auditreceipt.Keys {
	return auditreceipt.Keys{Format: auditreceipt.KeysFormat, Keys: []auditreceipt.PublicKey{w.pub}}
}

type receiptBody struct {
	auditreceipt.Bundle
	NextAfterSeq int64 `json:"next_after_seq"`
}

func decodeReceipt(t *testing.T, rec *httptest.ResponseRecorder) receiptBody {
	t.Helper()
	var b receiptBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode receipt: %v (%s)", err, rec.Body.String())
	}
	return b
}

func eventQuery(ids ...uuid.UUID) string {
	v := url.Values{}
	for _, id := range ids {
		v.Add("event", id.String())
	}
	return "/api/v1/audit/receipt?" + v.Encode()
}

// Anyone below the security tier is refused all three routes, a run's receipt included, and nothing is read.
func TestAuditReceipt_MemberRefused(t *testing.T) {
	w := newReceiptWorld(t, 3)
	srv, _ := w.server(t)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	for _, p := range []string{
		eventQuery(w.ids[1]),
		"/api/v1/audit/receipt?run_id=" + w.run.String(),
		"/api/v1/audit/checkpoints",
		"/api/v1/audit/receipt-keys",
	} {
		if rec := doSSO(t, srv, http.MethodGet, p, member, ""); rec.Code != http.StatusForbidden {
			t.Errorf("member GET %s = %d %s, want 403", p, rec.Code, rec.Body.String())
		}
	}
	if n := w.fake.reads.Load(); n != 0 {
		t.Errorf("the store was read %d time(s) on behalf of a member, want 0", n)
	}
	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	if rec := doSSO(t, srv, http.MethodGet, eventQuery(w.ids[1]), sec, ""); rec.Code != http.StatusOK {
		t.Errorf("security_admin GET receipt = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// With signing off (no key) the three routes answer 503 and say so.
func TestAuditReceipt_SigningOff503(t *testing.T) {
	w := newReceiptWorld(t, 3)
	h := newHarness(t)
	cfg := baseTestConfig(h, w.fake)
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)
	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	for _, p := range []string{eventQuery(w.ids[1]), "/api/v1/audit/checkpoints", "/api/v1/audit/receipt-keys"} {
		rec := doSSO(t, srv, http.MethodGet, p, sec, "")
		if rec.Code != http.StatusServiceUnavailable || respReason(rec) != "audit_receipt_signing_off" {
			t.Errorf("GET %s with signing off = %d %s, want 503 audit_receipt_signing_off", p, rec.Code, rec.Body.String())
		}
	}
}

// An event newer than the last checkpoint is not receiptable yet: 409 with Retry-After, never a partial answer.
func TestAuditReceipt_PendingUntilCheckpoint409(t *testing.T) {
	w := newReceiptWorld(t, 2) // rows 7-9 are not covered yet
	srv, _ := w.server(t)
	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	rec := doSSO(t, srv, http.MethodGet, eventQuery(w.ids[1], w.ids[7]), sec, "")
	if rec.Code != http.StatusConflict || respReason(rec) != "audit_receipt_pending" || rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("a row past the last checkpoint = %d %s Retry-After %q, want 409 audit_receipt_pending with Retry-After 30 (the interval)", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
	}
	if rec := doSSO(t, srv, http.MethodGet, eventQuery(w.ids[1]), sec, ""); rec.Code != http.StatusOK {
		t.Fatalf("a covered row = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// A hashless row, and one older than the first checkpoint, are in no checkpoint and never will be.
func TestAuditReceipt_UnchainedRow409(t *testing.T) {
	w := newReceiptWorld(t, 3)
	w.fake.rows[3].RowHash, w.fake.rows[3].PrevHash = nil, nil // seq 4, hashless
	w.fake.cps = w.fake.cps[1:]                                // signing began at seq 4: seq 1 is older than the first checkpoint
	srv, _ := w.server(t)
	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	for name, id := range map[string]uuid.UUID{"hashless": w.ids[3], "before signing began": w.ids[0]} {
		rec := doSSO(t, srv, http.MethodGet, eventQuery(id), sec, "")
		if rec.Code != http.StatusConflict || respReason(rec) != "audit_receipt_unchained" {
			t.Errorf("%s: %d %s, want 409 audit_receipt_unchained", name, rec.Code, rec.Body.String())
		}
	}
	if rec := doSSO(t, srv, http.MethodGet, eventQuery(uuid.New()), sec, ""); rec.Code != http.StatusNotFound || respReason(rec) != "audit_receipt_not_found" {
		t.Errorf("unknown event = %d %s, want 404 audit_receipt_not_found", rec.Code, rec.Body.String())
	}
}

// The receipt a super admin gets verifies whole. The one a security admin gets has the resolve row's mount
// source withheld and marked: verify links it into the signed list and reports it withheld, never verified.
func TestAuditReceipt_SecurityAdminHostRedactedMarked(t *testing.T) {
	w := newReceiptWorld(t, 3)
	srv, _ := w.server(t)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	rec := doSSO(t, srv, http.MethodGet, eventQuery(w.ids[1], w.ids[4]), admin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin receipt = %d %s", rec.Code, rec.Body.String())
	}
	if rep := auditreceipt.VerifyBundle(decodeReceipt(t, rec).Bundle, w.keys()); !rep.Complete() {
		t.Fatalf("super admin receipt: failures %+v, withheld %d; want complete", rep.Failures, rep.Withheld())
	}

	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	rec = doSSO(t, srv, http.MethodGet, eventQuery(w.ids[1], w.ids[4]), sec, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("security_admin receipt = %d %s", rec.Code, rec.Body.String())
	}
	b := decodeReceipt(t, rec)
	if bytes.Contains(rec.Body.Bytes(), []byte("/home/alice")) {
		t.Fatalf("a security_admin's receipt carries a host path: %s", rec.Body.String())
	}
	var marked int
	for _, cp := range b.Checkpoints {
		for _, ev := range cp.Events {
			if len(ev.RedactedFields) > 0 {
				marked++
				if ev.Row.Action != "run.policy.resolve" || ev.RedactedFields[0] != "workspace_mounts.source" {
					t.Errorf("redacted event = %s %v", ev.Row.Action, ev.RedactedFields)
				}
			}
		}
	}
	rep := auditreceipt.VerifyBundle(b.Bundle, w.keys())
	if marked != 1 || !rep.OK() || rep.Complete() || rep.Withheld() != 1 {
		t.Fatalf("security_admin receipt: %d marked, failures %+v, withheld %d; want one withheld event and nothing failed", marked, rep.Failures, rep.Withheld())
	}
}

// 1,001 events is past the cap before anything is read; so are events spread over more checkpoints than a
// receipt carries.
func TestAuditReceipt_CapsReturn413(t *testing.T) {
	w := newReceiptWorld(t, 3)
	srv, _ := w.server(t)
	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	many := make([]uuid.UUID, auditreceipt.MaxReceiptEvents+1)
	for i := range many {
		many[i] = uuid.New()
	}
	rec := doSSO(t, srv, http.MethodGet, eventQuery(many...), sec, "")
	if rec.Code != http.StatusRequestEntityTooLarge || respReason(rec) != "audit_receipt_too_large" {
		t.Fatalf("%d events = %d %s, want 413 audit_receipt_too_large", len(many), rec.Code, rec.Body.String())
	}
	if n := w.fake.reads.Load(); n != 0 {
		t.Errorf("an oversize request read the store %d time(s), want 0", n)
	}
	for _, p := range []string{"/api/v1/audit/receipt", "/api/v1/audit/receipt?run_id=" + w.run.String() + "&event=" + w.ids[0].String(),
		"/api/v1/audit/receipt?event=nope", "/api/v1/audit/receipt?run_id=" + w.run.String() + "&limit=1001",
		eventQuery(w.ids[0]) + "&after_seq=1"} {
		if rec := doSSO(t, srv, http.MethodGet, p, sec, ""); rec.Code != http.StatusBadRequest || respReason(rec) != "audit_receipt_request_invalid" {
			t.Errorf("GET %s = %d %s, want 400 audit_receipt_request_invalid", p, rec.Code, rec.Body.String())
		}
	}
}

// Events in the first and last checkpoints: the middle one comes as a signed header so the links verify, and
// the verifier says it checked its signature but not its inclusion.
func TestAuditReceipt_SparseRunReceiptCarriesTheCheckpointInBetween(t *testing.T) {
	w := newReceiptWorld(t, 3)
	srv, _ := w.server(t)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	rec := doSSO(t, srv, http.MethodGet, eventQuery(w.ids[1], w.ids[7]), admin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("receipt = %d %s", rec.Code, rec.Body.String())
	}
	b := decodeReceipt(t, rec)
	if len(b.Checkpoints) != 3 || len(b.Checkpoints[1].Events) != 0 || len(b.Checkpoints[1].RowHashes) != 0 {
		t.Fatalf("receipt has %d checkpoints; want 3 with the middle a header only", len(b.Checkpoints))
	}
	rep := auditreceipt.VerifyBundle(b.Bundle, w.keys())
	if !rep.Complete() || rep.Checkpoints[1].Inclusion || !rep.Checkpoints[1].Signed {
		t.Fatalf("failures %+v, checkpoints %+v; want complete with the middle signed and not inclusion-checked", rep.Failures, rep.Checkpoints)
	}
}

// A run's receipt pages: limit 2 over three run rows ends at the second and names where to continue.
func TestAuditReceipt_RunReceiptPagesByAfterSeq(t *testing.T) {
	w := newReceiptWorld(t, 3)
	srv, _ := w.server(t)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	rec := doSSO(t, srv, http.MethodGet, "/api/v1/audit/receipt?run_id="+w.run.String()+"&limit=2", admin, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("page 1 = %d %s", rec.Code, rec.Body.String())
	}
	b := decodeReceipt(t, rec)
	if b.NextAfterSeq != 5 {
		t.Fatalf("page 1 next_after_seq = %d, want 5 (the second run row)", b.NextAfterSeq)
	}
	if rep := auditreceipt.VerifyBundle(b.Bundle, w.keys()); !rep.Complete() || len(rep.Events) != 2 {
		t.Fatalf("page 1: failures %+v, %d events", rep.Failures, len(rep.Events))
	}
	rec = doSSO(t, srv, http.MethodGet, "/api/v1/audit/receipt?run_id="+w.run.String()+"&after_seq="+strconv.FormatInt(b.NextAfterSeq, 10), admin, "")
	b2 := decodeReceipt(t, rec)
	if rec.Code != http.StatusOK || b2.NextAfterSeq != 0 {
		t.Fatalf("page 2 = %d next_after_seq %d (%s), want the last page", rec.Code, b2.NextAfterSeq, rec.Body.String())
	}
	if rep := auditreceipt.VerifyBundle(b2.Bundle, w.keys()); !rep.Complete() || len(rep.Events) != 1 || rep.Events[0].Seq != 8 {
		t.Fatalf("page 2: failures %+v, events %+v; want seq 8 alone", rep.Failures, rep.Events)
	}
}

// One audit.receipt.create row per receipt, naming what was receipted and none for a refusal.
func TestAuditReceipt_RecordsReceiptCreate(t *testing.T) {
	w := newReceiptWorld(t, 2)
	srv, h := w.server(t)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	count := func() (n int) {
		for _, ev := range h.audit.snapshot() {
			if ev.Action == "audit.receipt.create" {
				n++
			}
		}
		return n
	}
	if rec := doSSO(t, srv, http.MethodGet, eventQuery(w.ids[8]), admin, ""); rec.Code != http.StatusConflict || count() != 0 {
		t.Fatalf("a pending receipt: %d, %d audit rows; want 409 and none", rec.Code, count())
	}
	if rec := doSSO(t, srv, http.MethodGet, eventQuery(w.ids[1], w.ids[2]), admin, ""); rec.Code != http.StatusOK {
		t.Fatalf("receipt = %d %s", rec.Code, rec.Body.String())
	}
	var got *struct {
		target string
		data   map[string]any
	}
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "audit.receipt.create" {
			var d map[string]any
			_ = json.Unmarshal(ev.Data, &d)
			got = &struct {
				target string
				data   map[string]any
			}{ev.Target, d}
		}
	}
	if count() != 1 || got == nil || got.target != "events:2" || got.data["events"] != float64(2) || got.data["checkpoints"] != float64(1) {
		t.Fatalf("audit rows %d, got %+v; want one audit.receipt.create naming 2 events in 1 checkpoint", count(), got)
	}
}

// The checkpoint export is signatures and links only, paged by after_seq; the keys route serves what verify pins.
func TestAuditReceipt_CheckpointsAndKeys(t *testing.T) {
	w := newReceiptWorld(t, 3)
	srv, _ := w.server(t)
	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	rec := doSSO(t, srv, http.MethodGet, "/api/v1/audit/checkpoints?limit=2", sec, "")
	b := decodeReceipt(t, rec)
	if rec.Code != http.StatusOK || b.Format != auditreceipt.CheckpointsFormat || len(b.Checkpoints) != 2 || b.NextAfterSeq != 6 {
		t.Fatalf("page 1 = %d %+v next %d", rec.Code, b.Bundle, b.NextAfterSeq)
	}
	rec = doSSO(t, srv, http.MethodGet, "/api/v1/audit/checkpoints?after_seq=6", sec, "")
	b2 := decodeReceipt(t, rec)
	if len(b2.Checkpoints) != 1 || b2.NextAfterSeq != 0 {
		t.Fatalf("page 2 = %+v next %d, want the last checkpoint alone", b2.Bundle, b2.NextAfterSeq)
	}
	if rep := auditreceipt.VerifyBundle(b.Bundle, w.keys()); !rep.Complete() || rep.Checkpoints[0].Inclusion {
		t.Fatalf("export: failures %+v checkpoints %+v; want signed, inclusion not checked", rep.Failures, rep.Checkpoints)
	}

	rec = doSSO(t, srv, http.MethodGet, "/api/v1/audit/receipt-keys", sec, "")
	var keys auditreceipt.Keys
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &keys) != nil || keys.Format != auditreceipt.KeysFormat || len(keys.Keys) != 1 {
		t.Fatalf("keys = %d %s", rec.Code, rec.Body.String())
	}
	k := keys.Keys[0]
	if k.Kid != w.pub.Kid || k.Fingerprint != auditreceipt.Fingerprint(w.pub.PublicKey) || k.PublicKeyPEM == "" || k.Retired {
		t.Fatalf("key = %+v", k)
	}
}
