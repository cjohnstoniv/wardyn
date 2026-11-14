// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auditreceipt"
)

// receiptFixture is a signed two-checkpoint receipt (rows 1-4 and 5-8, events at seq 2 and 6) and the keys
// file that pins its key, both written to dir.
type receiptFixture struct {
	dir, bundle, keys string
	b                 auditreceipt.Bundle
	pub               auditreceipt.PublicKey
	priv              ed25519.PrivateKey
	rows              [][]auditreceipt.Row
}

func strPtr(s string) *string { return &s }

func receiptChain(seq int64, n int, prev string, action string) []auditreceipt.Row {
	rows := make([]auditreceipt.Row, n)
	for i := range rows {
		r := auditreceipt.Row{Seq: seq + int64(i), ID: uuid.New(), TimeUS: 1700000000000000 + seq + int64(i), ActorType: "system", Actor: "t",
			Action: action, Target: strPtr("t"), Outcome: "success", SourceIP: strPtr(""), Data: strPtr(`{"n": 1}`)}
		if prev != "" {
			r.PrevHash = strPtr(prev)
		}
		r.RowHash = strPtr(auditreceipt.RowHash(r))
		prev = *r.RowHash
		rows[i] = r
	}
	return rows
}

func signCheckpoint(priv ed25519.PrivateKey, rows []auditreceipt.Row, prevStatement string) auditreceipt.Checkpoint {
	pub := priv.Public().(ed25519.PublicKey)
	prev := ""
	if prevStatement != "" {
		prev = auditreceipt.StatementHash(prevStatement)
	}
	seqs := make([]int64, len(rows))
	hashes := make([]string, len(rows))
	for i, r := range rows {
		seqs[i], hashes[i] = r.Seq, *r.RowHash
	}
	st := auditreceipt.Statement{Format: auditreceipt.CheckpointFormat, Kid: auditreceipt.KeyID(pub), SeqLo: seqs[0], SeqHi: seqs[len(seqs)-1],
		Rows: int64(len(rows)), HeadHash: hashes[len(hashes)-1], Digest: auditreceipt.Fold(prev, hashes),
		SeqDigest: auditreceipt.SeqDigest(seqs, hashes), Prev: prev, SignedAt: "2026-11-07T10:00:00Z"}
	text := string(st.Bytes())
	return auditreceipt.Checkpoint{Statement: text, Signature: ed25519.Sign(priv, []byte(text)), Seqs: seqs, RowHashes: hashes}
}

func newReceiptFixture(t *testing.T) receiptFixture {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	pubKey := priv.Public().(ed25519.PublicKey)
	pub := auditreceipt.PublicKey{Kid: auditreceipt.KeyID(pubKey), PublicKey: pubKey, Fingerprint: auditreceipt.Fingerprint(pubKey), CreatedAt: time.Unix(0, 0).UTC()}
	r1 := receiptChain(1, 4, "", "run.start")
	r2 := receiptChain(5, 4, *r1[3].RowHash, "run.start")
	c1 := signCheckpoint(priv, r1, "")
	c2 := signCheckpoint(priv, r2, c1.Statement)
	c1.Events = []auditreceipt.Event{{Index: 1, Row: r1[1]}}
	c2.Events = []auditreceipt.Event{{Index: 1, Row: r2[1]}}
	f := receiptFixture{dir: t.TempDir(), pub: pub, priv: priv, rows: [][]auditreceipt.Row{r1, r2},
		b: auditreceipt.Bundle{Format: auditreceipt.ReceiptFormat, Checkpoints: []auditreceipt.Checkpoint{c1, c2}}}
	f.bundle, f.keys = filepath.Join(f.dir, "r.json"), filepath.Join(f.dir, "keys.json")
	f.writeBundle(t, f.b)
	f.writeKeys(t, pub)
	return f
}

