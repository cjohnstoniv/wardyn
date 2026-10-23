// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// hostDriveWriteData is a drive.write payload as driveWriteAuditData records it for a host_path drive.
func hostDriveWriteData() json.RawMessage {
	return mustJSON(driveWriteAuditData(types.UserDrive{
		Name: "Corp NAS", Backend: types.DriveBackendHostPath, HostRoot: "/srv/homes", StorageClass: "fast-nfs",
		HomeTemplate: types.HomeTemplateSub, SizeMiB: 512,
	}, driveRehome{}))
}

func hostAuditServer(t *testing.T, evs []types.AuditEvent) *Server {
	t.Helper()
	st := &pagerFake{recentAudit: evs}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.OIDC = &oidc.Authenticator{}
	return New(cfg)
}

func hostAuditRows(t *testing.T, srv *Server, path string, cookie *http.Cookie) []types.AuditEvent {
	t.Helper()
	w := doSSO(t, srv, http.MethodGet, path, cookie, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	var out []types.AuditEvent
	if path == "/api/v1/audit" {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return out
	}
	dec := json.NewDecoder(w.Body)
	for dec.More() {
		var ev types.AuditEvent
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		out = append(out, ev)
	}
	return out
}

func hostAuditFixture() []types.AuditEvent {
	return []types.AuditEvent{
		{ID: uuid.New(), Action: "drive.write", Outcome: "success", Data: hostDriveWriteData(), RowHash: "r2", PrevHash: "r1"},
		{ID: uuid.New(), Action: "run.start", Outcome: "success", Data: json.RawMessage(`{"host":"api.example.com"}`), RowHash: "r1"},
	}
}

// A security_admin reads a drive.write row without host_root or storage_class, the row names both in
// redacted_fields and keeps its hashes, and a row with no host detail passes byte for byte. Both feed reads.
func TestAuditHostDetail_SecurityAdminReadsDriveWriteWithoutHost(t *testing.T) {
	srv := hostAuditServer(t, hostAuditFixture())
	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	for _, path := range []string{"/api/v1/audit", "/api/v1/audit/export"} {
		rows := hostAuditRows(t, srv, path, sec)
		if len(rows) != 2 {
			t.Fatalf("%s: %d rows, want 2", path, len(rows))
		}
		var data map[string]any
		if err := json.Unmarshal(rows[0].Data, &data); err != nil {
			t.Fatalf("%s: drive.write data: %v", path, err)
		}
		for _, k := range []string{"host_root", "storage_class"} {
			if _, has := data[k]; has {
				t.Errorf("%s: security_admin read drive.write with %s = %v", path, k, data[k])
			}
		}
		got, _ := data[auditRedactedFieldsKey].([]any)
		if len(got) != 2 || got[0] != "host_root" || got[1] != "storage_class" {
			t.Errorf("%s: redacted_fields = %v, want [host_root storage_class]", path, data[auditRedactedFieldsKey])
		}
		if data["name"] != "Corp NAS" || data["backend"] != string(types.DriveBackendHostPath) {
			t.Errorf("%s: drive.write lost a field that names no host: %v", path, data)
		}
		if rows[0].RowHash != "r2" || rows[0].PrevHash != "r1" {
			t.Errorf("%s: redacted row hashes = %q/%q, want the stored r2/r1", path, rows[0].RowHash, rows[0].PrevHash)
		}
		if string(rows[1].Data) != `{"host":"api.example.com"}` {
			t.Errorf("%s: a row with no host detail changed: %s", path, rows[1].Data)
		}
	}
}

// A super admin reads the same row as recorded: both keys, no redacted_fields.
func TestAuditHostDetail_SuperAdminReadsTheHost(t *testing.T) {
	srv := hostAuditServer(t, hostAuditFixture())
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	for _, path := range []string{"/api/v1/audit", "/api/v1/audit/export"} {
		rows := hostAuditRows(t, srv, path, admin)
		var data map[string]any
		if err := json.Unmarshal(rows[0].Data, &data); err != nil {
			t.Fatalf("%s: drive.write data: %v", path, err)
		}
		if data["host_root"] != "/srv/homes" || data["storage_class"] != "fast-nfs" {
			t.Errorf("%s: super admin read host_root=%v storage_class=%v, want /srv/homes and fast-nfs", path, data["host_root"], data["storage_class"])
		}
		if _, has := data[auditRedactedFieldsKey]; has {
			t.Errorf("%s: super admin row carries redacted_fields %v", path, data[auditRedactedFieldsKey])
		}
	}
}

// A member reading their own run's resolved policy gets each mount's host path blanked.
func TestAuditHostDetail_MemberReadsPolicySnapshotWithoutMountSources(t *testing.T) {
	run := uuid.New()
	st := &auditScopeStore{runs: map[uuid.UUID]types.AgentRun{run: {ID: run, CreatedBy: auditMemberSub}}}
	st.auditByRun = map[uuid.UUID][]types.AuditEvent{run: {{ID: uuid.New(), RunID: &run, Action: "run.policy.resolve", Outcome: "success",
		Data: json.RawMessage(`{"workspace_mounts":[{"source":"/srv/nfs/team","target":"/workspace"}],"egress_allow":["github.com"]}`)}}}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)
	member := ssoSession(t, auditMemberSub, "member@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/audit?run_id="+run.String(), member, "")
	var rows []types.AuditEvent
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 1 {
		t.Fatalf("GET /audit?run_id=: %d %s (%v)", w.Code, w.Body.String(), err)
	}
	var data struct {
		Mounts   []types.WorkspaceMount `json:"workspace_mounts"`
		Redacted []string               `json:"redacted_fields"`
	}
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Mounts) != 1 || data.Mounts[0].Source != "<redacted>" || data.Mounts[0].Target != "/workspace" {
		t.Errorf("member read workspace_mounts %+v, want the source <redacted> and the target kept", data.Mounts)
	}
	if !slices.Equal(data.Redacted, []string{"workspace_mounts.source"}) {
		t.Errorf("redacted_fields = %v, want [workspace_mounts.source]", data.Redacted)
	}
}

