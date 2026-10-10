// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestStreamAuditPreservesPrincipalDelegationAndTrustedRunner(t *testing.T) {
	id := uuid.New()
	via := types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}
	ctx := audit.WithDelegation(t.Context(), via)
	for _, tc := range []struct{ ref, want string }{
		{"runner:" + id.String() + "/container", "runner:" + id.String()},
		{"container", ""}, {"runner:/container", ""}, {"runner:" + id.String() + "/", ""},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			rec := &sshTestRecorder{}
			s := &Server{cfg: Config{Audit: rec, Now: time.Now}}
			s.recordStreamAudit(ctx, tc.ref, types.AuditEvent{ActorType: types.ActorHuman, Actor: "alice", Action: "ssh.exec", Outcome: "success", Data: json.RawMessage(`{"exit":7}`)})
			rows := rec.snapshot()
			if len(rows) != 1 {
				t.Fatalf("rows=%v", rows)
			}
			ev := rows[0]
			var data struct {
				Relay string
				Via   types.DelegationVia
				Exit  int
			}
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatal(err)
			}
			if ev.Actor != "alice" || ev.ActorType != types.ActorHuman || ev.Outcome != "success" || data.Via != via || data.Exit != 7 || data.Relay != tc.want {
				t.Fatalf("event=%+v data=%s", ev, ev.Data)
			}
		})
	}
}

func TestSSHExecAuditNamesRunnerAfterClientClose(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	id := uuid.New()
	run.SandboxRef = "runner:" + id.String() + "/container"
	st.putRun(run)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) { return fakeExecSession("ok\n", "", 0), nil }})
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err = sess.Run("echo ok"); err != nil {
		t.Fatal(err)
	}
	_ = sess.Close()
	_ = client.Close()
	ev := waitForAudit(t, h.audit, run.ID, "ssh.exec", "success")
	if ev == nil {
		t.Fatal("missing completed SSH audit")
	}
	var data map[string]any
	if err = json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	if ev.Actor != principal || data["relay"] != "runner:"+id.String() || data["argv"] != "echo ok" {
		t.Fatalf("event=%+v data=%s", ev, ev.Data)
	}
}

func TestUIGatewayStreamAuditUsesFreshRunAndLeavesCookieUnchanged(t *testing.T) {
	h := newUIHarness(t, okBackend())
	h.gateway = panicFails(t, h.gateway)
	cookie := h.openSession()
	id := uuid.New()
	h.run.SandboxRef = "runner:" + id.String() + "/container"
	h.store.putRun(h.run)
	if got := h.relay("/ide", cookie, nil); got.Code != http.StatusOK {
		t.Fatalf("relay=%d %s", got.Code, got.Body.String())
	}
	rows := h.audit.auditRows("ui.open", "success")
	if len(rows) != 1 {
		t.Fatalf("rows=%v", rows)
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if rows[0].Actor != h.owner || data["relay"] != "runner:"+id.String() {
		t.Fatalf("row=%+v", rows[0])
	}
	// The annotation never comes from a long-lived session cookie.
	raw, err := json.Marshal(uiSession{SandboxRef: h.run.SandboxRef})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["SandboxRef"]; ok {
		t.Fatalf("cookie exposes transport state: %s", raw)
	}
}