func (f receiptFixture) writeBundle(t *testing.T, b auditreceipt.Bundle) {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.bundle, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f receiptFixture) writeKeys(t *testing.T, pubs ...auditreceipt.PublicKey) {
	t.Helper()
	raw, err := json.Marshal(auditreceipt.Keys{Format: auditreceipt.KeysFormat, Keys: pubs})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.keys, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// runAudit runs the CLI and returns stdout, stderr and the process exit code main would use.
func runAudit(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	root := rootCmd()
	root.SetArgs(args)
	var out, errb strings.Builder
	root.SetOut(&out)
	root.SetErr(&errb)
	err := root.Execute()
	code := 0
	if err != nil {
		code = exitCodeFor(err)
	}
	return out.String(), errb.String(), code
}

// `wardyn audit verify` reads files and nothing else: no client is built, no request is made, even with a
// control plane configured and a token in hand.
func TestAuditVerify_BuildsNoClient(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	f := newReceiptFixture(t)
	out, _, code := runAudit(t, "audit", "verify", f.bundle, "--keys", f.keys, "--url", srv.URL, "--token", "tok")
	if code != 0 {
		t.Fatalf("verify exit %d, output %q", code, out)
	}
	if hits.Load() != 0 {
		t.Fatalf("verify made %d request(s) to the control plane, want 0", hits.Load())
	}
	cmd := auditVerifyCmd()
	if cmd.Use == "" || cmd.Flags().Lookup("keys") == nil {
		t.Fatalf("auditVerifyCmd takes no client and has a --keys flag")
	}
}

func TestAuditVerify_ExitCodes(t *testing.T) {
	f := newReceiptFixture(t)

	// 0: every receipted row verified.
	out, _, code := runAudit(t, "audit", "verify", f.bundle, "--keys", f.keys)
	if code != 0 || !strings.Contains(out, "2 event(s)") {
		t.Fatalf("a valid receipt: exit %d, output %q; want 0", code, out)
	}

	// 1: a failed check, with a one-line reason and the seq.
	tampered := f.b
	tampered.Checkpoints = append([]auditreceipt.Checkpoint(nil), f.b.Checkpoints...)
	ev := tampered.Checkpoints[1].Events[0]
	ev.Row.Action = "run.rewritten"
	tampered.Checkpoints[1].Events = []auditreceipt.Event{ev}
	f.writeBundle(t, tampered)
	out, errOut, code := runAudit(t, "audit", "verify", f.bundle, "--keys", f.keys)
	if code != 1 || !strings.Contains(out, "row_hash_mismatch") || !strings.Contains(out, "seq 6") {
		t.Fatalf("an edited row: exit %d, output %q, stderr %q; want 1 naming row_hash_mismatch and seq 6", code, out, errOut)
	}

	// 3: nothing failed, but an event's content was served without host detail and so was never checked.
	// The count is the first line, and the exit is never 0: OK alone is not the verdict.
	withheld := f.b
	withheld.Checkpoints = append([]auditreceipt.Checkpoint(nil), f.b.Checkpoints...)
	wr := receiptChain(5, 4, *f.rows[0][3].RowHash, "run.policy.resolve")
	data := `{"redacted_fields": ["workspace_mounts"]}`
	wr[1].Data, wr[1].PrevHash = &data, wr[0].RowHash
	wr[1].RowHash = strPtr(auditreceipt.RowHash(wr[1]))
	// Re-chain the rest so the signed list is consistent with the redacted row.
	for i := 2; i < 4; i++ {
		wr[i].PrevHash = wr[i-1].RowHash
		wr[i].RowHash = strPtr(auditreceipt.RowHash(wr[i]))
	}
	c1 := f.b.Checkpoints[0]
	c1.Events = nil
	c2 := signCheckpoint(f.priv, wr, c1.Statement)
	c2.Events = []auditreceipt.Event{{Index: 1, Row: wr[1], RedactedFields: []string{"workspace_mounts"}}}
	withheld.Checkpoints = []auditreceipt.Checkpoint{f.b.Checkpoints[0], c2}
	f.writeBundle(t, withheld)
	out, _, code = runAudit(t, "audit", "verify", f.bundle, "--keys", f.keys)
	first, _, _ := strings.Cut(out, "\n")
	if code != 3 || !strings.Contains(first, "1 event(s) withheld") {
		t.Fatalf("a withheld event: exit %d, first line %q; want 3 with the withheld count first", code, first)
	}

	// 2: usage. No keys, no bundle, a bundle that is not JSON, a keys file that is not a keys file.
	f.writeBundle(t, f.b)
	for name, args := range map[string][]string{
		"no keys flag":    {"audit", "verify", f.bundle},
		"no bundle":       {"audit", "verify", "--keys", f.keys},
		"missing bundle":  {"audit", "verify", filepath.Join(f.dir, "absent.json"), "--keys", f.keys},
		"missing keys":    {"audit", "verify", f.bundle, "--keys", filepath.Join(f.dir, "absent.json")},
		"bundle not json": {"audit", "verify", f.keys + ".txt", "--keys", f.keys},
	} {
		if name == "bundle not json" {
			if err := os.WriteFile(args[2], []byte("not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, code := runAudit(t, args...); code != 2 {
			t.Errorf("%s: exit %d, want 2", name, code)
		}
	}
	if err := os.WriteFile(f.keys, []byte(`{"format":"something-else","keys":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runAudit(t, "audit", "verify", f.bundle, "--keys", f.keys); code != 2 {
		t.Errorf("a keys file of another format: exit %d, want 2", code)
	}
}

// Keys come from the file the operator pinned and nowhere else: a bundle that carries a key of its own, or a
// bundle signed by a key the file does not list, is not trusted.
func TestAuditVerify_PinsKeysFromFileOnly(t *testing.T) {
	f := newReceiptFixture(t)
	attacker := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	aPub := attacker.Public().(ed25519.PublicKey)
	r := receiptChain(1, 3, "", "run.start")
	c := signCheckpoint(attacker, r, "")
	c.Events = []auditreceipt.Event{{Index: 0, Row: r[0]}}
	raw, err := json.Marshal(map[string]any{
		"format": auditreceipt.ReceiptFormat, "checkpoints": []auditreceipt.Checkpoint{c},
		// A key a forger ships beside the bundle, under every name a reader might look at.
		"keys": auditreceipt.Keys{Format: auditreceipt.KeysFormat, Keys: []auditreceipt.PublicKey{{Kid: auditreceipt.KeyID(aPub), PublicKey: aPub}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.bundle, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, code := runAudit(t, "audit", "verify", f.bundle, "--keys", f.keys)
	if code != 1 || !strings.Contains(out, "unknown_kid") {
		t.Fatalf("a bundle signed by a key the file does not pin: exit %d, output %q; want 1 unknown_kid", code, out)
	}

	// The control plane's key set is never consulted: no flag, environment variable or default path stands in
	// for --keys.
	t.Setenv("WARDYN_AUDIT_KEYS", f.keys)
	if _, _, code := runAudit(t, "audit", "verify", f.bundle); code != 2 {
		t.Fatalf("no --keys: exit %d, want 2 (the environment pins nothing)", code)
	}
}

// A checkpoint-only export is signatures and links. verify says so, and says inclusion was not checked.
func TestAuditVerify_CheckpointExportIsSignatureOnly(t *testing.T) {
	f := newReceiptFixture(t)
	export := auditreceipt.Bundle{Format: auditreceipt.CheckpointsFormat}
	for _, c := range f.b.Checkpoints {
		export.Checkpoints = append(export.Checkpoints, auditreceipt.Checkpoint{Statement: c.Statement, Signature: c.Signature})
	}
	f.writeBundle(t, export)
	out, _, code := runAudit(t, "audit", "verify", f.bundle, "--keys", f.keys)
	if code != 0 || !strings.Contains(out, "signature checked, inclusion not checked") || strings.Contains(out, "included") {
		t.Fatalf("export: exit %d, output %q; want 0 and \"signature checked, inclusion not checked\"", code, out)
	}
}

func TestAuditReceiptCommands(t *testing.T) {
	ev1, ev2, run := uuid.New(), uuid.New(), uuid.New()
	var queries []string
	var pending atomic.Int32
	pending.Store(2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/audit/receipt":
			if r.URL.Query().Get("run_id") == run.String() {
				_, _ = w.Write([]byte(`{"format":"wardyn-audit-receipt/2","next_after_seq":812,"checkpoints":[{"statement":"s","signature":"AAEC"}]}`))
				return
			}
			if pending.Add(-1) >= 0 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"not signed yet","reason":"audit_receipt_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"format":"wardyn-audit-receipt/2","checkpoints":[{"statement":"s","signature":"AAEC"}]}`))
		case "/api/v1/audit/checkpoints":
			_, _ = w.Write([]byte(`{"format":"wardyn-audit-checkpoints/1","checkpoints":[{"statement":"s","signature":"AAEC"}]}`))
		case "/api/v1/audit/receipt-keys":
			_, _ = w.Write([]byte(`{"format":"wardyn-audit-keys/1","keys":[{"kid":"k1","public_key":"AAEC","created_at":"2026-11-07T10:00:00Z","retired":false}]}`))
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	base := []string{"--url", srv.URL, "--token", "tok"}
	read := func(name string) auditreceipt.Bundle {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var b auditreceipt.Bundle
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return b
	}

	// Without --wait a pending event is a refusal that names the reason and the wait flag.
	if _, errOut, code := runAudit(t, append([]string{"audit", "receipt", ev1.String(), "-o", filepath.Join(dir, "no.json")}, base...)...); code == 0 {
		t.Fatalf("a pending receipt exited 0 (stderr %q)", errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "no.json")); err == nil {
		t.Fatalf("a refused receipt left a file behind")
	}

	// --wait retries a pending answer until it is signed, then writes the whole file.
	pending.Store(2)
	queries = nil
	if _, errOut, code := runAudit(t, append([]string{"audit", "receipt", ev1.String(), ev2.String(), "--wait", "-o", filepath.Join(dir, "r.json")}, base...)...); code != 0 {
		t.Fatalf("receipt --wait exit %d, stderr %q", code, errOut)
	}
	if len(queries) != 3 || !strings.Contains(queries[2], "event="+ev1.String()) || !strings.Contains(queries[2], "event="+ev2.String()) {
		t.Fatalf("queries = %v, want 3 requests naming both events", queries)
	}
	if b := read("r.json"); b.Format != auditreceipt.ReceiptFormat || len(b.Checkpoints) != 1 {
		t.Fatalf("receipt file = %+v", b)
	}

	// A run's receipt pages: the file is written, and stderr names the seq to continue from.
	queries = nil
	_, errOut, code := runAudit(t, append([]string{"audit", "receipt", "--run", run.String(), "--limit", "50", "-o", filepath.Join(dir, "run.json")}, base...)...)
	if code != 0 || !strings.Contains(errOut, "--after-seq 812") {
		t.Fatalf("run receipt: exit %d, stderr %q; want 0 naming --after-seq 812", code, errOut)
	}
	if len(queries) != 1 || !strings.Contains(queries[0], "run_id="+run.String()) || !strings.Contains(queries[0], "limit=50") {
		t.Fatalf("queries = %v", queries)
	}

	// Event ids and --run are exclusive; one of them is required.
	if _, _, code := runAudit(t, append([]string{"audit", "receipt", ev1.String(), "--run", run.String()}, base...)...); code != 2 {
		t.Errorf("event and --run together: exit %d, want 2", code)
	}
	if _, _, code := runAudit(t, append([]string{"audit", "receipt"}, base...)...); code != 2 {
		t.Errorf("neither event nor --run: exit %d, want 2", code)
	}
	if _, _, code := runAudit(t, append([]string{"audit", "receipt", "not-a-uuid"}, base...)...); code != 2 {
		t.Errorf("a bad event id: exit %d, want 2", code)
	}

	// checkpoints and keys write the files the verifier reads.
	queries = nil
	if _, _, code := runAudit(t, append([]string{"audit", "checkpoints", "--after-seq", "40", "-o", filepath.Join(dir, "c.json")}, base...)...); code != 0 {
		t.Fatalf("checkpoints exit %d", code)
	}
	if len(queries) != 1 || !strings.Contains(queries[0], "after_seq=40") {
		t.Fatalf("queries = %v", queries)
	}
	if b := read("c.json"); b.Format != auditreceipt.CheckpointsFormat {
		t.Fatalf("checkpoints file format = %q", b.Format)
	}
	if _, _, code := runAudit(t, append([]string{"audit", "keys", "-o", filepath.Join(dir, "k.json")}, base...)...); code != 0 {
		t.Fatalf("keys exit %d", code)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "k.json"))
	var keys auditreceipt.Keys
	if err != nil || json.Unmarshal(raw, &keys) != nil || keys.Format != auditreceipt.KeysFormat || len(keys.Keys) != 1 || keys.Keys[0].Kid != "k1" {
		t.Fatalf("keys file = %q, %v", raw, err)
	}
}

// A bundle is a file anyone can hand over: its format, actions and failure details are shown escaped, so
// a line break cannot print a forged "verified" line and an escape sequence cannot repaint the verdict.
func TestPrintVerifyReportEscapesTheBundlesText(t *testing.T) {
	var buf bytes.Buffer
	printVerifyReport(&buf, auditreceipt.Report{
		Format:      "x\x1b[32m\nVERIFIED",
		Checkpoints: []auditreceipt.CheckpointResult{{Kid: "k\x1b[0m"}},
		Events:      []auditreceipt.EventResult{{Seq: 2, Action: "run.create: verified\nevent seq 3"}},
		Policies:    []auditreceipt.PolicyResult{{Seq: 2, Action: "a\rb", Status: "odd\x07"}},
		Failures:    []auditreceipt.Failure{{Checkpoint: -1, Reason: auditreceipt.ReasonFormat, Detail: "d\nVERIFIED"}},
	})
	got := buf.String()
	if strings.ContainsAny(got, "\x1b\r\x07") || strings.Count(got, "\n") != 6 || strings.Contains(got, "\nVERIFIED") || strings.Contains(got, "\nevent seq 3") {
		t.Fatalf("report = %q, want every field escaped onto its own line", got)
	}
}