// exportVerdict is what a verifier of a raw partition export reports: rows whose content re-hashes, rows
// served without their host detail (linked by their stored hashes, content not verifiable from this
// export), rows that fail, and whether the chain links and the digest folds.
type exportVerdict struct {
	verified, redacted, failed int
	linked, digestOK           bool
}

// toyRowHash stands in for audit_row_hash over the fake's rows: the previous hash and the data text.
func toyRowHash(prev, data string) string {
	h := sha256.Sum256([]byte(prev + "|" + data))
	return hex.EncodeToString(h[:])
}

func verifyRawExport(t *testing.T, lines []map[string]any, m store.PartitionManifest) exportVerdict {
	t.Helper()
	v := exportVerdict{linked: true}
	digest := store.NewPartitionDigest(m)
	prev := ""
	for _, l := range lines {
		switch l["type"] {
		case "row":
			row, _ := l["row_hash"].(string)
			p, _ := l["prev_hash"].(string)
			data, _ := l["data"].(string)
			digest.Add(row)
			if prev != "" && p != prev {
				v.linked = false
			}
			prev = row
			var d map[string]any
			_ = json.Unmarshal([]byte(data), &d)
			switch {
			case toyRowHash(p, data) == row:
				v.verified++
			case d[auditRedactedFieldsKey] != nil:
				v.redacted++
			default:
				v.failed++
			}
		case "footer":
			v.digestOK = l["digest"] == digest.Sum()
		}
	}
	return v
}

func hostPartition() *partitionExportFake {
	datas := []string{`{"a": 1}`, string(hostDriveWriteData()), `{"b": 2}`}
	f := &partitionExportFake{manifest: store.PartitionManifest{Partition: "audit_events_p202610", Rows: 3, SeqLo: 1, SeqHi: 3, RecordedLoUS: 1, RecordedHiUS: 3}}
	prev := ""
	for i, d := range datas {
		action := "run.start"
		if i == 1 {
			action = "drive.write"
		}
		h := toyRowHash(prev, d)
		row := store.AuditPartitionRow{Seq: int64(i + 1), RecordedUS: int64(i + 1), TimeUS: int64(i + 1), ID: uuid.New(), ActorType: "human",
			Actor: "admin@corp.example", Action: action, Outcome: "success", Data: &d, RowHash: &h, FoldHash: h}
		if prev != "" {
			p := prev
			row.PrevHash = &p
		}
		f.rows = append(f.rows, row)
		prev = h
	}
	return f
}

// The security tier's raw export verifies with its redacted row reported apart: 2 verified, 1 redacted,
// 0 failed, the chain linked and the digest folded. A super admin's export verifies every row in full.
func TestAuditHostDetail_SecurityPartitionExportVerifies(t *testing.T) {
	fake := hostPartition()
	srv := partitionServer(t, fake)
	for _, tc := range []struct {
		who          string
		cookie       *http.Cookie
		verified     int
		redacted     int
		footerCounts any
	}{
		{"security_admin", ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin), 2, 1, float64(1)},
		{"admin", ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin), 3, 0, nil},
	} {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/audit/export?partition=audit_events_p202610&form=raw", tc.cookie, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.who, w.Code, w.Body.String())
		}
		lines := ndjsonObjects(t, w.Body.Bytes())
		v := verifyRawExport(t, lines, fake.manifest)
		if v.verified != tc.verified || v.redacted != tc.redacted || v.failed != 0 || !v.linked || !v.digestOK {
			t.Errorf("%s: verdict %+v, want %d verified, %d redacted, 0 failed, linked, digest ok", tc.who, v, tc.verified, tc.redacted)
		}
		if got := lines[len(lines)-1]["redacted_rows"]; got != tc.footerCounts {
			t.Errorf("%s: footer redacted_rows = %v, want %v", tc.who, got, tc.footerCounts)
		}
	}
	// The readable form serves the same projection.
	w := doSSO(t, srv, http.MethodGet, "/api/v1/audit/export?partition=audit_events_p202610", ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin), "")
	lines := ndjsonObjects(t, w.Body.Bytes())
	data, _ := lines[2]["data"].(map[string]any)
	if _, has := data["host_root"]; has || data[auditRedactedFieldsKey] == nil {
		t.Errorf("readable security export drive.write data = %v, want host_root withheld and redacted_fields set", data)
	}
}
